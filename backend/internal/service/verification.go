package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"

	"agent-workbench/backend/internal/netcap"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// HTTPCondition describes an explicit response condition, not a model verdict.
// Status alone cannot distinguish a fixed issue from a login/error page.
type HTTPCondition struct {
	StatusCode   int    `json:"status_code"`
	BodyContains string `json:"body_contains,omitempty"`
	HeaderName   string `json:"header_name,omitempty"`
	HeaderValue  string `json:"header_value,omitempty"`
}

type FindingVerification struct {
	Vulnerable HTTPCondition `json:"vulnerable"`
	Fixed      HTTPCondition `json:"fixed"`
}

type targetResponse struct {
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
	Body       string            `json:"body"`
}

func (c HTTPCondition) valid() bool {
	if (c.HeaderName == "") != (c.HeaderValue == "") {
		return false
	}
	return c.StatusCode >= 100 && c.StatusCode <= 599 &&
		(strings.TrimSpace(c.BodyContains) != "" || (strings.TrimSpace(c.HeaderName) != "" && c.HeaderValue != ""))
}

func (c HTTPCondition) matches(r targetResponse) bool {
	if !c.valid() || c.StatusCode != r.StatusCode || (c.BodyContains != "" && !strings.Contains(r.Body, c.BodyContains)) {
		return false
	}
	if c.HeaderName != "" {
		for name, value := range r.Headers {
			if strings.EqualFold(name, c.HeaderName) {
				return value == c.HeaderValue
			}
		}
		return false
	}
	return true
}

// targetEvidence is deliberately an allowlist of actual target-reading adapters.
// Knowledge, orchestration and reporting tools must never become empirical proof.
// New target adapters need an explicit parser and boundary checks here.
func targetEvidence(ctx context.Context, tx pgx.Tx, runID, assetID, callID uuid.UUID, epoch int, sessionID *uuid.UUID) (targetResponse, error) {
	var result targetResponse
	var name, status, output string
	var callRun uuid.UUID
	var callSession *uuid.UUID
	var callEpoch int
	var args []byte
	err := tx.QueryRow(ctx, `SELECT tool_name,status,run_id,epoch_number,session_id,arguments,COALESCE(output,'')
		FROM agent_tool_calls WHERE id=$1`, callID).Scan(&name, &status, &callRun, &callEpoch, &callSession, &args, &output)
	if err != nil {
		return result, fmt.Errorf("%w: source tool call unavailable: %v", ErrInvalidEvidence, err)
	}
	if callRun != runID {
		return result, ErrCrossRunBoundary
	}
	if callEpoch != epoch {
		return result, ErrStaleEpoch
	}
	if sessionID != nil && (callSession == nil || *sessionID != *callSession) {
		return result, fmt.Errorf("%w: evidence is not from this retest session", ErrInvalidEvidence)
	}
	if status != "success" || (name != "get_endpoint" && name != "http_request") {
		return result, fmt.Errorf("%w: source must be a successful target verification tool", ErrInvalidEvidence)
	}
	if name == "http_request" {
		statusCode, headers, body, err := netcap.VerifyToolReceipt(ctx, tx, runID, assetID, callID, epoch, output)
		if err != nil {
			return result, fmt.Errorf("%w: trusted http receipt required", ErrInvalidEvidence)
		}
		return targetResponse{StatusCode: statusCode, Headers: headers, Body: body}, nil
	}
	var input struct {
		AssetID uuid.UUID `json:"asset_id"`
	}
	if json.Unmarshal(args, &input) != nil || input.AssetID == uuid.Nil || input.AssetID != assetID {
		return result, fmt.Errorf("%w: source must explicitly target the finding asset", ErrInvalidEvidence)
	}
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Request struct {
			AssetID uuid.UUID `json:"asset_id"`
		} `json:"request"`
		AssetID   uuid.UUID `json:"asset_id"`
		Integrity struct {
			IsTruncated bool `json:"is_truncated"`
		} `json:"integrity"`
		Response *targetResponse `json:"response"`
		targetResponse
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil ||
		(len(envelope.Error) > 0 && string(envelope.Error) != "null") || envelope.Integrity.IsTruncated ||
		(envelope.Request.AssetID != uuid.Nil && envelope.Request.AssetID != assetID) ||
		(envelope.AssetID != uuid.Nil && envelope.AssetID != assetID) {
		return result, fmt.Errorf("%w: invalid or incomplete target response", ErrInvalidEvidence)
	}
	result = envelope.targetResponse
	if envelope.Response != nil {
		result = *envelope.Response
	}
	if result.StatusCode < 100 || result.StatusCode > 599 {
		return result, fmt.Errorf("%w: source has no valid HTTP response", ErrInvalidEvidence)
	}
	if err := contradictingReceipt(ctx, tx, runID, assetID, callID, epoch, result.StatusCode); err != nil {
		return result, err
	}
	return result, nil
}

