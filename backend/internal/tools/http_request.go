package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

// HTTPRequestTool is the scoped HTTP client. A model cannot set allow=true to
// widen the frozen network policy, and it cannot pass raw secret headers.
type HTTPRequestTool struct {
	runID      uuid.UUID
	workItemID uuid.UUID
	attemptID  uuid.UUID
	sessionID  uuid.UUID
	runService *service.RunService
	transport  contract.TargetTransport
}

func NewHTTPRequestTool(runID, workItemID, attemptID, sessionID uuid.UUID, runService *service.RunService, transport contract.TargetTransport) *HTTPRequestTool {
	return &HTTPRequestTool{
		runID: runID, workItemID: workItemID, attemptID: attemptID, sessionID: sessionID,
		runService: runService, transport: transport,
	}
}

// HTTPRequestAdapter is the factory Agent 0 registers. It uses the installed
// transport and does not dial when that transport is missing.
func HTTPRequestAdapter() Adapter {
	return Adapter{
		Name: "http_request", Version: "1",
		Factory: func(c AdapterContext) Tool {
			return NewHTTPRequestTool(c.RunID, c.WorkItemID, c.AttemptID, c.SessionID, c.Service, netcap.Shared())
		},
	}
}

func (t *HTTPRequestTool) Name() string { return "http_request" }

func (t *HTTPRequestTool) Description() string {
	return "Send an authorized HTTP request. Methods, origins, and credentials come from the frozen run policy and server-held references."
}

func (t *HTTPRequestTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"asset_id": {"type": "string"},
			"url": {"type": "string"},
			"method": {"type": "string"},
			"query": {"type": "object"},
			"headers": {"type": "object"},
			"body_json": {},
			"body_text": {"type": "string"},
			"form": {"type": "object"},
			"credential_ref": {"type": "string"},
			"session_ref": {"type": "string"},
			"timeout_seconds": {"type": "integer"}
		},
		"required": ["method"]
	}`)
}

func (t *HTTPRequestTool) Validate(argumentsJSON []byte) error {
	var in contract.HTTPRequestInput
	if err := json.Unmarshal(argumentsJSON, &in); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	return in.Validate()
}

func (t *HTTPRequestTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var in contract.HTTPRequestInput
	if err := json.Unmarshal(argumentsJSON, &in); err != nil {
		return "", contractErrorJSON(contract.CodeInvalidInput, "invalid arguments JSON")
	}
	// allow is not a field. A model cannot authorize itself by adding it.
	if err := in.Validate(); err != nil {
		return "", err
	}
	transport := t.transport
	if transport == nil {
		transport = netcap.Shared()
	}
	if transport == nil {
		return "", contract.ErrNotImplemented
	}
	if t.runService == nil {
		return "", contract.ErrNotImplemented
	}
	var epoch int
	if err := t.runService.Pool().QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1`, t.runID).Scan(&epoch); err != nil {
		return "", fmt.Errorf("load run epoch: %w", err)
	}
	scope := contract.ExecutionScope{
		RunID: t.runID, EpochNumber: epoch, Role: contract.RoleWorker,
	}
	if t.workItemID != uuid.Nil {
		id := t.workItemID
		scope.WorkItemID = &id
	}
	if t.attemptID != uuid.Nil {
		id := t.attemptID
		scope.AttemptID = &id
	}
	if t.sessionID != uuid.Nil {
		id := t.sessionID
		scope.SessionID = &id
	}
	if err := scope.Validate(); err != nil {
		return "", err
	}
	receipt, err := transport.Do(ctx, scope, in)
	if receipt != nil {
		if bindErr := netcap.NewStore(t.runService.Pool()).BindPendingToolCall(ctx, scope, receipt.ReceiptID, t.Name(), argumentsJSON); bindErr != nil && err == nil {
			err = bindErr
		}
	}
	if err != nil {
		payload := map[string]any{"outcome": outcomeFrom(err)}
		if receipt != nil {
			payload["receipt"] = receipt
		}
		raw, _ := json.Marshal(payload)
		return string(raw), err
	}
	out := map[string]any{
		"outcome":          "completed",
		"receipt":          receipt,
		"status_code":      receipt.StatusCode,
		"response_preview": receipt.ResponsePreview,
	}
	raw, _ := json.Marshal(out)
	if strings.Contains(string(raw), in.CredentialRef) && strings.Contains(in.CredentialRef, " ") {
		return "", contractErrorJSON(contract.CodeUnauthorized, "refused to emit a secret")
	}
	return string(raw), nil
}

func outcomeFrom(err error) string {
	if err == nil {
		return "completed"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "cancel"):
		return "cancelled"
	case strings.Contains(msg, "deadline") || strings.Contains(msg, "timeout"):
		return "timeout"
	case strings.Contains(msg, "certificate") || strings.Contains(msg, "tls"):
		return "tls_error"
	default:
		return "failed"
	}
}

func contractErrorJSON(code contract.ErrorCode, message string) error {
	return &contract.Error{Code: code, Scope: contract.ScopeRequest, Message: message, SuggestedAction: contract.ActionNone}
}
