package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"agent-workbench/backend/internal/netcap"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type TrafficFilter struct {
	Origin      string `json:"origin,omitempty"`
	Host        string `json:"host,omitempty"`
	URLContains string `json:"url_contains,omitempty"`
	Method      string `json:"method,omitempty"`
	Status      int    `json:"status,omitempty"`
	Epoch       int    `json:"epoch,omitempty"`
	Limit       int    `json:"limit,omitempty"`
	After       string `json:"after,omitempty"`
}

type TrafficEntry struct {
	ReceiptID          uuid.UUID  `json:"receipt_id"`
	RequestID          uuid.UUID  `json:"request_id"`
	Epoch              int        `json:"epoch_number"`
	Method             string     `json:"method"`
	URL                string     `json:"url"`
	Status             int        `json:"status_code"`
	ResponseBytes      int        `json:"response_bytes"`
	Outcome            string     `json:"outcome"`
	Truncated          bool       `json:"truncated"`
	AssetID            *uuid.UUID `json:"asset_id,omitempty"`
	SourceToolCallID   *uuid.UUID `json:"source_tool_call_id,omitempty"`
	WorkItemID         *uuid.UUID `json:"work_item_id,omitempty"`
	SessionID          *uuid.UUID `json:"session_id,omitempty"`
	RequestArtifactID  *uuid.UUID `json:"request_artifact_id,omitempty"`
	ResponseArtifactID *uuid.UUID `json:"response_artifact_id,omitempty"`
	CaptureMode        string     `json:"capture_mode"`
	CreatedAt          time.Time  `json:"created_at"`
}

func (s *RunService) trafficPolicy(ctx context.Context, runID uuid.UUID) (netcap.Policy, int, error) {
	var raw []byte
	var epoch int
	if err := s.pool.QueryRow(ctx, `SELECT execution_input,current_epoch FROM runs WHERE id=$1`, runID).Scan(&raw, &epoch); err != nil {
		return netcap.Policy{}, 0, err
	}
	if err := validateContextEpoch(ctx, epoch); err != nil {
		return netcap.Policy{}, 0, err
	}
	p, err := netcap.PolicyFromExecutionInput(raw)
	return p, epoch, err
}

// AuthorizeTrafficReader checks the server-bound caller, never a model role.
// All roles see the same redacted Run material. Old epochs are read-only history.
func (s *RunService) AuthorizeTrafficReader(ctx context.Context, runID, sessionID, workID uuid.UUID) error {
	var role string
	var owner *uuid.UUID
	var epoch, current int
	err := s.pool.QueryRow(ctx, `SELECT s.role,s.work_item_id,s.epoch_number,r.current_epoch FROM agent_sessions s JOIN runs r ON r.id=s.run_id WHERE s.id=$1 AND s.run_id=$2`, sessionID, runID).Scan(&role, &owner, &epoch, &current)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return ErrInvalidEvidence
	}
	if err := validateContextEpoch(ctx, current); err != nil {
		return err
	}
	switch role {
	case "worker":
		if epoch != current {
			return ErrStaleEpoch
		}
		if owner == nil || *owner != workID {
			return ErrAssetOutOfScope
		}
	case "planner", "verifier":
		if epoch != current {
			return ErrStaleEpoch
		}
		if workID != uuid.Nil {
			return ErrAssetOutOfScope
		}
	case "mainagent":
		if workID != uuid.Nil {
			return ErrAssetOutOfScope
		}
	default:
		return ErrInvalidEvidence
	}
	return nil
}

func trafficAllowed(p netcap.Policy, method, rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	if p.AllowsMethod(method) != nil {
		return false
	}
	_, err = p.Match(u)
	return err == nil
}

const trafficColumns = `h.id,h.request_id,h.epoch_number,h.method,h.url,h.status_code,COALESCE(a.byte_length,0),h.outcome,h.truncated,h.asset_id,h.source_tool_call_id,h.work_item_id,h.session_id,h.request_artifact_id,h.response_artifact_id,h.capture_mode,h.created_at`

