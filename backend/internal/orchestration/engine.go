package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

type EngineOption func(*Engine)

func WithOnPlannerRoundHook(hook func(round int)) EngineOption {
	return func(e *Engine) {
		e.onPlannerRoundHook = hook
	}
}

func WithOnWorkItemHook(hook func(key string)) EngineOption {
	return func(e *Engine) {
		e.onWorkItemHook = hook
	}
}

func WithWorkerActiveHook(hook func(delta int)) EngineOption {
	return func(e *Engine) {
		e.onWorkerActiveHook = hook
	}
}

// Engine coordinates Planner and Worker sessions for a multi-stage Run.
type Engine struct {
	runID              uuid.UUID
	runService         *service.RunService
	provider           provider.Provider
	budget             model.BudgetConfig
	maxWorkers         int
	onPlannerRoundHook func(round int)
	onWorkItemHook     func(key string)
	onWorkerActiveHook func(delta int)

	// Global budget tracking across Planner and all Workers
	globalModelCalls int32
	globalToolCalls  int32
}

func NewEngine(
	runID uuid.UUID,
	runService *service.RunService,
	p provider.Provider,
	budget model.BudgetConfig,
	opts ...EngineOption,
) *Engine {
	maxWorkers := budget.MaxParallelWorkers
	if maxWorkers <= 0 {
		maxWorkers = 1
	}
	eng := &Engine{
		runID:      runID,
		runService: runService,
		provider:   p,
		budget:     budget,
		maxWorkers: maxWorkers,
	}
	for _, opt := range opts {
		opt(eng)
	}
	return eng
}

func (e *Engine) Run(ctx context.Context) {
	defer e.ensureStoppingTerminal()

	// 1. Read Run from DB
	run, err := e.runService.Get(ctx, e.runID)
	if err != nil {
		slog.Error("engine: failed to get run", "run_id", e.runID, "error", err)
		return
	}

	// Early stop check
	if run.Status == model.RunStatusStopping {
		reason := "user_requested_stop"
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusStopping, model.RunStatusStopped, &reason)
		return
	}

	// 2. Transition queued -> running
	if run.Status == model.RunStatusQueued {
		_, err := e.runService.TransitionState(ctx, e.runID, model.RunStatusQueued, model.RunStatusRunning, nil)
		if err != nil {
			slog.Error("engine: failed to transition to running", "run_id", e.runID, "error", err)
			return
		}
	}

	// Parse input snapshot
	var inputMap map[string]interface{}
	if len(run.ExecutionInput) > 0 {
		json.Unmarshal(run.ExecutionInput, &inputMap)
	}
	objective, _ := inputMap["objective"].(string)
	if objective == "" {
		objective = "Analyze all available HTTP response samples and synthesize findings."
	}
	scopeBytes, _ := json.Marshal(inputMap["scope"])

	// Global execution timeout context
	runTimeout := time.Duration(e.budget.RunTimeoutSeconds) * time.Second
	if runTimeout <= 0 {
		runTimeout = 120 * time.Second
	}
	runCtx, cancelRun := context.WithTimeout(ctx, runTimeout)
	defer cancelRun()

	// 3. Round 1: Planner submits initial plan
	roundNum := 1
	if e.onPlannerRoundHook != nil {
		e.onPlannerRoundHook(roundNum)
	}

	prRound1, err := e.runService.CreatePlannerRound(ctx, e.runID, roundNum, "initial", nil)
	if err != nil {
		slog.Error("engine: failed to create planner round 1", "run_id", e.runID, "error", err)
		reason := "persistence_error"
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	sess1, err := e.runService.CreateSession(ctx, e.runID, "planner", &roundNum, nil, nil, run.ExecutionInput)
	if err != nil {
		slog.Error("engine: failed to create planner session 1", "run_id", e.runID, "error", err)
		reason := "persistence_error"
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}
	_ = e.runService.UpdatePlannerRound(ctx, prRound1.ID, "running", nil)

	var planSubmitted bool
	planTool := tools.NewSubmitPlanTool(e.runID, e.runService, func(items []model.WorkItem) {
		planSubmitted = true
	})
	plannerRegistry := tools.NewRegistry()
	plannerRegistry.Register(planTool)

	plannerPrompt := fmt.Sprintf(
		"You are the Lead Security Planner. Break down the task into discrete work items and submit a plan.\nObjective: %s\nScope: %s",
		objective, string(scopeBytes))
	plannerMessages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "You are the Lead Security Planner. Use submit_plan to create work items."},
		{Role: provider.RoleUser, Content: plannerPrompt},
	}

	srPlanner1 := agent.NewSessionRunner(
		sess1.ID, e.runID, "planner", e.runService, e.provider, plannerRegistry, e.budget,
		agent.WithSessionModelCallHook(func(r int) {
			atomic.AddInt32(&e.globalModelCalls, 1)
		}),
		agent.WithSessionToolCallHook(func(id, name string) {
			atomic.AddInt32(&e.globalToolCalls, 1)
		}),
	)
	pRes1, pErr1 := srPlanner1.Run(runCtx, plannerMessages)

	if runCtx.Err() != nil {
		e.handleCancellation(runCtx)
		return
	}
	if pErr1 != nil || pRes1 == nil || !planSubmitted {
		slog.Error("engine: planner round 1 failed to submit plan", "run_id", e.runID, "error", pErr1)
		reason := "planner_failed"
		_ = e.runService.UpdatePlannerRound(context.Background(), prRound1.ID, "failed", nil)
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}
	planSummary := "Initial plan submitted"
	_ = e.runService.UpdatePlannerRound(context.Background(), prRound1.ID, "completed", &planSummary)

	// 4. Worker Execution Phase (support single or dual workers)
	workCompletedChan := make(chan struct{}, 100)
	var activeWorkers sync.WaitGroup
	workerTokens := make(chan struct{}, e.maxWorkers)
	for i := 0; i < e.maxWorkers; i++ {
		workerTokens <- struct{}{}
	}

	var workerErr atomic.Value