func contradictingReceipt(ctx context.Context, tx pgx.Tx, runID, assetID, callID uuid.UUID, epoch, status int) error {
	var mode, outcome string
	var storedStatus int
	var truncated bool
	var storedEpoch int
	err := tx.QueryRow(ctx, `SELECT capture_mode,outcome,status_code,truncated,epoch_number FROM http_receipts
		WHERE source_tool_call_id=$1 AND minted_by='gateway' ORDER BY created_at DESC LIMIT 1`, callID).Scan(&mode, &outcome, &storedStatus, &truncated, &storedEpoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return nil
	}
	if storedEpoch != epoch || truncated || outcome != "completed" || storedStatus != status || (mode != "direct" && mode != "tls_intercept") {
		return fmt.Errorf("%w: stored receipt does not match the cited response", ErrInvalidEvidence)
	}
	_ = runID
	_ = assetID
	return nil
}

// evaluateRetest runs inside the finish transaction so both the API and the
// background executor enforce exactly the same evidence contract.
func evaluateRetest(ctx context.Context, tx pgx.Tx, runID, findingID uuid.UUID, retestSession *uuid.UUID, evidence string, epoch int) (string, error) {
	var ref struct {
		SourceToolCallID uuid.UUID `json:"source_tool_call_id"`
	}
	if json.Unmarshal([]byte(evidence), &ref) != nil || ref.SourceToolCallID == uuid.Nil || retestSession == nil {
		return "", fmt.Errorf("%w: structured retest source_tool_call_id is required", ErrInvalidEvidence)
	}
	var assetID uuid.UUID
	var details []byte
	if err := tx.QueryRow(ctx, `SELECT f.asset_id,cr.details FROM findings f JOIN check_results cr ON cr.id=f.check_result_id WHERE f.id=$1 AND f.run_id=$2`, findingID, runID).Scan(&assetID, &details); err != nil {
		return "", err
	}
	var spec struct {
		Verification *FindingVerification `json:"verification"`
	}
	if json.Unmarshal(details, &spec) != nil || spec.Verification == nil || !spec.Verification.Vulnerable.valid() || !spec.Verification.Fixed.valid() {
		return "inconclusive", nil
	}
	if unsafe, _ := originalReplayUnsafe(ctx, tx, findingID); unsafe {
		return "inconclusive", nil
	}
	response, err := targetEvidence(ctx, tx, runID, assetID, ref.SourceToolCallID, epoch, retestSession)
	if err != nil {
		return "", err
	}
	vulnerable, fixed := spec.Verification.Vulnerable.matches(response), spec.Verification.Fixed.matches(response)
	if vulnerable == fixed {
		return "inconclusive", nil
	}
	if vulnerable {
		return "still_reproducible", nil
	}
	return "fixed", nil
}

// FindingReplayUnsafe reports whether an automatic tool replay is forbidden.
// A database error is returned instead of being treated as safe. A missing
// receipt is not unsafe. scheme is the original receipt URL scheme, or empty
// when no gateway receipt exists.
func (s *RunService) FindingReplayUnsafe(ctx context.Context, findingID uuid.UUID) (unsafe bool, reason, scheme string, err error) {
	if s == nil || s.pool == nil {
		return false, "", "", errors.New("database pool unavailable")
	}
	var source *uuid.UUID
	if err = s.pool.QueryRow(ctx, `SELECT source_tool_call_id FROM findings WHERE id=$1`, findingID).Scan(&source); err != nil {
		return false, "", "", err
	}
	if source == nil {
		return false, "", "", nil
	}
	var replaySafe bool
	var method, mode, outcome, cred, session, rawURL string
	err = s.pool.QueryRow(ctx, `SELECT replay_safe, method, capture_mode, outcome, credential_ref, session_ref, url
		FROM http_receipts WHERE source_tool_call_id=$1 AND minted_by='gateway' ORDER BY created_at DESC LIMIT 1`, *source).Scan(
		&replaySafe, &method, &mode, &outcome, &cred, &session, &rawURL)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", "", nil
	}
	if err != nil {
		return false, "", "", err
	}
	unsafe, reason = netcap.ReplayUnsafe(replaySafe, method, mode, outcome, cred, session)
	if parsed, parseErr := url.Parse(rawURL); parseErr == nil {
		scheme = parsed.Scheme
	}
	return unsafe, reason, scheme, nil
}

func originalReplayUnsafe(ctx context.Context, tx pgx.Tx, findingID uuid.UUID) (bool, string) {
	var source *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT source_tool_call_id FROM findings WHERE id=$1`, findingID).Scan(&source); err != nil || source == nil {
		return false, ""
	}
	var replaySafe bool
	var method, mode, outcome, cred, session string
	err := tx.QueryRow(ctx, `SELECT replay_safe, method, capture_mode, outcome, credential_ref, session_ref
		FROM http_receipts WHERE source_tool_call_id=$1 AND minted_by='gateway' ORDER BY created_at DESC LIMIT 1`, *source).Scan(
		&replaySafe, &method, &mode, &outcome, &cred, &session)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, ""
	}
	if err != nil {
		return false, ""
	}
	return netcap.ReplayUnsafe(replaySafe, method, mode, outcome, cred, session)
}