func (s *RunService) SearchHTTPTraffic(ctx context.Context, runID uuid.UUID, f TrafficFilter) (map[string]any, error) {
	if f.Limit == 0 {
		f.Limit = 5
	}
	if f.Limit < 1 || f.Limit > 20 || f.Epoch < 0 || f.Status < 0 || (f.Status > 0 && f.Status < 100) || f.Status > 599 || len(f.URLContains) > 512 || len(f.Host) > 253 || len(f.Origin) > 512 || len(f.Method) > 16 {
		return nil, fmt.Errorf("invalid traffic filter or limit (1–20)")
	}
	p, current, err := s.trafficPolicy(ctx, runID)
	if err != nil {
		return nil, err
	}
	if f.Epoch > current {
		return nil, ErrStaleEpoch
	}
	var after *uuid.UUID
	if f.After != "" {
		id, e := uuid.Parse(f.After)
		if e != nil {
			return nil, fmt.Errorf("after must be a receipt_id")
		}
		after = &id
		var valid bool
		if err := s.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM http_receipts WHERE id=$1 AND run_id=$2 AND epoch_number BETWEEN 1 AND $3 AND minted_by='gateway')`, id, runID, current).Scan(&valid); err != nil {
			return nil, err
		}
		if !valid {
			return nil, ErrInvalidEvidence
		}
	}
	if f.Origin != "" {
		u, e := url.Parse(f.Origin)
		if e != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, fmt.Errorf("origin must be an HTTP(S) origin")
		}
		f.Origin = strings.TrimSuffix(u.String(), "/")
	}
	f.Method = strings.ToUpper(f.Method)
	rows, err := s.pool.Query(ctx, `SELECT `+trafficColumns+` FROM http_receipts h LEFT JOIN http_capture_artifacts a ON a.id=h.response_artifact_id
 WHERE h.run_id=$1 AND h.epoch_number BETWEEN 1 AND $2 AND h.minted_by='gateway'
 AND ($3=0 OR h.epoch_number=$3) AND ($4='' OR h.method=$4) AND ($5=0 OR h.status_code=$5)
 AND ($6='' OR strpos(h.url,$6)>0)
 AND ($7::uuid IS NULL OR (h.created_at,h.id)>(SELECT created_at,id FROM http_receipts WHERE id=$7 AND run_id=$1 AND epoch_number<=$2))
 ORDER BY h.created_at,h.id LIMIT 201`, runID, current, f.Epoch, f.Method, f.Status, f.URLContains, after)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := []TrafficEntry{}
	next := ""
	scanned := 0
	hasMore := false
	for rows.Next() {
		var e TrafficEntry
		if err := rows.Scan(&e.ReceiptID, &e.RequestID, &e.Epoch, &e.Method, &e.URL, &e.Status, &e.ResponseBytes, &e.Outcome, &e.Truncated, &e.AssetID, &e.SourceToolCallID, &e.WorkItemID, &e.SessionID, &e.RequestArtifactID, &e.ResponseArtifactID, &e.CaptureMode, &e.CreatedAt); err != nil {
			return nil, err
		}
		if len(entries) >= f.Limit || scanned >= 200 {
			hasMore = true
			break
		}
		scanned++
		next = e.ReceiptID.String()
		if !trafficAllowed(p, e.Method, e.URL) {
			continue
		}
		u, _ := url.Parse(e.URL)
		if f.Origin != "" && u.Scheme+"://"+u.Host != f.Origin {
			continue
		}
		if f.Host != "" && !strings.EqualFold(u.Host, f.Host) && !strings.EqualFold(u.Hostname(), f.Host) {
			continue
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if !hasMore {
		next = ""
	}
	return map[string]any{"entries": entries, "next_cursor": next, "has_more": hasMore, "read_tool": "get_http_exchange", "historical_material": true}, nil
}

func (s *RunService) ReadHTTPExchange(ctx context.Context, runID, receiptID uuid.UUID) (map[string]any, error) {
	p, current, err := s.trafficPolicy(ctx, runID)
	if err != nil {
		return nil, err
	}
	var e TrafficEntry
	err = s.pool.QueryRow(ctx, `SELECT `+trafficColumns+` FROM http_receipts h LEFT JOIN http_capture_artifacts a ON a.id=h.response_artifact_id WHERE h.id=$1 AND h.run_id=$2 AND h.epoch_number BETWEEN 1 AND $3 AND h.minted_by='gateway'`, receiptID, runID, current).Scan(&e.ReceiptID, &e.RequestID, &e.Epoch, &e.Method, &e.URL, &e.Status, &e.ResponseBytes, &e.Outcome, &e.Truncated, &e.AssetID, &e.SourceToolCallID, &e.WorkItemID, &e.SessionID, &e.RequestArtifactID, &e.ResponseArtifactID, &e.CaptureMode, &e.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrInvalidEvidence
		}
		return nil, err
	}
	if !trafficAllowed(p, e.Method, e.URL) {
		return nil, ErrAssetOutOfScope
	}
	parts := map[string]any{}
	complete := e.Outcome == "completed" && !e.Truncated && (e.CaptureMode == "direct" || e.CaptureMode == "tls_intercept") && e.RequestArtifactID != nil && e.ResponseArtifactID != nil
	for name, id := range map[string]*uuid.UUID{"request": e.RequestArtifactID, "response": e.ResponseArtifactID} {
		if id == nil {
			continue
		}
		page, err := s.ReadArtifact(ctx, runID, *id, 0, 4096)
		if err != nil {
			return nil, err
		}
		var raw []byte
		if err := s.pool.QueryRow(ctx, `SELECT headers FROM http_capture_artifacts WHERE id=$1`, *id).Scan(&raw); err != nil {
			return nil, err
		}
		// Header redaction is shared with all capture readers.
		var h map[string][]string
		_ = json.Unmarshal(raw, &h)
		page["headers"] = netcap.RedactedHeaders(h)
		if page["capture_truncated"] == true {
			complete = false
		}
		parts[name] = page
	}
	return map[string]any{"exchange": e, "parts": parts, "body_read_tool": "read_artifact", "historical_material": true, "new_verification": false, "complete": complete, "redacted": true}, nil
}
