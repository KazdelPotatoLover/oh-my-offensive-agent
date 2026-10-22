package netcap

import (
	"context"
	"errors"
	"net/http"

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
	e, err := ReadToolExchange(ctx, tx, runID, callID, epoch, true)
	if err != nil {
		return 0, nil, "", err
	}
	if e.AssetID != assetID {
		return 0, nil, "", ErrNotEmpirical
	}
	return e.Status, e.Headers, e.Body, nil
}
