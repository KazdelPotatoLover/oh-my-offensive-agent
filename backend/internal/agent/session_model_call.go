package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
)

// modelInvocation is one admission cycle. Agent 0 merges this with Agent 1's gate.
type modelInvocation struct {
	resp    *provider.ModelResponse
	callErr error
	rec     *model.ModelCall
	round   int

	settlement bool
	denied     bool
	gate       BudgetGateResult
	denyReason string

	stopped    bool
	stopStatus string
	stopReason string
	stopErr    error
	persistErr error
}

type modelAdmission struct {
	allowed    bool
	settlement bool
	gate       BudgetGateResult
	denyReason string
}

// admitModelCall is the Agent 1 gate region. Each real Provider.Call must pass it.
func (sr *SessionRunner) admitModelCall(ctx context.Context, modelCallCount, attemptIndex int, inSettlement bool) modelAdmission {
	if sr.structuredGate != nil {
		gateRes := sr.structuredGate.TryAcquireModel(ctx, inSettlement)
		if !gateRes.Allowed {
			if gateRes.Scope == ScopeWorkerLocal && gateRes.IsSettlement && !inSettlement {
				return modelAdmission{settlement: true, gate: gateRes}
			}
			return modelAdmission{gate: gateRes}
		}
		return modelAdmission{allowed: true}
	}
	if sr.modelCallGate != nil {
		if !sr.modelCallGate.TryAcquire() {
			return modelAdmission{denyReason: "model_calls_exhausted"}
		}
		return modelAdmission{allowed: true}
	}
	if modelCallCount+attemptIndex >= sr.budget.MaxModelCalls {
		return modelAdmission{denyReason: "model_calls_exhausted"}
	}
	return modelAdmission{allowed: true}
}

// invokeModel performs limiter, admission, one Provider.Call, and optional bounded retry.
// Provider.Call itself never retries. The final failed row stays started for the caller.
func (sr *SessionRunner) invokeModel(ctx context.Context, messages []provider.ChatMessage, modelCallCount int, inSettlement bool) modelInvocation {
	policy := sr.recovery
	var lastErr error
	var lastRec *model.ModelCall
	var lastRound int
	for attempt := 1; attempt <= policy.Attempts(); attempt++ {
		if err := ctx.Err(); err != nil {
			return sr.stoppedInvocation(ctx, err)
		}
		release, waitErr := sr.acquireLimiter(ctx)
		if waitErr != nil {
			return sr.stoppedInvocation(ctx, waitErr)
		}
		admission := sr.admitModelCall(ctx, modelCallCount, attempt-1, inSettlement)
		if !admission.allowed {
			release()
			if lastErr != nil {
				return modelInvocation{callErr: lastErr, rec: lastRec, round: lastRound}
			}
			if admission.settlement {
				return modelInvocation{settlement: true}
			}
			return modelInvocation{denied: true, gate: admission.gate, denyReason: admission.denyReason}
		}
		sr.modelRoundSeq++
		round := sr.modelRoundSeq
		rec, err := persistSessionAdmission(ctx, func(dbCtx context.Context) (*model.ModelCall, error) {
			return sr.runService.CreateModelCallWithSession(dbCtx, sr.runID, &sr.sessionID, round)
		})
		if err != nil {
			release()
			if sr.onModelCallFinishHook != nil {
				sr.onModelCallFinishHook(round, err)
			}
			slog.Error("session runner: failed to create model call", "session_id", sr.sessionID, "error", err)
			sr.updateSessionTerminal("failed", "persistence_error")
			return modelInvocation{persistErr: fmt.Errorf("%w: create model call: %v", ErrPersistenceFailure, err)}
		}
		sr.insertSessionEvent("model_call_start", map[string]interface{}{
			"round":         round,
			"model_call_id": rec.ID,
			"attempt":       attempt,
		})
		if sr.onModelCallHook != nil {
			sr.onModelCallHook(round)
		}
		resp, callErr := sr.callProviderOnce(ctx, messages)
		release()
		if sr.onModelCallFinishHook != nil {
			sr.onModelCallFinishHook(round, callErr)
		}
		if callErr == nil {
			return modelInvocation{resp: resp, rec: rec, round: round}
		}
		callErr = provider.AnnotateSource(callErr, rec.ID.String())
		lastErr, lastRec, lastRound = callErr, rec, round
		if !policy.ShouldRetry(callErr) || attempt == policy.Attempts() || ctx.Err() != nil {
			return modelInvocation{resp: resp, callErr: callErr, rec: rec, round: round}
		}
		sr.failModelCall(rec.ID, round, callErr)
		if waitErr := waitBackoff(ctx, policy.Backoff(attempt, provider.RetryAfter(callErr))); waitErr != nil {
			return sr.stoppedInvocation(ctx, waitErr)
		}
	}
	return modelInvocation{callErr: lastErr, rec: lastRec, round: lastRound}
}

