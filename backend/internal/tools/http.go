package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

// GetEndpointTool retrieves the content of an HTTP endpoint asset.
type GetEndpointTool struct {
	runID      uuid.UUID
	workItemID uuid.UUID
	runService *service.RunService
}

// NewGetEndpointTool creates a new GetEndpointTool for a specific Run and WorkItem.
func NewGetEndpointTool(runID, workItemID uuid.UUID, runService *service.RunService) *GetEndpointTool {
	return &GetEndpointTool{
		runID:      runID,
		workItemID: workItemID,
		runService: runService,
	}
}

func (t *GetEndpointTool) Name() string {
	return "get_endpoint"
}

func (t *GetEndpointTool) Description() string {
	return "Fetch an HTTP endpoint target by asset_id. The target URL is strictly resolved from the immutable RunAsset snapshot."
}

func (t *GetEndpointTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"asset_id": {
				"type": "string",
				"description": "The UUID of the target HTTP endpoint asset"
			}
		},
		"required": ["asset_id"]
	}`)
}

func (t *GetEndpointTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	if strings.TrimSpace(payload.AssetID) == "" {
		return errors.New("asset_id is required")
	}
	if _, err := uuid.Parse(payload.AssetID); err != nil {
		return fmt.Errorf("invalid asset_id: must be a valid UUID: %w", err)
	}
	return nil
}

func (t *GetEndpointTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		AssetID string `json:"asset_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %w", err)
	}

	assetID, err := uuid.Parse(payload.AssetID)
	if err != nil {
		return "", fmt.Errorf("invalid asset_id: must be a valid UUID: %w", err)
	}

	// 1. Verify asset belongs to RunAsset scope
	pool := t.runService.Pool()
	var inRunScope bool
	err = pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM run_assets WHERE run_id = $1 AND asset_id = $2)`,
		t.runID, assetID).Scan(&inRunScope)
	if err != nil {
		return "", fmt.Errorf("query run asset scope: %w", err)
	}
	if !inRunScope {
		return "", fmt.Errorf("asset %s is outside run scope: %w", assetID, service.ErrAssetOutOfScope)
	}

	// 2. Verify asset belongs to WorkItemAsset scope (if workItemID is set)
	if t.workItemID != uuid.Nil {
		var inWIScope bool
		err = pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM work_item_assets WHERE work_item_id = $1 AND asset_id = $2)`,
			t.workItemID, assetID).Scan(&inWIScope)
		if err != nil {
			return "", fmt.Errorf("query work item asset scope: %w", err)
		}
		if !inWIScope {
			return "", fmt.Errorf("asset %s is not associated with work item: %w", assetID, service.ErrAssetOutOfScope)
		}
	}

	// 3. Retrieve immutable snapshot and asset identity
	var kind, externalKey, assetName string
	var snapshotJSON []byte
	err = pool.QueryRow(ctx,
		`SELECT a.kind, a.external_key, a.name, ra.snapshot
		 FROM run_assets ra
		 JOIN assets a ON ra.asset_id = a.id
		 WHERE ra.run_id = $1 AND ra.asset_id = $2`,
		t.runID, assetID).Scan(&kind, &externalKey, &assetName, &snapshotJSON)
	if err != nil {
		return "", fmt.Errorf("retrieve run asset details: %w", err)
	}

	if kind != "http_endpoint" {
		return "", fmt.Errorf("asset %s has kind '%s', but get_endpoint requires 'http_endpoint'", assetID, kind)
	}

	// 4. Resolve URL from snapshot
	var targetURLStr string
	var snapMap map[string]interface{}
	if err := json.Unmarshal(snapshotJSON, &snapMap); err == nil {
		if meta, ok := snapMap["metadata"].(map[string]interface{}); ok {
			if u, ok := meta["url"].(string); ok && u != "" {
				targetURLStr = u
			}
		}
	}
	if targetURLStr == "" {
		// Fallback parse from externalKey e.g. "GET http://127.0.0.1:29090/..."
		parts := strings.SplitN(externalKey, " ", 2)
		if len(parts) == 2 {
			targetURLStr = parts[1]
		} else {
			targetURLStr = externalKey
		}
	}

	// 5. Strict URL validation
	parsedURL, err := url.Parse(targetURLStr)
	if err != nil {
		return "", fmt.Errorf("invalid endpoint URL: %w", err)
	}
	if parsedURL.Scheme != "http" {
		return "", errors.New("only http scheme is permitted")
	}
	if parsedURL.RawQuery != "" || parsedURL.Fragment != "" || parsedURL.User != nil {
		return "", errors.New("query, fragment, and user credentials are not permitted")
	}

	// Retrieve discovery policy if present in this Run
	var execInputJSON []byte
	_ = pool.QueryRow(ctx, `SELECT execution_input FROM runs WHERE id = $1`, t.runID).Scan(&execInputJSON)
	var execInput struct {
		DiscoveryPolicy *model.DiscoveryPolicy `json:"discovery_policy"`
		Scope           map[string]interface{} `json:"scope"`
	}
	_ = json.Unmarshal(execInputJSON, &execInput)
	dp := execInput.DiscoveryPolicy
	if dp == nil && execInput.Scope != nil {
		if dpRaw, ok := execInput.Scope["discovery_policy"]; ok {
			dpBytes, _ := json.Marshal(dpRaw)
			var parsedDP model.DiscoveryPolicy
			if err := json.Unmarshal(dpBytes, &parsedDP); err == nil && parsedDP.Origin != "" {
				dp = &parsedDP
			}
		}
	}

	if dp != nil && dp.Origin != "" {
		policyURL, err := url.Parse(dp.Origin)
		if err != nil {
			return "", fmt.Errorf("invalid discovery policy origin: %w", err)
		}
		if parsedURL.Host != policyURL.Host {
			return "", fmt.Errorf("host %s does not match discovery policy origin %s", parsedURL.Host, policyURL.Host)
		}
		if dp.Mode == "same_origin_html" {
			// Permitted on this origin
		} else {
			allowedPathMap := make(map[string]bool)
			for _, p := range dp.AllowedPaths {
				allowedPathMap[p] = true
			}
			if !allowedPathMap[parsedURL.Path] {
				return "", fmt.Errorf("path '%s' is outside discovery policy allowed_paths", parsedURL.Path)
			}
		}

		// Enforce request interval
		intervalMs := dp.RequestIntervalMs
		if intervalMs <= 0 {
			intervalMs = 500
		}
		time.Sleep(time.Duration(intervalMs) * time.Millisecond)
	} else {
		// Non-discovery run: enforce legacy fixture constraints
		host, port, err := net.SplitHostPort(parsedURL.Host)
		if err != nil {
			host = parsedURL.Hostname()
			port = parsedURL.Port()
		}
		if host != "127.0.0.1" {
			return "", errors.New("only numeric 127.0.0.1 host is permitted")
		}
		allowedPort := service.GetFixturePort()
		if port != allowedPort {
			return "", fmt.Errorf("port %s does not match allowed fixture port %s", port, allowedPort)
		}

		validPaths := map[string]bool{
			"/":         true,
			"/login":    true,
			"/health":   true,
			"/slow":     true,
			"/large":    true,
			"/500":      true,
			"/redirect": true,
			"/target":   true,
		}
		if !validPaths[parsedURL.Path] {
			return "", fmt.Errorf("path '%s' is not in allowed endpoint whitelist", parsedURL.Path)
		}
	}

	// 6. Build HTTP client with strict security settings
	client := &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy:                  nil,
			DisableCompression:     true,
			MaxResponseHeaderBytes: 64 * 1024,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	req, err := http.NewRequestWithContext(ctx, "GET", parsedURL.String(), nil)
	if err != nil {
		return "", fmt.Errorf("create http request: %w", err)
	}
	req.Header.Set("User-Agent", "AgentWorkbench-Trace/1.0")

	// 7. Execute HTTP request
	startTime := time.Now()
	resp, doErr := client.Do(req)
	headerDurationMs := time.Since(startTime).Milliseconds()

	if doErr != nil {
		errType := "connection_error"
		errMsg := doErr.Error()
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(doErr, context.Canceled) {
			errType = "context_canceled"
			errMsg = "request canceled by stop or timeout"
		} else if nErr, ok := doErr.(net.Error); ok && nErr.Timeout() {
			errType = "timeout"
			errMsg = "request timed out (5s limit exceeded)"
		}

		outData := map[string]interface{}{
			"request": map[string]interface{}{
				"method":     "GET",
				"url":        parsedURL.String(),
				"asset_id":   assetID.String(),
				"started_at": startTime.UTC().Format(time.RFC3339Nano),
			},
			"response": nil,
			"integrity": map[string]interface{}{
				"saved_bytes":  0,
				"is_truncated": false,
				"note":         "",
			},
			"error": map[string]interface{}{
				"type":    errType,
				"message": errMsg,
			},
		}
		outJSON, _ := json.Marshal(outData)
		return string(outJSON), doErr
	}
	defer resp.Body.Close()

	// 8. Bounded read: 64 KiB + 1 byte limit
	const maxBodyBytes = 64 * 1024
	bodyBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, int64(maxBodyBytes+1)))
	receivedAt := time.Now().UTC().Format(time.RFC3339Nano)
	totalDurationMs := time.Since(startTime).Milliseconds()

	headerMap := make(map[string]string)
	for k, v := range resp.Header {
		headerMap[k] = strings.Join(v, ", ")
	}

	if readErr != nil {
		errType := "read_body_error"
		if errors.Is(ctx.Err(), context.Canceled) || errors.Is(readErr, context.Canceled) {
			errType = "context_canceled"
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(readErr, context.DeadlineExceeded) {
			errType = "timeout"
		} else if errors.Is(readErr, io.ErrUnexpectedEOF) {
			errType = "incomplete_body"
		}

		type errOutPayload struct {
			Error     map[string]string `json:"error"`
			Integrity struct {
				SavedBytes  int    `json:"saved_bytes"`
				IsTruncated bool   `json:"is_truncated"`
				Note        string `json:"note"`
			} `json:"integrity"`
			Request struct {
				Method    string `json:"method"`
				URL       string `json:"url"`
				AssetID   string `json:"asset_id"`
				StartedAt string `json:"started_at"`
			} `json:"request"`
			Response struct {
				StatusCode       int               `json:"status_code"`
				Headers          map[string]string `json:"headers"`
				ReceivedAt       string            `json:"received_at"`
				DurationMs       int64             `json:"duration_ms"`
				HeaderDurationMs int64             `json:"header_duration_ms"`
				Body             string            `json:"body"`
			} `json:"response"`
		}
		var eo errOutPayload
		eo.Error = map[string]string{"type": errType, "message": readErr.Error()}
		eo.Integrity.SavedBytes = len(bodyBytes)
		eo.Integrity.IsTruncated = false
		eo.Integrity.Note = fmt.Sprintf("incomplete read: %v", readErr)
		eo.Request.Method = "GET"
		eo.Request.URL = parsedURL.String()
		eo.Request.AssetID = assetID.String()
		eo.Request.StartedAt = startTime.UTC().Format(time.RFC3339Nano)
		eo.Response.StatusCode = resp.StatusCode
		eo.Response.Headers = headerMap
		eo.Response.Body = string(bodyBytes)
		eo.Response.ReceivedAt = receivedAt
		eo.Response.DurationMs = totalDurationMs
		eo.Response.HeaderDurationMs = headerDurationMs
		outJSON, _ := json.Marshal(eo)
		return string(outJSON), fmt.Errorf("read response body: %w", readErr)
	}

	isTruncated := false
	note := ""
	if len(bodyBytes) > maxBodyBytes {
		isTruncated = true
		bodyBytes = bodyBytes[:maxBodyBytes]
		note = "saved partial response: response body exceeded 64 KiB limit"
	}

	// Use struct to guarantee stable JSON field order:
	// error/integrity/request/response — so headers always appear before body
	// when MaxToolOutputBytes truncates the model context window.
	type responsePayload struct {
		StatusCode       int               `json:"status_code"`
		Headers          map[string]string `json:"headers"`
		ReceivedAt       string            `json:"received_at"`
		DurationMs       int64             `json:"duration_ms"`
		HeaderDurationMs int64             `json:"header_duration_ms"`
		Body             string            `json:"body"`
	}
	type integrityPayload struct {
		SavedBytes  int    `json:"saved_bytes"`
		IsTruncated bool   `json:"is_truncated"`
		Note        string `json:"note"`
	}
	type requestPayload struct {
		Method    string `json:"method"`
		URL       string `json:"url"`
		AssetID   string `json:"asset_id"`
		StartedAt string `json:"started_at"`
	}
	type toolOutput struct {
		Error     interface{}      `json:"error"`
		Integrity integrityPayload `json:"integrity"`
		Request   requestPayload   `json:"request"`
		Response  responsePayload  `json:"response"`
	}
	outData := toolOutput{
		Error: nil,
		Integrity: integrityPayload{
			SavedBytes:  len(bodyBytes),
			IsTruncated: isTruncated,
			Note:        note,
		},
		Request: requestPayload{
			Method:    "GET",
			URL:       parsedURL.String(),
			AssetID:   assetID.String(),
			StartedAt: startTime.UTC().Format(time.RFC3339Nano),
		},
		Response: responsePayload{
			StatusCode:       resp.StatusCode,
			Headers:          headerMap,
			ReceivedAt:       receivedAt,
			DurationMs:       totalDurationMs,
			HeaderDurationMs: headerDurationMs,
			Body:             string(bodyBytes),
		},
	}
	outJSON, _ := json.Marshal(outData)
	return string(outJSON), nil
}
