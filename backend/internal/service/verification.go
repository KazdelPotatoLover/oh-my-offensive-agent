package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
	if name != "get_endpoint" || status != "success" {
		return result, fmt.Errorf("%w: source must be a successful target verification tool", ErrInvalidEvidence)
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
	return result, nil
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
