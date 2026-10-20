package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
	"unicode/utf8"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

var (
	ErrPersistenceFailure = errors.New("persistence error: database write failed")
)

type SessionResult struct {
	SessionID       uuid.UUID
	FinalContent    string
	FinishReason    string
	ModelCallsCount int
	ToolCallsCount  int
	TotalTokens     int
}

type SessionRunnerOption func(*SessionRunner)

func WithSessionModelCallHook(hook func(round int)) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.onModelCallHook = hook
	}
}

func WithSessionToolCallHook(hook func(callID string, toolName string)) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.onToolCallHook = hook
	}
}

// SessionRunner executes a single multi-turn conversational tool-calling session.
type SessionRunner struct {
	sessionID       uuid.UUID
	runID           uuid.UUID
	role            string
	runService      *service.RunService
	provider        provider.Provider
	registry        *tools.Registry
	budget          model.BudgetConfig
	onModelCallHook func(round int)
	onToolCallHook  func(callID string, toolName string)
}

func NewSessionRunner(
	sessionID uuid.UUID,
	runID uuid.UUID,
	role string,
	runService *service.RunService,
	p provider.Provider,
	reg *tools.Registry,
	budget model.BudgetConfig,
	opts ...SessionRunnerOption,
) *SessionRunner {
	if reg == nil {
		reg = tools.DefaultRegistry()
	}
	sr := &SessionRunner{
		sessionID:  sessionID,
		runID:      runID,
		role:       role,
		runService: runService,
		provider:   p,
		registry:   reg,
		budget:     budget,
	}
	for _, opt := range opts {
		opt(sr)
	}
	return sr
}

