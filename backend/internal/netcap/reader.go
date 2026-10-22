package netcap

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"agent-workbench/backend/internal/contract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type RowReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ToolExchange is server-read evidence, never a model-authored response body.
// ReceiptID identifies one exchange; RequestID is its network request identity.
type ToolExchange struct {
	ReceiptID, RequestID, CallID, AssetID uuid.UUID
	SessionID, WorkItemID                 *uuid.UUID
	Epoch                                 int
	Method, URL, FinalURL, Outcome        string
	Status                                int
	Truncated                             bool
	RequestArtifactID, ResponseArtifactID uuid.UUID
	Headers                               map[string]string
	Body                                  string
}

// ReadToolExchange centralizes binding, asset and response integrity checks.
// Visits may accept a partial body; discovery and empirical proof require it complete.
func ReadToolExchange(ctx context.Context, q RowReader, runID, callID uuid.UUID, epoch int, complete bool) (*ToolExchange, error) {
	e := &ToolExchange{CallID: callID}
	var mode, minted, responseHash, status, name, output string
	var callSession, receiptSession, receiptWork, sessionWork *uuid.UUID
	var headers, body []byte
	var bodyHash string
	var artifactTruncated bool
	err := q.QueryRow(ctx, `SELECT h.id,h.request_id,h.asset_id,h.epoch_number,h.method,h.url,h.final_url,h.outcome,h.status_code,h.truncated,
 h.request_artifact_id,h.response_artifact_id,h.capture_mode,h.minted_by,h.response_hash,
 tc.status,tc.tool_name,COALESCE(tc.output,''),tc.session_id,h.session_id,h.work_item_id,s.work_item_id,
 a.headers,a.content,a.sha256,a.truncated
 FROM http_receipts h JOIN agent_tool_calls tc ON tc.id=h.source_tool_call_id AND tc.run_id=h.run_id AND tc.epoch_number=h.epoch_number
 LEFT JOIN agent_sessions s ON s.id=tc.session_id AND s.run_id=tc.run_id AND s.epoch_number=tc.epoch_number
 JOIN http_capture_artifacts a ON a.id=h.response_artifact_id AND a.run_id=h.run_id AND a.epoch_number=h.epoch_number AND a.kind='response'
 WHERE h.run_id=$1 AND h.source_tool_call_id=$2 AND h.epoch_number=$3
 AND h.asset_id IS NOT NULL AND EXISTS(SELECT 1 FROM run_assets ra WHERE ra.run_id=h.run_id AND ra.asset_id=h.asset_id)
 ORDER BY h.created_at DESC LIMIT 1`, runID, callID, epoch).Scan(
		&e.ReceiptID, &e.RequestID, &e.AssetID, &e.Epoch, &e.Method, &e.URL, &e.FinalURL, &e.Outcome, &e.Status, &e.Truncated,
		&e.RequestArtifactID, &e.ResponseArtifactID, &mode, &minted, &responseHash, &status, &name, &output, &callSession, &receiptSession, &receiptWork, &sessionWork,
		&headers, &body, &bodyHash, &artifactTruncated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotEmpirical
	}
	if err != nil {
		return nil, err
	}
	if minted != "gateway" || status != "success" || (name != "http_request" && name != "get_endpoint") ||
		(mode != contract.CaptureDirect && mode != contract.CaptureTLSIntercept) || e.Outcome != "completed" || e.Status < 100 || e.Status > 599 ||
		(complete && (e.Truncated || artifactTruncated)) {
		return nil, ErrNotEmpirical
	}
	if receiptSession != nil && (callSession == nil || *receiptSession != *callSession) {
		return nil, ErrNotEmpirical
	}
	if receiptWork != nil && (sessionWork == nil || *receiptWork != *sessionWork) {
		return nil, ErrNotEmpirical
	}
	if sessionWork != nil {
		var assigned bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_item_assets WHERE work_item_id=$1 AND asset_id=$2)`, sessionWork, e.AssetID).Scan(&assigned); err != nil {
			return nil, err
		}
		if !assigned {
			return nil, ErrNotEmpirical
		}
	}
	if name == "http_request" {
		if callSession == nil || sessionWork == nil || receiptSession == nil || receiptWork == nil {
			return nil, ErrNotEmpirical
		}
		var cited struct {
			Receipt struct {
				ID uuid.UUID `json:"receipt_id"`
			} `json:"receipt"`
			ID uuid.UUID `json:"receipt_id"`
		}
		if json.Unmarshal([]byte(output), &cited) != nil || (cited.ID != e.ReceiptID && cited.Receipt.ID != e.ReceiptID) {
			return nil, ErrNotEmpirical
		}
	}
	var h http.Header
	if json.Unmarshal(headers, &h) != nil || sha256Hex(body) != bodyHash || hashParts(e.Status, h, body, "", "") != responseHash {
		return nil, ErrNotEmpirical
	}
	e.Headers = map[string]string{}
	for k, v := range h {
		e.Headers[k] = strings.Join(v, ", ")
	}
	e.Body = string(body)
	e.SessionID, e.WorkItemID = callSession, sessionWork
	return e, nil
}
