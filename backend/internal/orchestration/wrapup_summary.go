package orchestration

import (
	"context"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/tools"
)

// summarizeWithReservedCall allows exactly one tool-free summary request, within
// both the recorded total call budget and the original execution deadline.
// The deterministic evidence inventory is retained even if the model fails.
func (e *Engine) summarizeWithReservedCall(cleanupCtx context.Context, cause model.TerminationCause, fallback string) string {
	if e.executionCtx == nil || e.executionCtx.Err() != nil || !e.budgetMgr.HasWrapupQuota() {
		return fallback
	}
	run, err := e.runService.Get(cleanupCtx, e.runID)
	if err != nil || run.Status != model.RunStatusRunning {
		return fallback
	}
	remaining := e.budgetMgr.RemainingTime()
	if remaining <= 0 {
		return fallback
	}
	limit := time.Duration(e.budget.ModelTimeoutSeconds) * time.Second
	if limit <= 0 {
		limit = 30 * time.Second
	}
	if limit > 5*time.Second {
		limit = 5 * time.Second
	}
	if limit > remaining {
		limit = remaining
	}
	ctx, cancel := context.WithTimeout(e.executionCtx, limit)
	defer cancel()
	// Also bound the request by the coordinator's cleanup allowance.
	stop := context.AfterFunc(cleanupCtx, cancel)
	defer stop()
	session, err := e.runService.CreateSession(ctx, e.runID, "planner", nil, nil, nil, nil)
	if err != nil {
		return fallback
	}
	budget := e.budget
	budget.MaxModelCalls = 1
	budget.MaxToolCalls = 0
	runner := agent.NewSessionRunner(session.ID, e.runID, "planner", e.runService, e.provider, tools.NewRegistry(), budget,
		agent.WithModelCallGate(agent.CallGateFunc(func() bool { return e.budgetMgr.AcquireModelCall(ctx, true) == nil })),
		agent.WithSessionModelCallFinishHook(func(_ int, _ error) { e.budgetMgr.ReleaseModelCall() }),
		agent.WithToolCallGate(agent.CallGateFunc(func() bool { return false })),
	)
	result, err := runner.Run(ctx, []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "Execution has stopped. Summarize only the persisted evidence inventory below. State unfinished coverage and the termination cause. Do not claim unobserved findings or request tools."},
		{Role: provider.RoleUser, Content: fallback},
	})
	if err != nil || result == nil || result.FinishReason != "completed" || result.FinalContent == "" {
		return fallback
	}
	return result.FinalContent + "\n\n" + fallback
}