workerDispatchLoop:
	for {
		select {
		case <-runCtx.Done():
			e.handleCancellation(runCtx)
			activeWorkers.Wait()
			return
		default:
		}

		// Check global budget limits
		if atomic.LoadInt32(&e.globalModelCalls) >= int32(e.budget.MaxModelCalls) {
			reason := "model_calls_exhausted"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return
		}
		if atomic.LoadInt32(&e.globalToolCalls) >= int32(e.budget.MaxToolCalls) {
			reason := "tool_calls_exhausted"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return
		}

		// Try acquiring worker concurrency token
		select {
		case token := <-workerTokens:
			// Claim next pending work item
			wi, att, sess, claimErr := e.runService.ClaimNextPendingWorkItem(runCtx, e.runID)
			if claimErr != nil {
				workerTokens <- token
				slog.Error("engine: claim work item error", "run_id", e.runID, "error", claimErr)
				reason := "claim_failed"
				e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
				activeWorkers.Wait()
				return
			}

			if wi != nil && att != nil && sess != nil {
				// Successfully claimed! Launch worker goroutine
				activeWorkers.Add(1)
				if e.onWorkItemHook != nil {
					e.onWorkItemHook(wi.Key)
				}

				go func(item *model.WorkItem, attempt *model.Attempt, s *model.AgentSession, tok struct{}) {
					if e.onWorkerActiveHook != nil {
						e.onWorkerActiveHook(1)
					}
					defer func() {
						if e.onWorkerActiveHook != nil {
							e.onWorkerActiveHook(-1)
						}
						workerTokens <- tok
						activeWorkers.Done()
						workCompletedChan <- struct{}{}
					}()

					e.executeWorkerSession(runCtx, item, attempt, s, &workerErr)
				}(wi, att, sess, token)

				continue
			}

			// Nothing was ready to claim, return token
			workerTokens <- token

		default:
			// All worker slots currently busy, wait for a worker to finish
		}

		// Check status of all items in this Run
		items, err := e.runService.ListWorkItemsByRun(runCtx, e.runID)
		if err != nil {
			slog.Error("engine: failed to list work items", "run_id", e.runID, "error", err)
			reason := "persistence_error"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return
		}

		hasRunning := false
		hasBlocked := false
		allDone := true

		for _, it := range items {
			if it.Status == model.WorkItemStatusRunning {
				hasRunning = true
				allDone = false
			} else if it.Status == model.WorkItemStatusPending {
				allDone = false
			} else if it.Status == model.WorkItemStatusBlocked {
				hasBlocked = true
				allDone = false
			} else if it.Status == model.WorkItemStatusFailed {
				hasBlocked = true
				allDone = false
			}
		}

		if allDone {
			// All work items finished! Proceed to final evaluation
			break workerDispatchLoop
		}

		if hasBlocked && !hasRunning {
			// Stuck due to failed/blocked prerequisites
			slog.Warn("engine: run blocked due to failed work item dependencies", "run_id", e.runID)
			reason := "work_item_blocked"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return
		}

		// Wait for next work completion notification or cancellation
		select {
		case <-runCtx.Done():
			e.handleCancellation(runCtx)
			activeWorkers.Wait()
			return
		case <-workCompletedChan:
			// A worker completed, check loop again!
		case <-time.After(100 * time.Millisecond):
			// Poll safeguard
		}
	}

	activeWorkers.Wait()

	// Check if any worker reported terminal failure
	if wErrVal := workerErr.Load(); wErrVal != nil {
		slog.Error("engine: worker reported fatal failure", "run_id", e.runID, "error", wErrVal)
		reason := "worker_failed"
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	// 5. Final Planner Round (Evaluation & Completion)
	roundNum = 2
	if e.onPlannerRoundHook != nil {
		e.onPlannerRoundHook(roundNum)
	}

	prRound2, err := e.runService.CreatePlannerRound(ctx, e.runID, roundNum, "all_work_completed", nil)
	if err != nil {
		slog.Error("engine: failed to create planner round 2", "run_id", e.runID, "error", err)
		reason := "persistence_error"
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	sess2, err := e.runService.CreateSession(ctx, e.runID, "planner", &roundNum, nil, nil, nil)
	if err != nil {
		slog.Error("engine: failed to create planner session 2", "run_id", e.runID, "error", err)
		reason := "persistence_error"
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}
	_ = e.runService.UpdatePlannerRound(ctx, prRound2.ID, "running", nil)

	// Gather all observations for final review
	obsList, _ := e.runService.ListObservationsByRun(ctx, e.runID)
	obsSummary := fmt.Sprintf("All %d work items completed. Gathered %d observations:\n", len(inputMap), len(obsList))
	for _, obs := range obsList {
		obsSummary += fmt.Sprintf("- [%s] %s\n", obs.ObsType, obs.Content)
	}

	var finalSummaryResult string
	var finalConclusion string
	var completionSuggested bool
	suggestTool := tools.NewSuggestCompletionTool(e.runID, e.runService, func(summary, conclusion string) {
		completionSuggested = true
		finalSummaryResult = summary
		finalConclusion = conclusion
	})
	evalRegistry := tools.NewRegistry()
	evalRegistry.Register(suggestTool)

	evalPrompt := fmt.Sprintf(
		"All work items have finished. Review observations and call suggest_completion to finish the run.\n%s",
		obsSummary)
	evalMessages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "You are the Lead Security Planner. Call suggest_completion with final report."},
		{Role: provider.RoleUser, Content: evalPrompt},
	}

	srPlanner2 := agent.NewSessionRunner(
		sess2.ID, e.runID, "planner", e.runService, e.provider, evalRegistry, e.budget,
		agent.WithSessionModelCallHook(func(r int) {
			atomic.AddInt32(&e.globalModelCalls, 1)
		}),
		agent.WithSessionToolCallHook(func(id, name string) {
			atomic.AddInt32(&e.globalToolCalls, 1)
		}),
	)
	pRes2, pErr2 := srPlanner2.Run(runCtx, evalMessages)

	if runCtx.Err() != nil {
		e.handleCancellation(runCtx)
		return
	}
	if pErr2 != nil || pRes2 == nil || !completionSuggested {
		slog.Error("engine: planner round 2 failed to suggest completion", "run_id", e.runID, "error", pErr2)
		reason := "planner_failed"
		_ = e.runService.UpdatePlannerRound(context.Background(), prRound2.ID, "failed", nil)
		e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	_ = e.runService.UpdatePlannerRound(context.Background(), prRound2.ID, "completed", &finalSummaryResult)

	// Finalize Run successfully with final answer!
	if finalSummaryResult == "" {
		finalSummaryResult = pRes2.FinalContent
	}
	if finalSummaryResult == "" {
		finalSummaryResult = "Execution finished with conclusion: " + finalConclusion
	}

	_, err = e.runService.CompleteRunWithOutput(context.Background(), e.runID, "completed", finalSummaryResult)
	if err != nil {
		slog.Error("engine: failed to complete run with output", "run_id", e.runID, "error", err)
	}
}

