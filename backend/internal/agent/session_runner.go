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

func WithStructuredCallGate(gate StructuredCallGate) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.structuredGate = gate
	}
}

func WithBeforeTurnMessagesPersisted(persisted bool) SessionRunnerOption {
	return func(r *SessionRunner) { r.beforeTurnMessagesPersisted = persisted }
}

func WithInitialMessagesPersisted(persisted bool) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.initialMessagesPersisted = persisted
	}
}

type CallGate interface {
	TryAcquire() bool
}

type CallGateFunc func() bool

func (f CallGateFunc) TryAcquire() bool {
	return f()
}

func WithModelCallGate(gate CallGate) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.modelCallGate = gate
	}
}

func WithToolCallGate(gate CallGate) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.toolCallGate = gate
	}
}

func WithSessionModelCallHook(hook func(round int)) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.onModelCallHook = hook
	}
}

func WithSessionModelCallFinishHook(hook func(round int, err error)) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.onModelCallFinishHook = hook
	}
}

func WithSessionToolCallHook(hook func(callID string, toolName string)) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.onToolCallHook = hook
	}
}

// BeforeTurnHook is invoked before each turn. It can block on pause barriers, or return
// dynamic steering messages to be appended and persisted before the LLM call.
type BeforeTurnHook func(ctx context.Context, turn int) ([]provider.ChatMessage, error)

func WithBeforeTurnHook(hook BeforeTurnHook) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.beforeTurnHook = hook
	}
}

// BeforeToolHook is invoked immediately before each tool call executes. It can block on pause barriers.
type BeforeToolHook func(ctx context.Context, toolName string) error

func WithBeforeToolHook(hook BeforeToolHook) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.beforeToolHook = hook
	}
}

// WithModelRecovery enables bounded re-requests. The zero value performs one call.
func WithModelRecovery(policy provider.RecoveryPolicy) SessionRunnerOption {
	return func(r *SessionRunner) { r.recovery = policy }
}

// WithRequestLimiter shares the upstream in-flight limit across roles. It is not the worker pool.
func WithRequestLimiter(limiter *provider.RequestLimiter) SessionRunnerOption {
	return func(r *SessionRunner) { r.limiter = limiter }
}

// WithSavedOutcomes restores persisted tool results and does not execute those call IDs again.
func WithSavedOutcomes(outcomes map[string]provider.SavedToolOutcome) SessionRunnerOption {
	return func(r *SessionRunner) { r.savedOutcomes = outcomes }
}

// WithMaxOutputTokens sets an explainable output cap. Zero uses the provider default.
func WithMaxOutputTokens(tokens int) SessionRunnerOption {
	return func(r *SessionRunner) { r.maxOutputTokens = tokens }
}

