package orchestration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	planTool := tools.NewSubmitPlanToolWithRound(e.runID, &prRound1.ID, e.runService, func(items []model.WorkItem) {
		planSubmitted = true
	})
	plannerRegistry := tools.NewRegistry()
	plannerRegistry.Register(planTool)
	plannerRegistry.Register(tools.NewListRunAssetsTool(e.runID, e.runService))

	assets, _ := e.runService.ListAssetsByRun(ctx, e.runID)
	var assetInfo strings.Builder
	if len(assets) > 0 {
		assetInfo.WriteString("\nAvailable Assets in Scope:\n")
		for _, a := range assets {
			assetInfo.WriteString(fmt.Sprintf("- Asset: %s [id: %s] (kind: %s, name: %s)\n", a.ExternalKey, a.ID.String(), a.Kind, a.Name))
		}
	}

	plannerPrompt := fmt.Sprintf(
		"You are the Lead Security Planner. Break down the task into discrete work items and submit a plan.\nObjective: %s\nScope: %s%s",
		objective, string(scopeBytes), assetInfo.String())
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

	// 4. Worker Execution and Subsequent Planner Rounds Loop
	workCompletedChan := make(chan struct{}, 100)
	var activeWorkers sync.WaitGroup
	workerTokens := make(chan struct{}, e.maxWorkers)
	for i := 0; i < e.maxWorkers; i++ {
		workerTokens <- struct{}{}
	}
	var workerErr atomic.Value

	for {
		// 4a. Dispatch workers for any pending work items
		if err := e.dispatchWorkers(runCtx, workerTokens, workCompletedChan, &activeWorkers, &workerErr); err != nil {
			return
		}

		// 4b. Next Planner Round (Evaluation or Follow-up Planning)
		roundNum++
		if e.onPlannerRoundHook != nil {
			e.onPlannerRoundHook(roundNum)
		}

		roundType := "all_work_completed"
		prRound, err := e.runService.CreatePlannerRound(ctx, e.runID, roundNum, roundType, nil)
		if err != nil {
			slog.Error("engine: failed to create planner round", "run_id", e.runID, "round", roundNum, "error", err)
			reason := "persistence_error"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		sess, err := e.runService.CreateSession(ctx, e.runID, "planner", &roundNum, nil, nil, nil)
		if err != nil {
			slog.Error("engine: failed to create planner session", "run_id", e.runID, "round", roundNum, "error", err)
			reason := "persistence_error"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}
		_ = e.runService.UpdatePlannerRound(ctx, prRound.ID, "running", nil)

		// Gather work items and observations for review
		items, _ := e.runService.ListWorkItemsByRun(ctx, e.runID)
		obsList, _ := e.runService.ListObservationsByRun(ctx, e.runID)
		assets, _ = e.runService.ListAssetsByRun(ctx, e.runID)

		var obsInfo strings.Builder
		obsInfo.WriteString("Current execution progress:\n")
		obsInfo.WriteString(fmt.Sprintf("Completed work items: %d. Gathered observations: %d\n", len(items), len(obsList)))
		if len(items) > 0 {
			obsInfo.WriteString("Existing Work Items:\n")
			for _, it := range items {
				obsInfo.WriteString(fmt.Sprintf("- WorkItem: %s [id: %s] status: %s (type: %s)\n", it.Key, it.ID.String(), it.Status, it.WorkType))
			}
		}
		for _, obs := range obsList {
			assetRef := ""
			if obs.AssetID != nil {
				assetRef = fmt.Sprintf(" asset_id: %s", obs.AssetID.String())
			}
			obsInfo.WriteString(fmt.Sprintf("- [obs_id: %s] [%s]%s content: %s data: %s\n",
				obs.ID.String(), obs.ObsType, assetRef, obs.Content, string(obs.Data)))
		}
		if len(assets) > 0 {
			obsInfo.WriteString("\nAvailable Assets in Scope:\n")
			for _, a := range assets {
				obsInfo.WriteString(fmt.Sprintf("- Asset: %s [id: %s] (kind: %s, name: %s)\n", a.ExternalKey, a.ID.String(), a.Kind, a.Name))
			}
		}

		var followUpPlanSubmitted bool
		followUpPlanTool := tools.NewSubmitPlanToolWithRound(e.runID, &prRound.ID, e.runService, func(newItems []model.WorkItem) {
			followUpPlanSubmitted = true
		})

		var finalSummaryResult string
		var finalConclusion string
		var completionSuggested bool
		suggestTool := tools.NewSuggestCompletionTool(e.runID, e.runService, func(summary, conclusion string) {
			completionSuggested = true
			finalSummaryResult = summary
			finalConclusion = conclusion
		})

		evalRegistry := tools.NewRegistry()
		evalRegistry.Register(followUpPlanTool)
		evalRegistry.Register(suggestTool)
		evalRegistry.Register(tools.NewListRunAssetsTool(e.runID, e.runService))

		evalPrompt := fmt.Sprintf(
			"Review current execution progress and observations. You can either call submit_plan to schedule additional follow-up work items, or call suggest_completion to finish the run.\n%s",
			obsInfo.String())
		evalMessages := []provider.ChatMessage{
			{Role: provider.RoleSystem, Content: "You are the Lead Security Planner. Evaluate progress and either submit follow-up plan or suggest completion."},
			{Role: provider.RoleUser, Content: evalPrompt},
		}

		srPlanner := agent.NewSessionRunner(
			sess.ID, e.runID, "planner", e.runService, e.provider, evalRegistry, e.budget,
			agent.WithSessionModelCallHook(func(r int) {
				atomic.AddInt32(&e.globalModelCalls, 1)
			}),
			agent.WithSessionToolCallHook(func(id, name string) {
				atomic.AddInt32(&e.globalToolCalls, 1)
			}),
		)
		pRes, pErr := srPlanner.Run(runCtx, evalMessages)

		if runCtx.Err() != nil {
			e.handleCancellation(runCtx)
			return
		}

		if pErr != nil || pRes == nil || (!followUpPlanSubmitted && !completionSuggested) {
			slog.Error("engine: planner round failed", "run_id", e.runID, "round", roundNum, "error", pErr)
			reason := "planner_failed"
			_ = e.runService.UpdatePlannerRound(context.Background(), prRound.ID, "failed", nil)
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		if completionSuggested {
			_ = e.runService.UpdatePlannerRound(context.Background(), prRound.ID, "completed", &finalSummaryResult)

			if finalSummaryResult == "" {
				finalSummaryResult = pRes.FinalContent
			}
			if finalSummaryResult == "" {
				finalSummaryResult = "Execution finished with conclusion: " + finalConclusion
			}

			_, err = e.runService.CompleteRunWithOutput(context.Background(), e.runID, "completed", finalSummaryResult)
			if err != nil {
				slog.Error("engine: failed to complete run with output", "run_id", e.runID, "error", err)
			}
			return
		}

		// Follow-up plan was submitted! Mark round completed and loop to dispatch workers
		planSum := "Follow-up plan submitted"
		_ = e.runService.UpdatePlannerRound(context.Background(), prRound.ID, "completed", &planSum)
	}
}

func (e *Engine) dispatchWorkers(
	ctx context.Context,
	workerTokens chan struct{},
	workCompletedChan chan struct{},
	activeWorkers *sync.WaitGroup,
	workerErr *atomic.Value,
) error {
	for {
		select {
		case <-ctx.Done():
			e.handleCancellation(ctx)
			activeWorkers.Wait()
			return ctx.Err()
		default:
		}

		// Check global budget limits
		if atomic.LoadInt32(&e.globalModelCalls) >= int32(e.budget.MaxModelCalls) {
			reason := "model_calls_exhausted"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return errors.New("model_calls_exhausted")
		}
		if atomic.LoadInt32(&e.globalToolCalls) >= int32(e.budget.MaxToolCalls) {
			reason := "tool_calls_exhausted"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return errors.New("tool_calls_exhausted")
		}

		// Try acquiring worker concurrency token
		select {
		case token := <-workerTokens:
			// Claim next pending work item
			wi, att, sess, claimErr := e.runService.ClaimNextPendingWorkItem(ctx, e.runID)
			if claimErr != nil {
				workerTokens <- token
				slog.Error("engine: claim work item error", "run_id", e.runID, "error", claimErr)
				reason := "claim_failed"
				e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
				activeWorkers.Wait()
				return claimErr
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

					e.executeWorkerSession(ctx, item, attempt, s, workerErr)
				}(wi, att, sess, token)

				continue
			}

			// Nothing was ready to claim, return token
			workerTokens <- token

		default:
			// All worker slots currently busy, wait for a worker to finish
		}

		// Check status of all items in this Run
		items, err := e.runService.ListWorkItemsByRun(ctx, e.runID)
		if err != nil {
			slog.Error("engine: failed to list work items", "run_id", e.runID, "error", err)
			reason := "persistence_error"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return err
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
			// All work items finished
			break
		}

		if hasBlocked && !hasRunning {
			// Stuck due to failed/blocked prerequisites
			slog.Warn("engine: run blocked due to failed work item dependencies", "run_id", e.runID)
			reason := "work_item_blocked"
			e.runService.TransitionState(context.Background(), e.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			activeWorkers.Wait()
			return errors.New("work_item_blocked")
		}

		// Wait for next work completion notification or cancellation
		select {
		case <-ctx.Done():
			e.handleCancellation(ctx)
			activeWorkers.Wait()
			return ctx.Err()
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
		return fmt.Errorf("worker failed: %v", wErrVal)
	}

	return nil
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
	} else if wi.WorkType == "inspect_http_endpoint" {
		workerRegistry.Register(tools.NewGetEndpointTool(e.runID, wi.ID, e.runService))
		workerRegistry.Register(tools.NewExtractEndpointLinksTool(e.runID, wi.ID, sess.ID, e.runService))
		workerRegistry.Register(tools.NewRecordObservationToolWithSession(e.runID, att.ID, sess.ID, e.runService))
	} else {
		workerRegistry.Register(tools.NewReadSampleToolWithScope(e.runID, func(ctx context.Context, sampleID string) (bool, error) {
			return e.runService.IsSampleInRunScope(ctx, e.runID, sampleID)
		}))
		workerRegistry.Register(tools.NewRecordObservationToolWithSession(e.runID, att.ID, sess.ID, e.runService))
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