func (e *Engine) executeWorkerSession(
	ctx context.Context,
	wi *model.WorkItem,
	att *model.Attempt,
	sess *model.AgentSession,
	workerErr *atomic.Value,
) {
	// Construct worker tools based on work_type
	workerRegistry := tools.NewRegistry()
	if wi.WorkType == "aggregate_summary" {
		workerRegistry.Register(tools.NewReadObservationsTool(e.runID, e.runService))
	} else {
		workerRegistry.Register(tools.NewReadSampleTool())
		workerRegistry.Register(tools.NewRecordObservationTool(e.runID, att.ID, e.runService))
	}

	workerPrompt := fmt.Sprintf(
		"Work Item: %s\nTitle: %s\nObjective: %s\nInput Data: %s",
		wi.Key, wi.Title, wi.Objective, string(wi.InputData))

	workerMessages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: fmt.Sprintf("You are Worker for task %s (%s). Execute tools and submit results.", wi.Key, wi.WorkType)},
		{Role: provider.RoleUser, Content: workerPrompt},
	}

	sr := agent.NewSessionRunner(
		sess.ID, e.runID, "worker", e.runService, e.provider, workerRegistry, e.budget,
		agent.WithSessionModelCallHook(func(r int) {
			atomic.AddInt32(&e.globalModelCalls, 1)
		}),
		agent.WithSessionToolCallHook(func(id, name string) {
			atomic.AddInt32(&e.globalToolCalls, 1)
		}),
	)

	res, err := sr.Run(ctx, workerMessages)
	if ctx.Err() != nil {
		_ = e.runService.FinishWorkItem(context.Background(), wi.ID, att.ID, sess.ID, false, nil, "user_requested_stop")
		return
	}

	if err != nil || res == nil || res.FinishReason != "completed" {
		finishReason := "worker_failed"
		if res != nil && res.FinishReason != "" {
			finishReason = res.FinishReason
		}
		_ = e.runService.FinishWorkItem(context.Background(), wi.ID, att.ID, sess.ID, false, nil, finishReason)
		return
	}

	outputMap := map[string]interface{}{
		"summary": res.FinalContent,
	}
	outBytes, _ := json.Marshal(outputMap)
	_ = e.runService.FinishWorkItem(context.Background(), wi.ID, att.ID, sess.ID, true, outBytes, "completed")
}

func (e *Engine) handleCancellation(ctx context.Context) {
	cleanupCtx := context.Background()
	run, err := e.runService.Get(cleanupCtx, e.runID)
	if err != nil || run.Status.IsTerminal() {
		return
	}

	reason := "user_requested_stop"
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		reason = "timeout"
	}
	_, _ = e.runService.TransitionState(cleanupCtx, e.runID, run.Status, model.RunStatusStopped, &reason)
}

func (e *Engine) ensureStoppingTerminal() {
	cleanupCtx := context.Background()
	run, err := e.runService.Get(cleanupCtx, e.runID)
	if err != nil {
		return
	}
	if run.Status == model.RunStatusStopping {
		reason := "user_requested_stop"
		_, _ = e.runService.TransitionState(cleanupCtx, e.runID, model.RunStatusStopping, model.RunStatusStopped, &reason)
	}
}
