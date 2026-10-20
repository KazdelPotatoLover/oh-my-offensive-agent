package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"golang.org/x/net/html"
)

type ExtractEndpointLinksTool struct {
	runID      uuid.UUID
	workItemID uuid.UUID
	sessionID  uuid.UUID
	runService *service.RunService
}

func NewExtractEndpointLinksTool(runID, workItemID, sessionID uuid.UUID, runService *service.RunService) *ExtractEndpointLinksTool {
	return &ExtractEndpointLinksTool{
		runID:      runID,
		workItemID: workItemID,
		sessionID:  sessionID,
		runService: runService,
	}
}

func (t *ExtractEndpointLinksTool) Name() string {
	return "extract_endpoint_links"
}

func (t *ExtractEndpointLinksTool) Description() string {
	return "Extract and register verified HTTP endpoint links from an existing successful get_endpoint tool call HTML response."
}

func (t *ExtractEndpointLinksTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"source_tool_call_id": {
				"type": "string",
				"description": "Copy _execution.source_tool_call_id from a successful get_endpoint result, not asset_id. The exact original call_id is also accepted. Parses the stored HTML evidence, including when the model preview was shortened."
			}
		},
		"required": ["source_tool_call_id"]
	}`)
}

func (t *ExtractEndpointLinksTool) Validate(argsBytes []byte) error {
	var args struct {
		SourceToolCallID string `json:"source_tool_call_id"`
	}
	if err := json.Unmarshal(argsBytes, &args); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if strings.TrimSpace(args.SourceToolCallID) == "" {
		return errors.New("source_tool_call_id is required")
	}
	return nil
}

type DiscoveredItem struct {
	AssetID uuid.UUID `json:"asset_id"`
	URL     string    `json:"url"`
	Path    string    `json:"path"`
	RawHref string    `json:"raw_href"`
}

type ExistingItem struct {
	AssetID uuid.UUID `json:"asset_id"`
	URL     string    `json:"url"`
	Path    string    `json:"path"`
	RawHref string    `json:"raw_href"`
}

type RejectedItem struct {
	RawHref string `json:"raw_href"`
	Reason  string `json:"reason"`
}

func (t *ExtractEndpointLinksTool) Execute(ctx context.Context, argsBytes []byte) (string, error) {
	var args struct {
		SourceToolCallID string `json:"source_tool_call_id"`
	}
	if err := json.Unmarshal(argsBytes, &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}

	pool := t.runService.Pool()

	// 1. Retrieve and validate source tool call
	var tcID uuid.UUID
	var sessionID *uuid.UUID
	var toolName, tcStatus, output string
	var argsJSON []byte

	err := pool.QueryRow(ctx,
		`SELECT id, session_id, tool_name, status, output, arguments
		 FROM agent_tool_calls
		 WHERE run_id = $1 AND (id::text = $2 OR call_id = $2)`,
		t.runID, strings.TrimSpace(args.SourceToolCallID)).Scan(
		&tcID, &sessionID, &toolName, &tcStatus, &output, &argsJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("source tool call '%s' not found in run %s", args.SourceToolCallID, t.runID)
		}
		return "", fmt.Errorf("query source tool call: %w", err)
	}

	// 2. Validate session association: must belong to current session
	if sessionID == nil || *sessionID != t.sessionID {
		return "", fmt.Errorf("source tool call belongs to a different session; cross-session discovery rejected")
	}
	if toolName != "get_endpoint" {
		return "", fmt.Errorf("source tool call is '%s', but extract_endpoint_links requires 'get_endpoint'", toolName)
	}
	if tcStatus != "success" {
		return "", fmt.Errorf("source tool call status is '%s', must be 'success'", tcStatus)
	}

	// 3. Parse structured tool call evidence
	var tcData struct {
		Request struct {
			Method  string `json:"method"`
			URL     string `json:"url"`
			AssetID string `json:"asset_id"`
		} `json:"request"`
		Response *struct {
			StatusCode int               `json:"status_code"`
			Headers    map[string]string `json:"headers"`
			Body       string            `json:"body"`
		} `json:"response"`
		Integrity struct {
			SavedBytes  int    `json:"saved_bytes"`
			IsTruncated bool   `json:"is_truncated"`
			Note        string `json:"note"`
		} `json:"integrity"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(output), &tcData); err != nil {
		return "", fmt.Errorf("source tool call output is not valid structured evidence: %w", err)
	}
	if tcData.Error != nil {
		return "", fmt.Errorf("source tool call recorded error (%s: %s)", tcData.Error.Type, tcData.Error.Message)
	}
	if tcData.Response == nil {
		return "", errors.New("source tool call output contains no response")
	}
	if tcData.Integrity.IsTruncated {
		return "", errors.New("cannot extract links from truncated response body")
	}

	// 4. Verify Content-Type is text/html
	var isHTML bool
	for k, v := range tcData.Response.Headers {
		if strings.EqualFold(k, "Content-Type") && strings.Contains(strings.ToLower(v), "text/html") {
			isHTML = true
			break
		}
	}
	if !isHTML {
		return "", errors.New("source tool call response is not text/html")
	}

	// 5. Verify source asset belongs to current work item
	sourceAssetUUID, err := uuid.Parse(tcData.Request.AssetID)
	if err != nil {
		return "", fmt.Errorf("invalid source asset ID: %w", err)
	}
	if t.workItemID != uuid.Nil {
		var inWIScope bool
		err = pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM work_item_assets WHERE work_item_id = $1 AND asset_id = $2)`,
			t.workItemID, sourceAssetUUID).Scan(&inWIScope)
		if err != nil || !inWIScope {
			return "", fmt.Errorf("source asset %s is not associated with work item: %w", sourceAssetUUID, service.ErrAssetOutOfScope)
		}
	}

	// 6. Check Run state and retrieve discovery policy
	var runStatus model.RunStatus
	var executionInputJSON []byte
	err = pool.QueryRow(ctx,
		`SELECT status, execution_input FROM runs WHERE id = $1`, t.runID).Scan(&runStatus, &executionInputJSON)
	if err != nil {
		return "", fmt.Errorf("get run status: %w", err)
	}
	if runStatus.IsTerminal() || runStatus == model.RunStatusStopping {
		return "", fmt.Errorf("run is in status '%s': discovery rejected", runStatus)
	}

	// Resolve discovery policy from execution_input
	var execInput struct {
		DiscoveryPolicy *model.DiscoveryPolicy `json:"discovery_policy"`
		Scope           map[string]interface{} `json:"scope"`
	}
	_ = json.Unmarshal(executionInputJSON, &execInput)
	discPolicy := execInput.DiscoveryPolicy
	if discPolicy == nil && execInput.Scope != nil {
		if dpRaw, ok := execInput.Scope["discovery_policy"]; ok {
			dpBytes, _ := json.Marshal(dpRaw)
			var dp model.DiscoveryPolicy
			if err := json.Unmarshal(dpBytes, &dp); err == nil && dp.Origin != "" {
				discPolicy = &dp
			}
		}
	}
	if discPolicy == nil {
		fixturePort := os.Getenv("FIXTURE_PORT")
		if fixturePort == "" {
			fixturePort = "29090"
		}
		discPolicy = &model.DiscoveryPolicy{
			Mode:           "exact_paths",
			Origin:         fmt.Sprintf("http://127.0.0.1:%s", fixturePort),
			AllowedMethods: []string{"GET"},
			AllowedPaths:   []string{"/", "/login", "/health"},
		}
	}
	if discPolicy.Mode == "" {
		if len(discPolicy.AllowedPaths) > 0 {
			discPolicy.Mode = "exact_paths"
		} else {
			discPolicy.Mode = "same_origin_html"
		}
	}

	policyURL, err := url.Parse(discPolicy.Origin)
	if err != nil {
		return "", fmt.Errorf("invalid discovery policy origin '%s': %w", discPolicy.Origin, err)
	}

	allowedPathMap := make(map[string]bool)
	for _, p := range discPolicy.AllowedPaths {
		allowedPathMap[p] = true
	}

	// 7. Parse HTML for <a href>
	doc, err := html.Parse(strings.NewReader(tcData.Response.Body))
	if err != nil {
		return "", fmt.Errorf("parse response html: %w", err)
	}

	var rawHrefs []string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if len(rawHrefs) >= 100 {
			return // upper bound limit: max 100 hrefs
		}
		if n.Type == html.ElementNode && strings.EqualFold(n.Data, "a") {
			for _, attr := range n.Attr {
				if strings.EqualFold(attr.Key, "href") {
					rawHrefs = append(rawHrefs, attr.Val)
					break
				}
			}
		}
		for c := n.FirstChild; c != nil; c = c.NextSibling {
			walk(c)
		}
	}
	walk(doc)

	sourceReqURL, err := url.Parse(tcData.Request.URL)
	if err != nil {
		return "", fmt.Errorf("invalid source request URL: %w", err)
	}

	var discovered []DiscoveredItem
	var existing []ExistingItem
	var rejected []RejectedItem

	type candidateSpec struct {
		rawHref string
		path    string
		fullURL string
	}
	var candidates []candidateSpec

	for _, href := range rawHrefs {
		// Upper bound: single href max 2 KiB
		if len(href) > 2048 {
			rejected = append(rejected, RejectedItem{
				RawHref: href[:64] + "...",
				Reason:  "href exceeds 2048 bytes limit",
			})
			continue
		}

		trimmedHref := strings.TrimSpace(href)
		if trimmedHref == "" || strings.HasPrefix(trimmedHref, "#") {
			// Pure fragment or empty link skipped
			continue
		}

		lowerHref := strings.ToLower(trimmedHref)
		if strings.HasPrefix(lowerHref, "javascript:") {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  "unsupported scheme 'javascript'",
			})
			continue
		}
		if strings.HasPrefix(lowerHref, "mailto:") {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  "unsupported scheme 'mailto'",
			})
			continue
		}

		parsedHref, err := url.Parse(trimmedHref)
		if err != nil {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  fmt.Sprintf("malformed href: %v", err),
			})
			continue
		}

		resolved := sourceReqURL.ResolveReference(parsedHref)

		if resolved.Scheme != "http" {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  fmt.Sprintf("unsupported scheme '%s'", resolved.Scheme),
			})
			continue
		}

		if resolved.RawQuery != "" {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  "queries not permitted in discovery policy",
			})
			continue
		}

		if resolved.User != nil {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  "user credentials not permitted",
			})
			continue
		}

		if resolved.Host != policyURL.Host {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  fmt.Sprintf("origin '%s' does not match policy origin '%s'", resolved.Host, policyURL.Host),
			})
			continue
		}

		lowerPath := strings.ToLower(resolved.Path)
		if strings.HasSuffix(lowerPath, ".pdf") || strings.HasSuffix(lowerPath, ".zip") ||
			strings.HasSuffix(lowerPath, ".tar.gz") || strings.HasSuffix(lowerPath, ".tar") ||
			strings.HasSuffix(lowerPath, ".exe") || strings.HasSuffix(lowerPath, ".bin") ||
			strings.HasSuffix(lowerPath, ".iso") || strings.HasSuffix(lowerPath, ".png") ||
			strings.HasSuffix(lowerPath, ".jpg") || strings.HasSuffix(lowerPath, ".jpeg") ||
			strings.HasSuffix(lowerPath, ".gif") || strings.HasSuffix(lowerPath, ".svg") ||
			strings.HasSuffix(lowerPath, ".css") || strings.HasSuffix(lowerPath, ".js") {
			rejected = append(rejected, RejectedItem{
				RawHref: trimmedHref,
				Reason:  "file download candidate skipped (non-HTML file)",
			})
			continue
		}

		if discPolicy.Mode == "same_origin_html" {
			// Permitted on this origin
		} else {
			if !allowedPathMap[resolved.Path] {
				rejected = append(rejected, RejectedItem{
					RawHref: trimmedHref,
					Reason:  fmt.Sprintf("path '%s' is outside discovery policy allowed_paths", resolved.Path),
				})
				continue
			}
		}

		candidateFullURL := discPolicy.Origin + resolved.Path
		candidates = append(candidates, candidateSpec{
			rawHref: trimmedHref,
			path:    resolved.Path,
			fullURL: candidateFullURL,
		})
	}

	// 8. Atomically register discoveries in a short transaction
	tx, err := pool.Begin(ctx)
	if err != nil {
		return "", fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Acquire row lock on runs to enforce consistency with stopping
	var currentRunStatus model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, t.runID).Scan(&currentRunStatus)
	if err != nil {
		return "", fmt.Errorf("lock run for discovery: %w", err)
	}
	if currentRunStatus.IsTerminal() || currentRunStatus == model.RunStatusStopping {
		return "", fmt.Errorf("run transitioned to status '%s': discovery registration aborted", currentRunStatus)
	}

	// Get current attempt ID for session
	var attemptID *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT attempt_id FROM agent_sessions WHERE id = $1`, t.sessionID).Scan(&attemptID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return "", fmt.Errorf("get session attempt_id: %w", err)
	}

	// Calculate shortest discovery depth of source asset from seeds (BFS with cycle detection)
	maxDepth := discPolicy.MaxDepth
	if maxDepth <= 0 {
		maxDepth = 2
	}
	var sourceDepth *int
	err = tx.QueryRow(ctx, `
 WITH RECURSIVE reachable(asset_id, depth) AS (
   SELECT asset_id, 0 FROM run_assets WHERE run_id=$1 AND is_seed
   UNION
   SELECT ad.target_asset_id, r.depth+1
   FROM reachable r JOIN asset_discoveries ad ON ad.source_asset_id=r.asset_id
   WHERE ad.run_id=$1 AND r.depth < $3
 )
 SELECT MIN(depth) FROM reachable WHERE asset_id=$2
 `, t.runID, sourceAssetUUID, maxDepth).Scan(&sourceDepth)
	if err != nil {
		return "", fmt.Errorf("calculate discovery depth for source asset: %w", err)
	}
	if sourceDepth == nil {
		return "", fmt.Errorf("source asset is not reachable from a seed within depth limit")
	}

	candidateDepth := *sourceDepth + 1

	for _, c := range candidates {
		targetKey := "GET " + c.fullURL

		// Check if already in run_assets
		var existingAssetID uuid.UUID
		err = tx.QueryRow(ctx,
			`SELECT a.id FROM run_assets ra
			 JOIN assets a ON ra.asset_id = a.id
			 WHERE ra.run_id = $1 AND a.external_key = $2`,
			t.runID, targetKey).Scan(&existingAssetID)

		if err == nil {
			// Already in run_assets
			existing = append(existing, ExistingItem{
				AssetID: existingAssetID,
				URL:     c.fullURL,
				Path:    c.path,
				RawHref: c.rawHref,
			})

			// Record discovery relationship if not yet present (skip self-loops where source == target)
			if existingAssetID != sourceAssetUUID {
				_, err = tx.Exec(ctx,
					`INSERT INTO asset_discoveries (id, run_id, source_asset_id, source_tool_call_id, target_asset_id, raw_href)
					 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
					 ON CONFLICT (run_id, source_asset_id, target_asset_id, raw_href) DO NOTHING`,
					t.runID, sourceAssetUUID, tcID, existingAssetID, c.rawHref)
				if err != nil {
					return "", fmt.Errorf("insert existing asset discovery edge: %w", err)
				}
			}
			continue
		}

		if !errors.Is(err, pgx.ErrNoRows) {
			return "", fmt.Errorf("check run asset: %w", err)
		}

		// Check discovery depth limit (default 2)
		if candidateDepth > maxDepth {
			rejected = append(rejected, RejectedItem{
				RawHref: c.rawHref,
				Reason:  fmt.Sprintf("discovery depth limit exceeded (depth %d > max %d)", candidateDepth, maxDepth),
			})
			continue
		}

		// Check maximum pages limit (default 20)
		maxPages := discPolicy.MaxPages
		if maxPages <= 0 {
			maxPages = 20
		}
		var currentAssetCount int
		err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM run_assets WHERE run_id = $1`, t.runID).Scan(&currentAssetCount)
		if err != nil {
			return "", fmt.Errorf("count run assets: %w", err)
		}
		if currentAssetCount >= maxPages {
			rejected = append(rejected, RejectedItem{
				RawHref: c.rawHref,
				Reason:  fmt.Sprintf("run reached maximum pages limit (%d)", maxPages),
			})
			continue
		}

		// Newly discovered asset for this Run!
		// Upsert into global assets table
		ns := "target-range"
		if strings.Contains(discPolicy.Origin, "127.0.0.1") {
			ns = "local-fixture-v1"
		}
		assetName := fmt.Sprintf("GET %s", c.path)
		desc := fmt.Sprintf("HTTP endpoint %s of %s", c.path, discPolicy.Origin)
		if ns == "local-fixture-v1" {
			if c.path == "/" {
				assetName = "Local Fixture Home"
			} else if c.path == "/login" {
				assetName = "Local Fixture Login"
			} else if c.path == "/health" {
				assetName = "Local Fixture Health Check"
			}
			desc = fmt.Sprintf("HTTP endpoint %s of local fixture application", c.path)
		}

		metaJSON, _ := json.Marshal(map[string]interface{}{
			"url":    c.fullURL,
			"path":   c.path,
			"port":   policyURL.Port(),
			"method": "GET",
		})

		var targetAssetID uuid.UUID
		err = tx.QueryRow(ctx,
			`INSERT INTO assets (id, namespace, kind, external_key, name, description, metadata, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, 'http_endpoint', $2, $3, $4, $5, now(), now())
			 ON CONFLICT (namespace, kind, external_key) DO UPDATE
			 SET updated_at = now()
			 RETURNING id`,
			ns, targetKey, assetName, desc, metaJSON).Scan(&targetAssetID)
		if err != nil {
			return "", fmt.Errorf("upsert asset %s: %w", targetKey, err)
		}

		// Associate with run_assets
		snapJSON, _ := json.Marshal(map[string]interface{}{
			"kind":         "http_endpoint",
			"external_key": targetKey,
			"is_seed":      false,
			"metadata": map[string]interface{}{
				"url":    c.fullURL,
				"method": "GET",
			},
		})
		_, err = tx.Exec(ctx,
			`INSERT INTO run_assets (run_id, asset_id, snapshot, depth)
			 VALUES ($1, $2, $3, $4)
			 ON CONFLICT (run_id, asset_id) DO NOTHING`,
			t.runID, targetAssetID, snapJSON, candidateDepth)
		if err != nil {
			return "", fmt.Errorf("insert run_asset: %w", err)
		}

		// Insert asset_discoveries (skip self-loops)
		if targetAssetID != sourceAssetUUID {
			_, err = tx.Exec(ctx,
				`INSERT INTO asset_discoveries (id, run_id, source_asset_id, source_tool_call_id, target_asset_id, raw_href)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5)
				 ON CONFLICT (run_id, source_asset_id, target_asset_id, raw_href) DO NOTHING`,
				t.runID, sourceAssetUUID, tcID, targetAssetID, c.rawHref)
			if err != nil {
				return "", fmt.Errorf("insert asset discovery: %w", err)
			}
		}

		// Record observation on SOURCE asset
		obsContent := fmt.Sprintf("Discovered endpoint %s (%s) from link %s", c.path, c.fullURL, c.rawHref)
		obsDataJSON, _ := json.Marshal(map[string]interface{}{
			"source_asset_id": sourceAssetUUID,
			"target_asset_id": targetAssetID,
			"raw_href":        c.rawHref,
			"path":            c.path,
			"url":             c.fullURL,
		})

		var newObsID uuid.UUID
		err = tx.QueryRow(ctx,
			`INSERT INTO observations (id, run_id, attempt_id, asset_id, obs_type, content, data, source_tool_call_id, created_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, 'discovered_links', $4, $5, $6, now())
			 RETURNING id`,
			t.runID, attemptID, sourceAssetUUID, obsContent, obsDataJSON, tcID).Scan(&newObsID)
		if err != nil {
			return "", fmt.Errorf("insert observation: %w", err)
		}

		// Insert run_event
		var nextSeq int
		err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, t.runID).Scan(&nextSeq)
		if err != nil {
			return "", fmt.Errorf("get next run_events seq: %w", err)
		}
		eventPayload, _ := json.Marshal(map[string]interface{}{
			"observation_id": newObsID,
			"asset_id":       sourceAssetUUID,
			"target_asset":   targetAssetID,
			"obs_type":       "discovered_links",
			"content":        obsContent,
		})
		_, err = tx.Exec(ctx,
			`INSERT INTO run_events (id, run_id, seq, type, payload)
			 VALUES (gen_random_uuid(), $1, $2, 'observation', $3)`,
			t.runID, nextSeq, eventPayload)
		if err != nil {
			return "", fmt.Errorf("insert run event: %w", err)
		}

		discovered = append(discovered, DiscoveredItem{
			AssetID: targetAssetID,
			URL:     c.fullURL,
			Path:    c.path,
			RawHref: c.rawHref,
		})
	}

	// Refresh shortest distances, including existing assets reached via a shorter
	// route. UNION deduplicates (asset,depth); the bound prevents cyclic expansion.
	if _, err = tx.Exec(ctx, `
 WITH RECURSIVE reachable(asset_id, depth) AS (
   SELECT asset_id, 0 FROM run_assets WHERE run_id=$1 AND is_seed
   UNION
   SELECT ad.target_asset_id, r.depth+1
   FROM reachable r JOIN asset_discoveries ad ON ad.source_asset_id=r.asset_id
   WHERE ad.run_id=$1 AND r.depth < $2
 ), distances AS (SELECT asset_id, MIN(depth) AS depth FROM reachable GROUP BY asset_id)
 UPDATE run_assets ra SET depth=d.depth FROM distances d
 WHERE ra.run_id=$1 AND ra.asset_id=d.asset_id
 `, t.runID, maxDepth); err != nil {
		return "", fmt.Errorf("refresh discovery depths: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return "", fmt.Errorf("commit discovery transaction: %w", err)
	}

	resMap := map[string]interface{}{
		"source_asset_id": sourceAssetUUID.String(),
		"discovered":      discovered,
		"existing":        existing,
		"rejected":        rejected,
	}
	resJSON, _ := json.Marshal(resMap)
	return string(resJSON), nil
}
