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

// ErrNotEmpirical is returned when a cited source is not a trusted HTTP exchange.
var ErrNotEmpirical = errors.New("not empirical http evidence")

// ReplayUnsafe reports that an original exchange must not be marked fixed from
// a fresh unauthenticated GET. Login pages and side effects are inconclusive.
func ReplayUnsafe(replaySafe bool, method, captureMode, outcome, credentialRef, sessionRef string) (bool, string) {
	if captureMode == contract.CaptureConnectMeta {
		return true, "CONNECT metadata is not a response body and cannot be auto-replayed"
	}
	if credentialRef != "" || sessionRef != "" {
		return true, "authenticated request cannot be safely auto-replayed"
	}
	if method != "" && method != http.MethodGet && method != http.MethodHead {
		return true, "side-effect request cannot be safely auto-replayed"
	}
	if !replaySafe && (credentialRef != "" || sessionRef != "" || (method != http.MethodGet && method != http.MethodHead && method != "")) {
		return true, "authenticated or side-effect request cannot be safely auto-replayed"
	}
	if outcome != "" && outcome != "completed" {
		return true, "incomplete exchange cannot be safely auto-replayed"
	}
	if !replaySafe && captureMode != "" {
		return true, "original exchange is not marked replay-safe"
	}
	return false, ""
}

// VerifyToolReceipt loads a gateway-minted receipt bound to this tool call.
// Model JSON, command stdout, and a receipt from another asset or epoch fail.
func VerifyToolReceipt(ctx context.Context, tx pgx.Tx, runID, assetID, callID uuid.UUID, epoch int, output string) (int, map[string]string, string, error) {
	var cited struct {
		ReceiptID uuid.UUID `json:"receipt_id"`
		Receipt   struct {
			ReceiptID uuid.UUID `json:"receipt_id"`
		} `json:"receipt"`
	}
	_ = json.Unmarshal([]byte(output), &cited)
	receiptID := cited.ReceiptID
	if receiptID == uuid.Nil {
		receiptID = cited.Receipt.ReceiptID
	}
	if receiptID == uuid.Nil {
		return 0, nil, "", ErrNotEmpirical
	}
	var mode, outcome, minted, reqHash, respHash, method string
	var status int
	var truncated bool
	var storedAsset *uuid.UUID
	var storedRun uuid.UUID
	var storedEpoch int
	var source *uuid.UUID
	var respArt *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT capture_mode,outcome,minted_by,request_hash,response_hash,method,status_code,truncated,asset_id,run_id,epoch_number,source_tool_call_id,response_artifact_id
		FROM http_receipts WHERE id=$1`, receiptID).Scan(&mode, &outcome, &minted, &reqHash, &respHash, &method, &status, &truncated, &storedAsset, &storedRun, &storedEpoch, &source, &respArt)
	if err != nil {
		return 0, nil, "", ErrNotEmpirical
	}
	if minted != "gateway" || source == nil || *source != callID || storedRun != runID || storedEpoch != epoch {
		return 0, nil, "", ErrNotEmpirical
	}
	if storedAsset == nil || *storedAsset != assetID {
		return 0, nil, "", ErrNotEmpirical
	}
	if mode != contract.CaptureDirect && mode != contract.CaptureTLSIntercept {
		return 0, nil, "", ErrNotEmpirical
	}
	if outcome != "completed" || truncated || status < 100 || status > 599 || respArt == nil {
		return 0, nil, "", ErrNotEmpirical
	}
	var rawHeaders []byte
	var body []byte
	var storedSum string
	if err := tx.QueryRow(ctx, `SELECT headers,content,sha256 FROM http_capture_artifacts WHERE id=$1 AND run_id=$2 AND epoch_number=$3`, *respArt, runID, epoch).Scan(&rawHeaders, &body, &storedSum); err != nil {
		return 0, nil, "", ErrNotEmpirical
	}
	if sha256Hex(body) != storedSum {
		return 0, nil, "", ErrNotEmpirical
	}
	header := http.Header{}
	_ = json.Unmarshal(rawHeaders, &header)
	if hashParts(status, header, body, "", "") != respHash {
		return 0, nil, "", ErrNotEmpirical
	}
	flat := map[string]string{}
	for key, values := range header {
		flat[key] = strings.Join(values, ", ")
	}
	return status, flat, string(body), nil
}