func (sr *SessionRunner) acquireLimiter(ctx context.Context) (func(), error) {
	if sr.limiter == nil {
		return func() {}, nil
	}
	if err := sr.limiter.Acquire(ctx); err != nil {
		return func() {}, err
	}
	return sr.limiter.Release, nil
}

func (sr *SessionRunner) callProviderOnce(ctx context.Context, messages []provider.ChatMessage) (*provider.ModelResponse, error) {
	modelTimeout := time.Duration(sr.budget.ModelTimeoutSeconds) * time.Second
	if modelTimeout <= 0 {
		modelTimeout = 30 * time.Second
	}
	modelCtx, cancel := context.WithTimeout(ctx, modelTimeout)
	defer cancel()
	if err := modelCtx.Err(); err != nil {
		return nil, err
	}
	return sr.provider.Call(modelCtx, messages, sr.registry.Definitions(), provider.ModelOpts{
		Timeout:         modelTimeout,
		MaxOutputTokens: sr.maxOutputTokens,
	})
}

func (sr *SessionRunner) failModelCall(id uuid.UUID, round int, callErr error) {
	errMsg := callErr.Error()
	_ = persistWrite(func(dbCtx context.Context) error {
		return sr.runService.UpdateModelCallWithMetadata(dbCtx, id, "failed", nil, nil, nil, false, 0, 0, 0, &errMsg)
	})
	sr.insertSessionEvent("model_call_retry", map[string]interface{}{
		"round": round,
		"error": errMsg,
	})
}

func (sr *SessionRunner) stoppedInvocation(ctx context.Context, err error) modelInvocation {
	reason := string(model.ResolveCancelCause(ctx))
	if reason == "" {
		reason = "timeout"
	}
	return modelInvocation{
		stopped:    true,
		stopStatus: cancellationSessionStatus(ctx),
		stopReason: reason,
		stopErr:    err,
	}
}

// restoreSavedToolHistory appends saved tool results and does not execute tools.
// Calls without a saved outcome stay open and are not replayed.
func (sr *SessionRunner) restoreSavedToolHistory(ctx context.Context, messages []provider.ChatMessage) ([]provider.ChatMessage, error) {
	if sr.savedOutcomes == nil {
		return messages, nil
	}
	restored, open := provider.RestoreSavedToolResults(messages, sr.savedOutcomes)
	for _, msg := range addedToolMessages(messages, restored) {
		callID := msg.ToolCallID
		sr.insertSessionEvent("tool_call_recovered", map[string]interface{}{
			"call_id":  callID,
			"replayed": false,
		})
		if _, err := persistSessionCall(ctx, func(dbCtx context.Context) (*model.AgentMessage, error) {
			return sr.runService.InsertAgentMessageWithSession(dbCtx, sr.runID, &sr.sessionID, "tool", msg.Content, &callID, nil)
		}); err != nil {
			sr.updateSessionTerminal("failed", "persistence_error")
			return nil, fmt.Errorf("%w: restore tool result: %v", ErrPersistenceFailure, err)
		}
	}
	for _, id := range open {
		sr.insertSessionEvent("tool_call_not_replayed", map[string]interface{}{
			"call_id":  id,
			"replayed": false,
		})
	}
	return restored, nil
}

func addedToolMessages(before, after []provider.ChatMessage) []provider.ChatMessage {
	var added []provider.ChatMessage
	j := 0
	for _, msg := range after {
		if j < len(before) && msg.Role == before[j].Role && msg.Content == before[j].Content && msg.ToolCallID == before[j].ToolCallID && len(msg.ToolCalls) == len(before[j].ToolCalls) {
			j++
			continue
		}
		added = append(added, msg)
	}
	return added
}

func waitBackoff(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}