// Run executes the session until the model returns a final text response, exhausts budget, or is cancelled.
func (sr *SessionRunner) Run(ctx context.Context, initialMessages []provider.ChatMessage) (*SessionResult, error) {
	// Persist initial messages with session_id
	for _, msg := range initialMessages {
		var toolCallID *string
		if msg.ToolCallID != "" {
			id := msg.ToolCallID
			toolCallID = &id
		}
		var toolCallsJSON json.RawMessage
		if len(msg.ToolCalls) > 0 {
			toolCallsJSON, _ = json.Marshal(msg.ToolCalls)
		}
		var payload json.RawMessage
		if msg.NativePayload != nil {
			payload, _ = json.Marshal(msg.NativePayload)
		}
		_, err := sr.runService.InsertAgentMessageWithPayload(
			context.Background(), sr.runID, &sr.sessionID, string(msg.Role), msg.Content, toolCallID, toolCallsJSON, payload)
		if err != nil {
			slog.Error("session runner: failed to persist initial message", "session_id", sr.sessionID, "error", err)
			sr.updateSessionTerminal("failed", "persistence_error")
			return nil, fmt.Errorf("%w: insert initial message: %v", ErrPersistenceFailure, err)
		}
	}

	messages := make([]provider.ChatMessage, len(initialMessages))
	copy(messages, initialMessages)

	modelCallCount := 0
	toolCallCount := 0
	totalTokens := 0

	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			reason := "user_requested_stop"
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				reason = "timeout"
			}
			sr.updateSessionTerminal("stopped", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, ctx.Err()
		default:
		}

		// Check model call budget
		if modelCallCount >= sr.budget.MaxModelCalls {
			reason := "max_model_calls_exceeded"
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}

		// 1. Record model call start in DB
		round := modelCallCount + 1
		modelCallRec, err := sr.runService.CreateModelCallWithSession(context.Background(), sr.runID, &sr.sessionID, round)
		if err != nil {
			slog.Error("session runner: failed to create model call", "session_id", sr.sessionID, "error", err)
			sr.updateSessionTerminal("failed", "persistence_error")
			return nil, fmt.Errorf("%w: create model call: %v", ErrPersistenceFailure, err)
		}

		sr.insertSessionEvent("model_call_start", map[string]interface{}{
			"round":         round,
			"model_call_id": modelCallRec.ID,
		})

		if sr.onModelCallHook != nil {
			sr.onModelCallHook(round)
		}

		// 2. Invoke model without holding DB locks
		modelTimeout := time.Duration(sr.budget.ModelTimeoutSeconds) * time.Second
		if modelTimeout <= 0 {
			modelTimeout = 30 * time.Second
		}
		modelCtx, cancelModel := context.WithTimeout(ctx, modelTimeout)
		resp, callErr := sr.provider.Call(modelCtx, messages, sr.registry.Definitions(), provider.ModelOpts{
			Timeout: modelTimeout,
		})
		cancelModel()

		if callErr != nil {
			errMsg := callErr.Error()
			var requestID, responseID *string
			if resp != nil {
				if resp.RequestID != "" {
					requestID = &resp.RequestID
				}
				if resp.ResponseID != "" {
					responseID = &resp.ResponseID
				}
			}
			_ = sr.runService.UpdateModelCallWithMetadata(context.Background(), modelCallRec.ID, "failed", nil, requestID, responseID, false, 0, 0, 0, &errMsg)
			sr.insertSessionEvent("model_call_failed", map[string]interface{}{
				"round": round,
				"error": errMsg,
			})

			if ctx.Err() != nil {
				reason := "user_requested_stop"
				if errors.Is(ctx.Err(), context.DeadlineExceeded) {
					reason = "timeout"
				}
				sr.updateSessionTerminal("stopped", reason)
				return &SessionResult{
					SessionID:    sr.sessionID,
					FinishReason: reason,
				}, ctx.Err()
			}

			reason := "provider_error"
			if errors.Is(callErr, provider.ErrProtocol) {
				reason = "protocol_error"
			}
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, fmt.Errorf("provider error: %w", callErr)
		}

		// Model call completed
		modelCallCount++
		var promptTokens, completionTokens, turnTokens int
		if resp.Usage != nil {
			promptTokens = resp.Usage.PromptTokens
			completionTokens = resp.Usage.CompletionTokens
			turnTokens = resp.Usage.TotalTokens
			totalTokens += turnTokens
		}
		finishReason := resp.FinishReason
		requestID := resp.RequestID
		responseID := resp.ResponseID

		if err := sr.runService.UpdateModelCallWithMetadata(context.Background(), modelCallRec.ID, "completed", &finishReason, &requestID, &responseID, resp.Usage != nil,
			promptTokens, completionTokens, turnTokens, nil); err != nil {
			slog.Error("session runner: failed to update model call", "session_id", sr.sessionID, "error", err)
			sr.updateSessionTerminal("failed", "persistence_error")
			return nil, fmt.Errorf("%w: update model call: %v", ErrPersistenceFailure, err)
		}

		sr.insertSessionEvent("model_call_finish", map[string]interface{}{
			"round":            round,
			"finish_reason":    finishReason,
			"tool_calls_count": len(resp.ToolCalls),
			"has_content":      resp.Content != "",
		})

		// 3. Persist assistant message
		var toolCallsJSON json.RawMessage
		if len(resp.ToolCalls) > 0 {
			toolCallsJSON, _ = json.Marshal(resp.ToolCalls)
		}
		var payload json.RawMessage
		if resp.NativePayload != nil {
			payload, _ = json.Marshal(resp.NativePayload)
		}
		if _, err := sr.runService.InsertAgentMessageWithPayload(
			context.Background(), sr.runID, &sr.sessionID, "assistant", resp.Content, nil, toolCallsJSON, payload); err != nil {
			slog.Error("session runner: failed to persist assistant message", "session_id", sr.sessionID, "error", err)
			sr.updateSessionTerminal("failed", "persistence_error")
			return nil, fmt.Errorf("%w: insert assistant message: %v", ErrPersistenceFailure, err)
		}

		messages = append(messages, provider.ChatMessage{
			Role:          provider.RoleAssistant,
			Content:       resp.Content,
			ToolCalls:     resp.ToolCalls,
			NativePayload: resp.NativePayload,
		})

		// Handle refusal and length truncation
		if resp.FinishReason == "refusal" {
			reason := "model_refused"
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}
		if resp.FinishReason == "length" {
			reason := "model_output_truncated"
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}

		// 4. Check if final answer (no tool calls requested)
		if len(resp.ToolCalls) == 0 {
			if strings.TrimSpace(resp.Content) == "" {
				reason := "protocol_error"
				sr.updateSessionTerminal("failed", reason)
				return &SessionResult{
					SessionID:       sr.sessionID,
					FinishReason:    reason,
					ModelCallsCount: modelCallCount,
					ToolCallsCount:  toolCallCount,
					TotalTokens:     totalTokens,
				}, nil
			}

			// Session successfully completed!
			reason := "completed"
			sr.updateSessionTerminal("completed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinalContent:    resp.Content,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}

		// 5. Tool call batch validation
		seenIDs := make(map[string]bool)
		protocolErr := false
		for _, tc := range resp.ToolCalls {
			if strings.TrimSpace(tc.ID) == "" || seenIDs[tc.ID] {
				protocolErr = true
				break
			}
			seenIDs[tc.ID] = true
		}
		if protocolErr {
			reason := "protocol_error"
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}

		// Check tool call budget
		if toolCallCount+len(resp.ToolCalls) > sr.budget.MaxToolCalls {
			reason := "max_tool_calls_exceeded"
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}

		// 6. Execute tool calls serially with strict persistence checks
		for _, tc := range resp.ToolCalls {
			select {
			case <-ctx.Done():
				reason := "user_requested_stop"
				sr.updateSessionTerminal("stopped", reason)
				return &SessionResult{
					SessionID:       sr.sessionID,
					FinishReason:    reason,
					ModelCallsCount: modelCallCount,
					ToolCallsCount:  toolCallCount,
					TotalTokens:     totalTokens,
				}, ctx.Err()
			default:
			}

			toolCallCount++
			argsBytes := []byte(tc.Function.Arguments)

			// Persistence check #1: If saving tool request fails, DO NOT execute tool!
			tcRec, err := sr.runService.CreateAgentToolCallWithSession(
				context.Background(), sr.runID, &sr.sessionID, &modelCallRec.ID, tc.ID, tc.Function.Name, argsBytes)
			if err != nil {
				slog.Error("session runner: failed to persist tool call, aborting tool execution",
					"session_id", sr.sessionID, "call_id", tc.ID, "error", err)
				sr.updateSessionTerminal("failed", "persistence_error")
				return nil, fmt.Errorf("%w: create tool call record: %v", ErrPersistenceFailure, err)
			}

			if sr.onToolCallHook != nil {
				sr.onToolCallHook(tc.ID, tc.Function.Name)
			}

			// Tool validation
			tool, valErr := sr.registry.Validate(tc.Function.Name, argsBytes)
			if valErr != nil {
				slog.Error("session runner: tool validation failed", "tool", tc.Function.Name, "error", valErr)
				errMsg := valErr.Error()
				now := time.Now()
				if err := sr.runService.UpdateAgentToolCall(
					context.Background(), tcRec.ID, "rejected", "", &errMsg, &now, &now); err != nil {
					slog.Error("session runner: failed to update rejected tool call", "session_id", sr.sessionID, "error", err)
					sr.updateSessionTerminal("failed", "persistence_error")
					return nil, fmt.Errorf("%w: update rejected tool call: %v", ErrPersistenceFailure, err)
				}

				sr.insertSessionEvent("tool_call_rejected", map[string]interface{}{
					"call_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"error":     errMsg,
				})

				toolErrOutput := fmt.Sprintf("Error: %s", errMsg)
				if _, err := sr.runService.InsertAgentMessageWithSession(
					context.Background(), sr.runID, &sr.sessionID, "tool", toolErrOutput, &tc.ID, nil); err != nil {
					slog.Error("session runner: failed to persist tool error message", "session_id", sr.sessionID, "error", err)
					sr.updateSessionTerminal("failed", "persistence_error")
					return nil, fmt.Errorf("%w: insert tool error message: %v", ErrPersistenceFailure, err)
				}

				messages = append(messages, provider.ChatMessage{
					Role:       provider.RoleTool,
					Content:    toolErrOutput,
					ToolCallID: tc.ID,
				})
				continue
			}

			// Execute tool outside DB transaction
			sr.insertSessionEvent("tool_call_start", map[string]interface{}{
				"call_id":   tc.ID,
				"tool_name": tc.Function.Name,
			})

			toolTimeout := time.Duration(sr.budget.ToolTimeoutSeconds) * time.Second
			if toolTimeout <= 0 {
				toolTimeout = 10 * time.Second
			}
			toolCtx, cancelTool := context.WithTimeout(ctx, toolTimeout)
			startTime := time.Now()
			out, execErr := tool.Execute(toolCtx, argsBytes)
			finishTime := time.Now()
			cancelTool()

			var status string
			var storedOutStr string
			var errMsg *string
			var toolMsgContent string

			if execErr != nil {
				slog.Error("session runner: tool execution failed", "tool", tc.Function.Name, "error", execErr)
				status = "failed"
				errStr := execErr.Error()
				errMsg = &errStr
				if len(out) > 0 {
					storedOutStr = string(out)
				}
				toolMsgContent = fmt.Sprintf("Error executing tool %s: %s", tc.Function.Name, errStr)
				sr.insertSessionEvent("tool_call_failed", map[string]interface{}{
					"call_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"error":     errStr,
				})
			} else {
				status = "success"
				storedOutStr = string(out)
				toolMsgContent = storedOutStr
				if len(toolMsgContent) > sr.budget.MaxToolOutputBytes {
					toolMsgContent = truncateUTF8(toolMsgContent, sr.budget.MaxToolOutputBytes) + "\n...[truncated: output exceeded model context budget limit]"
				}
				sr.insertSessionEvent("tool_call_success", map[string]interface{}{
					"call_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"bytes":     len(storedOutStr),
				})
			}

			// Persistence check #2: If saving tool result fails, DO NOT continue or claim completion!
			if err := sr.runService.UpdateAgentToolCall(
				context.Background(), tcRec.ID, status, storedOutStr, errMsg, &startTime, &finishTime); err != nil {
				slog.Error("session runner: failed to persist tool result, stopping execution",
					"session_id", sr.sessionID, "call_id", tc.ID, "error", err)
				sr.updateSessionTerminal("failed", "persistence_error")
				return nil, fmt.Errorf("%w: update tool result: %v", ErrPersistenceFailure, err)
			}

			// Persistence check #3: Save tool message to conversation
			if _, err := sr.runService.InsertAgentMessageWithSession(
				context.Background(), sr.runID, &sr.sessionID, "tool", toolMsgContent, &tc.ID, nil); err != nil {
				slog.Error("session runner: failed to persist tool message", "session_id", sr.sessionID, "error", err)
				sr.updateSessionTerminal("failed", "persistence_error")
				return nil, fmt.Errorf("%w: insert tool message: %v", ErrPersistenceFailure, err)
			}

			messages = append(messages, provider.ChatMessage{
				Role:       provider.RoleTool,
				Content:    toolMsgContent,
				ToolCallID: tc.ID,
			})
		}
	}
}

func (sr *SessionRunner) updateSessionTerminal(status string, finishReason string) {
	_ = sr.runService.UpdateSessionStatus(context.Background(), sr.sessionID, status, &finishReason)
}

func (sr *SessionRunner) insertSessionEvent(eventType string, payload map[string]interface{}) {
	payloadBytes, _ := json.Marshal(payload)
	_, _ = sr.runService.InsertRunEventWithContext(context.Background(), sr.runID, &sr.sessionID, nil, nil, eventType, payloadBytes)
}

func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	for maxBytes > 0 && maxBytes < len(s) && !utf8.RuneStart(s[maxBytes]) {
		maxBytes--
	}
	return s[:maxBytes]
}