// SessionRunner executes a single multi-turn conversational tool-calling session.
type SessionRunner struct {
	sessionID                   uuid.UUID
	runID                       uuid.UUID
	role                        string
	runService                  *service.RunService
	provider                    provider.Provider
	registry                    *tools.Registry
	budget                      model.BudgetConfig
	modelCallGate               CallGate
	toolCallGate                CallGate
	structuredGate              StructuredCallGate
	initialMessagesPersisted    bool
	beforeTurnMessagesPersisted bool
	beforeTurnHook              BeforeTurnHook
	beforeToolHook              BeforeToolHook
	onModelCallHook             func(round int)
	onModelCallFinishHook       func(round int, err error)
	onToolCallHook              func(callID string, toolName string)
	recovery                    provider.RecoveryPolicy
	limiter                     *provider.RequestLimiter
	savedOutcomes               map[string]provider.SavedToolOutcome
	maxOutputTokens             int
	modelRoundSeq               int
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
	// Persist initial messages with session_id if not already persisted
	if !sr.initialMessagesPersisted {
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
			_, err := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
				return sr.runService.InsertAgentMessageWithPayload(
					dbCtx, sr.runID, &sr.sessionID, string(msg.Role), msg.Content, toolCallID, toolCallsJSON, payload)
			})
			if err != nil {
				slog.Error("session runner: failed to persist initial message", "session_id", sr.sessionID, "error", err)
				sr.updateSessionTerminal("failed", "persistence_error")
				return nil, fmt.Errorf("%w: insert initial message: %v", ErrPersistenceFailure, err)
			}
		}
	}

	messages := make([]provider.ChatMessage, len(initialMessages))
	copy(messages, initialMessages)
	var restoreErr error
	messages, restoreErr = sr.restoreSavedToolHistory(ctx, messages)
	if restoreErr != nil {
		return nil, restoreErr
	}

	modelCallCount := 0
	toolCallCount := 0
	totalTokens := 0
	inSettlement := false

	for {
		// Check context cancellation
		select {
		case <-ctx.Done():
			reason := string(model.ResolveCancelCause(ctx))
			sr.updateSessionTerminal(cancellationSessionStatus(ctx), reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, ctx.Err()
		default:
		}

		// Invoke before turn hook (e.g. pause barrier check or operator steering injection)
		if sr.beforeTurnHook != nil {
			injectedMsgs, err := sr.beforeTurnHook(ctx, modelCallCount+1)
			if err != nil {
				reason := "hook_cancelled"
				if errors.Is(err, context.Canceled) {
					reason = string(model.ResolveCancelCause(ctx))
				}
				sr.updateSessionTerminal(cancellationSessionStatus(ctx), reason)
				return nil, err
			}
			for _, injMsg := range injectedMsgs {
				messages = append(messages, injMsg)
				if !sr.beforeTurnMessagesPersisted {
					_, _ = persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
						return sr.runService.InsertAgentMessageWithSession(
							dbCtx, sr.runID, &sr.sessionID, string(injMsg.Role), injMsg.Content, nil, nil)
					})
				}
			}
		}

		view, summary, compacted, compactErr := provider.CompactConversation(messages, sr.budget.ContextWindowBytes)
		if compactErr != nil {
			sr.insertSessionEvent("context_compaction_failed", map[string]any{"error": compactErr.Error()})
			sr.updateSessionTerminal("failed", "context_compaction_failed")
			return nil, compactErr
		}
		if compacted {
			if err := sr.runService.SaveContextCheckpoint(ctx, sr.runID, sr.sessionID, view, summary); err != nil {
				sr.updateSessionTerminal("failed", "checkpoint_persistence_error")
				return nil, err
			}
			messages = view
			sr.insertSessionEvent("context_compacted", map[string]any{"strategy": "extractive_protocol_blocks_v1"})
		}

		// Admission, limiter, record, and one Provider.Call. Retries stay in this loop.
		invocation := sr.invokeModel(ctx, messages, modelCallCount, inSettlement)
		if invocation.settlement {
			inSettlement = true
			settleMsg := provider.ChatMessage{
				Role:    provider.RoleUser,
				Content: "【局部探索额度耗尽】本轮工作局部额度已耗尽，已转入有界收尾阶段。请对已获得的发现/观察进行整理和总结，保存有效事实，切勿发起新的探测或请求。",
			}
			messages = append(messages, settleMsg)
			_, _ = persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
				return sr.runService.InsertAgentMessageWithSession(dbCtx, sr.runID, &sr.sessionID, string(settleMsg.Role), settleMsg.Content, nil, nil)
			})
			continue
		}
		if invocation.persistErr != nil {
			return nil, invocation.persistErr
		}
		if invocation.stopped {
			sr.updateSessionTerminal(invocation.stopStatus, invocation.stopReason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    invocation.stopReason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, invocation.stopErr
		}
		if invocation.denied {
			reason := invocation.denyReason
			status := "exhausted"
			if reason == "" {
				reason = string(invocation.gate.Cause)
				if invocation.gate.Scope == ScopeGlobal && (invocation.gate.Cause == model.CauseTimeout || invocation.gate.Cause == model.CauseUserStop) {
					status = "stopped"
				}
			}
			sr.updateSessionTerminal(status, reason)
			return &SessionResult{
				SessionID:       sr.sessionID,
				FinishReason:    reason,
				ModelCallsCount: modelCallCount,
				ToolCallsCount:  toolCallCount,
				TotalTokens:     totalTokens,
			}, nil
		}
		resp := invocation.resp
		callErr := invocation.callErr
		modelCallRec := invocation.rec
		round := invocation.round

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
			_ = persistWrite(func(dbCtx context.Context) error {
				return sr.runService.UpdateModelCallWithMetadata(dbCtx, modelCallRec.ID, "failed", nil, requestID, responseID, false, 0, 0, 0, &errMsg)
			})
			sr.insertSessionEvent("model_call_failed", map[string]interface{}{
				"round": round,
				"error": errMsg,
			})

			if ctx.Err() != nil {
				reason := string(model.ResolveCancelCause(ctx))
				sr.updateSessionTerminal(cancellationSessionStatus(ctx), reason)
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

		// A response arriving after cancellation must not start tools or revive state.
		if ctx.Err() != nil {
			reason := string(model.ResolveCancelCause(ctx))
			_ = persistWrite(func(dbCtx context.Context) error {
				return sr.runService.UpdateModelCallWithMetadata(dbCtx, modelCallRec.ID, "interrupted", nil, nil, nil, false, 0, 0, 0, &reason)
			})
			sr.updateSessionTerminal(cancellationSessionStatus(ctx), reason)
			return &SessionResult{SessionID: sr.sessionID, FinishReason: reason}, ctx.Err()
		}
		if resp == nil {
			reason := "provider_error"
			_ = persistWrite(func(dbCtx context.Context) error {
				return sr.runService.UpdateModelCallWithMetadata(dbCtx, modelCallRec.ID, "failed", nil, nil, nil, false, 0, 0, 0, &reason)
			})
			sr.updateSessionTerminal("failed", reason)
			return &SessionResult{SessionID: sr.sessionID, FinishReason: reason}, errors.New("provider returned nil response")
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

		if err := persistWrite(func(dbCtx context.Context) error {
			return sr.runService.UpdateModelCallWithMetadata(dbCtx, modelCallRec.ID, "completed", &finishReason, &requestID, &responseID, resp.Usage != nil,
				promptTokens, completionTokens, turnTokens, nil)
		}); err != nil {
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
		if _, err := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
			return sr.runService.InsertAgentMessageWithPayload(
				dbCtx, sr.runID, &sr.sessionID, "assistant", resp.Content, nil, toolCallsJSON, payload)
		}); err != nil {
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

			// Session successfully completed or concluded settlement
			reason := "completed"
			status := "completed"
			if inSettlement || resp.FinishReason == "worker_exhausted" {
				reason = string(model.CauseWorkerSettlementComplete)
				if resp.FinishReason == "worker_exhausted" {
					reason = "worker_exhausted"
				}
				status = "exhausted"
			}
			sr.updateSessionTerminal(status, reason)
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
		if sr.toolCallGate == nil && toolCallCount+len(resp.ToolCalls) > sr.budget.MaxToolCalls {
			reason := "tool_calls_exhausted"
			sr.updateSessionTerminal("exhausted", reason)
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
				reason := string(model.ResolveCancelCause(ctx))
				sr.updateSessionTerminal(cancellationSessionStatus(ctx), reason)
				return &SessionResult{
					SessionID:       sr.sessionID,
					FinishReason:    reason,
					ModelCallsCount: modelCallCount,
					ToolCallsCount:  toolCallCount,
					TotalTokens:     totalTokens,
				}, ctx.Err()
			default:
			}

			if saved, ok := sr.savedOutcomes[tc.ID]; ok {
				content := saved.Content
				if strings.TrimSpace(content) == "" {
					content = `{"status":"outcome_unknown","recovered_result":true,"instruction":"do not automatically replay the external action"}`
				}
				sr.insertSessionEvent("tool_call_recovered", map[string]interface{}{
					"call_id":  tc.ID,
					"status":   saved.Status,
					"replayed": false,
				})
				if _, err := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
					return sr.runService.InsertAgentMessageWithSession(dbCtx, sr.runID, &sr.sessionID, "tool", content, &tc.ID, nil)
				}); err != nil {
					sr.updateSessionTerminal("failed", "persistence_error")
					return nil, fmt.Errorf("%w: restore tool result: %v", ErrPersistenceFailure, err)
				}
				messages = append(messages, provider.ChatMessage{Role: provider.RoleTool, Content: content, ToolCallID: tc.ID})
				continue
			}

			// Invoke before tool hook (e.g. pause barrier check before any tool executes)
			if sr.beforeToolHook != nil {
				if hookErr := sr.beforeToolHook(ctx, tc.Function.Name); hookErr != nil {
					reason := string(model.ResolveCancelCause(ctx))
					sr.updateSessionTerminal(cancellationSessionStatus(ctx), reason)
					return &SessionResult{
						SessionID:       sr.sessionID,
						FinishReason:    reason,
						ModelCallsCount: modelCallCount,
						ToolCallsCount:  toolCallCount,
						TotalTokens:     totalTokens,
					}, hookErr
				}
			}

			// Check tool call gate if configured
			if sr.structuredGate != nil {
				gateRes := sr.structuredGate.TryAcquireTool(ctx, tc.Function.Name, inSettlement)
				if !gateRes.Allowed {
					if inSettlement && gateRes.Cause == model.CauseWorkerSettlementComplete {
						now := time.Now()
						errMsg := "settlement phase active: exploratory tools not permitted during wrapup"
						argsBytes := []byte(tc.Function.Arguments)
						tcRec, _ := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentToolCall, error) {
							return sr.runService.CreateAgentToolCallWithSession(
								dbCtx, sr.runID, &sr.sessionID, &modelCallRec.ID, tc.ID, tc.Function.Name, argsBytes)
						})
						if tcRec != nil {
							_ = persistWrite(func(dbCtx context.Context) error {
								return sr.runService.UpdateAgentToolCall(dbCtx, tcRec.ID, "rejected", "", &errMsg, &now, &now)
							})
						}
						toolErrOutput := fmt.Sprintf("Error: %s", errMsg)
						_, _ = persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
							return sr.runService.InsertAgentMessageWithSession(dbCtx, sr.runID, &sr.sessionID, "tool", toolErrOutput, &tc.ID, nil)
						})
						messages = append(messages, provider.ChatMessage{
							Role:       provider.RoleTool,
							Content:    toolErrOutput,
							ToolCallID: tc.ID,
						})
						continue
					}
					reason := string(gateRes.Cause)
					sr.updateSessionTerminal("exhausted", reason)
					return &SessionResult{
						SessionID:       sr.sessionID,
						FinishReason:    reason,
						ModelCallsCount: modelCallCount,
						ToolCallsCount:  toolCallCount,
						TotalTokens:     totalTokens,
					}, nil
				}
			} else if sr.toolCallGate != nil {
				if !sr.toolCallGate.TryAcquire() {
					reason := "tool_calls_exhausted"
					sr.updateSessionTerminal("exhausted", reason)
					return &SessionResult{
						SessionID:       sr.sessionID,
						FinishReason:    reason,
						ModelCallsCount: modelCallCount,
						ToolCallsCount:  toolCallCount,
						TotalTokens:     totalTokens,
					}, nil
				}
			}

			toolCallCount++
			argsBytes := []byte(tc.Function.Arguments)

			// Persistence check #1: If saving tool request fails, DO NOT execute tool!
			tcRec, err := persistSessionAdmission(ctx, func(dbCtx context.Context) (*model.AgentToolCall, error) {
				return sr.runService.CreateAgentToolCallWithSession(
					dbCtx, sr.runID, &sr.sessionID, &modelCallRec.ID, tc.ID, tc.Function.Name, argsBytes)
			})
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
				if err := persistWrite(func(dbCtx context.Context) error {
					return sr.runService.UpdateAgentToolCall(
						dbCtx, tcRec.ID, "rejected", "", &errMsg, &now, &now)
				}); err != nil {
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
				if _, err := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
					return sr.runService.InsertAgentMessageWithSession(
						dbCtx, sr.runID, &sr.sessionID, "tool", toolErrOutput, &tc.ID, nil)
				}); err != nil {
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
				if tc.Function.Name == "get_endpoint" || tc.Function.Name == "read_sample" {
					toolMsgContent = evidenceToolMessage(storedOutStr, tcRec.ID.String(), tc.ID, sr.budget.MaxToolOutputBytes)
				} else if len(toolMsgContent) > sr.budget.MaxToolOutputBytes {
					toolMsgContent = truncateUTF8(toolMsgContent, sr.budget.MaxToolOutputBytes) + "\n...[truncated: output exceeded model context budget limit]"
				}
				sr.insertSessionEvent("tool_call_success", map[string]interface{}{
					"call_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"bytes":     len(storedOutStr),
				})
			}

			// Persistence check #2: If saving tool result fails, DO NOT continue or claim completion!
			if err := persistWrite(func(dbCtx context.Context) error {
				return sr.runService.UpdateAgentToolCall(
					dbCtx, tcRec.ID, status, storedOutStr, errMsg, &startTime, &finishTime)
			}); err != nil {
				slog.Error("session runner: failed to persist tool result, stopping execution",
					"session_id", sr.sessionID, "call_id", tc.ID, "error", err)
				sr.updateSessionTerminal("failed", "persistence_error")
				return nil, fmt.Errorf("%w: update tool result: %v", ErrPersistenceFailure, err)
			}

			if storedOutStr != "" && tc.Function.Name != "read_artifact" && tc.Function.Name != "get_worker_output" && tc.Function.Name != "list_artifacts" {
				artifact, err := sr.runService.SaveToolArtifact(ctx, sr.runID, sr.sessionID, tcRec.ID, "tool-output.txt", storedOutStr)
				if err != nil {
					sr.updateSessionTerminal("failed", "artifact_persistence_error")
					return nil, err
				}
				var object map[string]any
				if json.Unmarshal([]byte(toolMsgContent), &object) == nil && object != nil {
					object["_artifact"] = map[string]any{"id": artifact.ID, "total_bytes": artifact.ByteLength, "sha256": artifact.SHA256, "read_tool": "read_artifact"}
					encoded, _ := json.Marshal(object)
					toolMsgContent = string(encoded)
				} else {
					toolMsgContent += fmt.Sprintf("\nFull output: artifact_id=%s; read_artifact supports offset/limit.", artifact.ID)
				}
			}

			// Persistence check #3: Save tool message to conversation
			if _, err := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
				return sr.runService.InsertAgentMessageWithSession(
					dbCtx, sr.runID, &sr.sessionID, "tool", toolMsgContent, &tc.ID, nil)
			}); err != nil {
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
	_ = persistWrite(func(dbCtx context.Context) error {
		return sr.runService.UpdateSessionStatus(dbCtx, sr.sessionID, status, &finishReason)
	})
}

