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
	limit := time.Duration(e.budget.SummaryTimeoutSeconds) * time.Second
	if limit <= 0 {
		limit = time.Duration(e.budget.ModelTimeoutSeconds) * time.Second
		if limit <= 0 {
			limit = 30 * time.Second
		}
	}
	if limit > remaining {
		limit = remaining
	}
	ctx, cancel := context.WithTimeout(e.executionCtx, limit)
	defer cancel()
	session, err := e.runService.CreateSession(ctx, e.runID, "planner", nil, nil, nil, nil)
	if err != nil {
		return fallback
	}
	budget := e.budget
	budget.MaxModelCalls = 1
	budget.ModelTimeoutSeconds = int((limit + time.Second - 1) / time.Second)
	budget.MaxToolCalls = 0
	runner := agent.NewSessionRunner(session.ID, e.runID, "planner", e.runService, e.provider, tools.NewRegistry(), budget,
		e.sessionOptions(
			agent.WithModelCallGate(agent.CallGateFunc(func() bool { return e.budgetMgr.AcquireModelCall(ctx, true) == nil })),
			agent.WithSessionModelCallFinishHook(func(_ int, _ error) { e.budgetMgr.ReleaseModelCall() }),
			agent.WithToolCallGate(agent.CallGateFunc(func() bool { return false })),
		)...,
	)
	messages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "Execution has stopped. Summarize only the persisted evidence inventory below. State unfinished coverage and the termination cause. Do not claim unobserved findings or request tools. Provide the summary in Simplified Chinese (简体中文)."},
		{Role: provider.RoleUser, Content: fallback},
	}
	type summaryResult struct {
		result *agent.SessionResult
		err    error
	}
	done := make(chan summaryResult, 1)
	go func() {
		result, err := runner.Run(ctx, messages)
		done <- summaryResult{result, err}
	}()
	// A provider that ignores cancellation cannot hold the terminal transaction
	// hostage. Late results remain subject to the existing database terminal guards.
	select {
	case <-ctx.Done():
		return fallback
	case completed := <-done:
		result := completed.result
		if ctx.Err() != nil || completed.err != nil || result == nil || result.FinishReason != "completed" || result.FinalContent == "" {
			return fallback
		}
		return result.FinalContent + "\n\n" + fallback
	}
}
