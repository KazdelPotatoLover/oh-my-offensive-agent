package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// legacyControl is the decoded body of an old control route. expected_epoch is
// passed through unchanged. Zero is not rewritten to the current epoch.
type legacyControl struct {
	ExpectedEpoch        int             `json:"expected_epoch"`
	Instructions         string          `json:"instructions"`
	InputDataOverride    json.RawMessage `json:"input_data_override"`
	Reason               string          `json:"reason"`
	Summary              string          `json:"summary"`
	ExpectedAttemptID    string          `json:"expected_attempt_id"`
	AdditionalModelCalls int             `json:"additional_model_calls"`
	AdditionalToolCalls  int             `json:"additional_tool_calls"`
	AdditionalSeconds    int             `json:"additional_seconds"`
}

func (h *RunHandler) readLegacyControl(r *http.Request) (legacyControl, error) {
	var body legacyControl
	if r.Body == nil {
		return body, nil
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(&body); err != nil {
		if errors.Is(err, io.EOF) {
			return body, nil
		}
		return body, err
	}
	return body, nil
}

func (h *RunHandler) applyLegacy(w http.ResponseWriter, r *http.Request, runID uuid.UUID, workItemID *uuid.UUID, cmdType contract.CommandType, body legacyControl, payload json.RawMessage) (*contract.CommandResult, bool) {
	key := strings.TrimSpace(r.Header.Get(contract.HeaderIdempotencyKey))
	if err := contract.RequireIdempotencyKey(key); err != nil {
		respondError(w, http.StatusBadRequest, "Idempotency-Key is required")
		return nil, false
	}
	if h == nil || h.controller == nil {
		respondError(w, http.StatusServiceUnavailable, "execution controller is not configured")
		return nil, false
	}
	cmd := contract.Command{
		Type:          cmdType,
		ExpectedEpoch: body.ExpectedEpoch,
		WorkItemID:    workItemID,
		Payload:       payload,
	}
	if body.ExpectedAttemptID != "" {
		attemptID, err := uuid.Parse(body.ExpectedAttemptID)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid expected_attempt_id")
			return nil, false
		}
		cmd.ExpectedAttemptID = &attemptID
	}
	result, err := h.controller.Apply(r.Context(), humanPrincipal(r, runID), humanScope(runID, body.ExpectedEpoch), key, cmd)
	if err != nil {
		writeLegacyControlError(w, err)
		return nil, false
	}
	return result, true
}

func writeLegacyControlError(w http.ResponseWriter, err error) {
	var ce *contract.Error
	if !errors.As(err, &ce) || ce == nil {
		respondError(w, http.StatusInternalServerError, "control request failed")
		return
	}
	respondError(w, ce.Status(), ce.Message)
}

func rejectSteerOverride(w http.ResponseWriter, raw json.RawMessage) bool {
	if len(raw) == 0 || string(raw) == "null" {
		return false
	}
	respondError(w, http.StatusConflict, "steer cannot change the immutable input/profile; submit a new authorized direction instead")
	return true
}

func (h *RunHandler) legacyGrant(w http.ResponseWriter, r *http.Request, runID uuid.UUID, result *contract.CommandResult) {
	var payload struct {
		GrantID uuid.UUID `json:"grant_id"`
	}
	if result != nil && len(result.Result) > 0 {
		_ = json.Unmarshal(result.Result, &payload)
	}
	if payload.GrantID != uuid.Nil && h.runService != nil {
		grants, err := h.runService.ListBudgetGrants(r.Context(), runID)
		if err == nil {
			for _, grant := range grants {
				if grant.ID == payload.GrantID {
					respondJSON(w, http.StatusCreated, grant)
					return
				}
			}
		}
	}
	respondJSON(w, http.StatusCreated, result)
}

func (h *RunHandler) legacyWorkItem(w http.ResponseWriter, r *http.Request, workItemID uuid.UUID) {
	item, err := h.runService.GetWorkItem(r.Context(), workItemID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "work item was updated but could not be reloaded")
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"work_item": item})
}

func (h *RunHandler) legacyRun(w http.ResponseWriter, r *http.Request, runID uuid.UUID) {
	run, err := h.runService.Get(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "run was updated but could not be reloaded")
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func epochPayload(body legacyControl) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{"reason": body.Reason, "summary": body.Summary})
	return raw
}

func grantPayload(body legacyControl) json.RawMessage {
	raw, _ := json.Marshal(map[string]any{
		"additional_model_calls": body.AdditionalModelCalls,
		"additional_tool_calls":  body.AdditionalToolCalls,
		"additional_seconds":     body.AdditionalSeconds,
		"reason":                 body.Reason,
	})
	return raw
}

func textPayload(field, value string) json.RawMessage {
	raw, _ := json.Marshal(map[string]string{field: value})
	return raw
}