func (sr *SessionRunner) insertSessionEvent(eventType string, payload map[string]interface{}) {
	payloadBytes, _ := json.Marshal(payload)
	_, _ = persistCall(func(dbCtx context.Context) (*model.RunEvent, error) {
		return sr.runService.InsertRunEventWithContext(dbCtx, sr.runID, &sr.sessionID, nil, nil, eventType, payloadBytes)
	})
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

func cancellationSessionStatus(ctx context.Context) string {
	if model.ResolveCancelCause(ctx) == model.CauseShutdown {
		return "interrupted"
	}
	return "stopped"
}

// Persistence must survive execution cancellation long enough to record its
// outcome, while retaining a short independent timeout for every database call.
func persistSessionAdmission[T any](ctx context.Context, operation func(context.Context) (T, error)) (T, error) {
	for {
		value, err := persistSessionCall(ctx, operation)
		if !errors.Is(err, service.ErrRunPaused) {
			return value, err
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func persistSessionCall[T any](parent context.Context, operation func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 3*time.Second)
	defer cancel()
	return operation(ctx)
}

func persistCall[T any](operation func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return operation(ctx)
}

func persistWrite(operation func(context.Context) error) error {
	_, err := persistCall(func(ctx context.Context) (struct{}, error) { return struct{}{}, operation(ctx) })
	return err
}
