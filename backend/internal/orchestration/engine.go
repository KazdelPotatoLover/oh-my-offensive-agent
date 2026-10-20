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
	budgetMgr         *BudgetManager
	executionCtx      context.Context
	cancelWorkerCause context.CancelCauseFunc
	activeWorkerCount atomic.Int32
	wrapupOnce        sync.Once
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
		budgetMgr:  NewBudgetManager(budget, time.Now()),
	}
	for _, opt := range opts {
		opt(eng)
	}
	return eng
}

func (e *Engine) TryAcquireModelCall() bool {
	return e.budgetMgr.AcquireModelCall(e.callContext(), false) == nil
}

func (e *Engine) TryAcquireToolCall() bool {
	return e.budgetMgr.AcquireToolCall(e.callContext()) == nil
}

func waitWithContext(ctx context.Context, wg *sync.WaitGroup) error {
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func (e *Engine) Run(ctx context.Context) {
	defer e.ensureStoppingTerminal()

	// 1. Read Run from DB
	run, err := e.runService.Get(ctx, e.runID)
	if err != nil {
		slog.Error("engine: failed to get run", "run_id", e.runID, "error", err)
		return
	}

	if run.Status.IsTerminal() {
		return
	}

	// Early stop check
	if run.Status == model.RunStatusStopping {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		_ = e.runService.FinalizeTerminatedRun(cleanupCtx, e.runID, model.RunStatusStopped, "user_requested_stop")
		return
	}

	// 2. Transition queued -> running
	if run.Status == model.RunStatusQueued {
		run, err = e.runService.TransitionState(ctx, e.runID, model.RunStatusQueued, model.RunStatusRunning, nil)
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
	startedAt := time.Now()
	if run.StartedAt != nil {
		startedAt = *run.StartedAt
	}
	runCtx, cancelRun := context.WithDeadline(ctx, startedAt.Add(runTimeout))
	defer cancelRun()
	e.executionCtx = runCtx
	e.budgetMgr = NewBudgetManager(e.budget, startedAt)
	modelCalls, modelErr := e.runService.ListModelCalls(runCtx, e.runID)
	toolCalls, toolErr := e.runService.ListAgentToolCalls(runCtx, e.runID)
	if modelErr != nil || toolErr != nil {
		e.coordinateWrapup(runCtx, model.TerminationCause("persistence_error"), nil, nil, nil)
		return
	}
	e.budgetMgr.restoreUsage(len(modelCalls), len(toolCalls))

	// 3. Round 1: Planner submits initial plan
	roundNum := 1
	if e.onPlannerRoundHook != nil {
		e.onPlannerRoundHook(roundNum)
	}

	prRound1, err := e.runService.CreatePlannerRound(ctx, e.runID, roundNum, "initial", nil)
	if err != nil {
		slog.Error("engine: failed to create planner round 1", "run_id", e.runID, "error", err)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		e.coordinateWrapup(cleanupCtx, model.TerminationCause("persistence_error"), nil, nil, nil)
		return
	}

	sess1, err := e.runService.CreateSession(ctx, e.runID, "planner", &roundNum, nil, nil, run.ExecutionInput)
	if err != nil {
		slog.Error("engine: failed to create planner session 1", "run_id", e.runID, "error", err)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		e.coordinateWrapup(cleanupCtx, model.TerminationCause("persistence_error"), nil, nil, nil)
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
		{Role: provider.RoleSystem, Content: plannerContract + fmt.Sprintf("\nRun budget: %+v\nBudget snapshot: %+v", e.budget, e.budgetSummary())},
		{Role: provider.RoleUser, Content: plannerPrompt},
	}

	srPlanner1 := agent.NewSessionRunner(
		sess1.ID, e.runID, "planner", e.runService, e.provider, plannerRegistry, e.budget,
		agent.WithModelCallGate(agent.CallGateFunc(e.TryAcquireModelCall)),
		agent.WithSessionModelCallFinishHook(func(round int, err error) {
			if e.budgetMgr != nil {
				e.budgetMgr.ReleaseModelCall()
			}
		}),
		agent.WithToolCallGate(agent.CallGateFunc(e.TryAcquireToolCall)),
	)
	pRes1, pErr1 := srPlanner1.Run(runCtx, plannerMessages)

	if runCtx.Err() != nil {
		e.handleCancellation(runCtx)
		return
	}
	if pErr1 != nil || pRes1 == nil || pRes1.FinishReason != "completed" || !planSubmitted {
		slog.Error("engine: planner round 1 failed to submit plan", "run_id", e.runID, "error", pErr1)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		_ = e.runService.UpdatePlannerRound(cleanupCtx, prRound1.ID, "failed", nil)
		cause := plannerFailureCause(pRes1, pErr1)
		e.coordinateWrapup(cleanupCtx, cause, nil, nil, nil)
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

	workerCtx, cancelWorkerCause := context.WithCancelCause(runCtx)
	e.cancelWorkerCause = cancelWorkerCause
	cancelWorkers := func() { cancelWorkerCause(model.CauseInternalError) }
	defer cancelWorkers()

	for {
		// 4a. Dispatch workers for any pending work items
		if err := e.dispatchWorkers(workerCtx, cancelWorkers, workerTokens, workCompletedChan, &activeWorkers, &workerErr); err != nil {
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
			cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
			defer cancel()
			e.coordinateWrapup(cleanupCtx, model.TerminationCause("persistence_error"), nil, cancelWorkers, &activeWorkers)
			return
		}

		sess, err := e.runService.CreateSession(ctx, e.runID, "planner", &roundNum, nil, nil, nil)
		if err != nil {
			slog.Error("engine: failed to create planner session", "run_id", e.runID, "round", roundNum, "error", err)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
			defer cancel()
			e.coordinateWrapup(cleanupCtx, model.TerminationCause("persistence_error"), nil, cancelWorkers, &activeWorkers)
			return
		}
		_ = e.runService.UpdatePlannerRound(ctx, prRound.ID, "running", nil)

		// Gather work items and observations for review
		items, _ := e.runService.ListWorkItemsByRun(ctx, e.runID)
		obsList, _ := e.runService.ListObservationsByRun(ctx, e.runID)
		assets, _ = e.runService.ListAssetsByRun(ctx, e.runID)

		var obsInfo strings.Builder
		obsInfo.WriteString("Current execution progress:\n")
		completedCount, exhaustedCount, pendingCount := 0, 0, 0
		for _, it := range items {
			switch it.Status {
			case model.WorkItemStatusCompleted:
				completedCount++
			case model.WorkItemStatusExhausted:
				exhaustedCount++
			case model.WorkItemStatusPending, model.WorkItemStatusRunning:
				pendingCount++
			}
		}
		coverage, coverageErr := e.runService.ListAssetCoverage(ctx, e.runID)
		if coverageErr != nil {
			e.coordinateWrapup(ctx, model.TerminationCause("persistence_error"), nil, cancelWorkers, &activeWorkers)
			return
		}
		planned, visited := 0, 0
		for _, a := range coverage {
			if a.Planned {
				planned++
			}
			if a.Visited {
				visited++
			}
		}
		obsInfo.WriteString(fmt.Sprintf("Work items: completed=%d, worker_exhausted=%d, pending_or_running=%d; observations=%d; assets discovered=%d, planned=%d, visited=%d, unvisited=%d. These are current persisted snapshots and do not imply complete coverage. Visits come from successful reads, independently of observations and work completion.\n", completedCount, exhaustedCount, pendingCount, len(obsList), len(coverage), planned, visited, len(coverage)-visited))
		for _, a := range coverage {
			obsInfo.WriteString(fmt.Sprintf("- Asset coverage: %s [id: %s] planned=%t visited=%t\n", a.ExternalKey, a.AssetID, a.Planned, a.Visited))
		}
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
			{Role: provider.RoleSystem, Content: plannerContract + fmt.Sprintf("\nObjective: %s\nScope: %s\nRun budget: %+v; budget snapshot=%+v. Work item statuses and asset coverage are current snapshots, not proof of full coverage. Budget must include the model turn after tool calls. An exhausted worker has not completed its work; decide any bounded follow-up from existing evidence, never automatically replay external operations.", objective, scopeBytes, e.budget, e.budgetSummary())},
			{Role: provider.RoleUser, Content: evalPrompt},
		}

		srPlanner := agent.NewSessionRunner(
			sess.ID, e.runID, "planner", e.runService, e.provider, evalRegistry, e.budget,
			agent.WithModelCallGate(agent.CallGateFunc(e.TryAcquireModelCall)),
			agent.WithSessionModelCallFinishHook(func(round int, err error) {
				if e.budgetMgr != nil {
					e.budgetMgr.ReleaseModelCall()
				}
			}),
			agent.WithToolCallGate(agent.CallGateFunc(e.TryAcquireToolCall)),
		)
		pRes, pErr := srPlanner.Run(runCtx, evalMessages)

		if runCtx.Err() != nil {
			e.handleCancellation(runCtx)
			return
		}

		if pErr != nil || pRes == nil || pRes.FinishReason != "completed" || (!followUpPlanSubmitted && !completionSuggested) {
			slog.Error("engine: planner round failed", "run_id", e.runID, "round", roundNum, "error", pErr)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
			defer cancel()
			_ = e.runService.UpdatePlannerRound(cleanupCtx, prRound.ID, "failed", nil)
			cause := plannerFailureCause(pRes, pErr)
			e.coordinateWrapup(cleanupCtx, cause, nil, cancelWorkers, &activeWorkers)
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

			e.coordinateWrapup(context.Background(), model.CauseCompleted, &finalSummaryResult, cancelWorkers, &activeWorkers)
			return
		}

		// Follow-up plan was submitted! Mark round completed and loop to dispatch workers
		planSum := "Follow-up plan submitted"
		_ = e.runService.UpdatePlannerRound(context.Background(), prRound.ID, "completed", &planSum)
	}
}

func (e *Engine) dispatchWorkers(
	ctx context.Context,
	cancelWorkers context.CancelFunc,
	workerTokens chan struct{},
	workCompletedChan chan struct{},
	activeWorkers *sync.WaitGroup,
	workerErr *atomic.Value,
) error {
	for {
		select {
		case <-ctx.Done():
			e.coordinateWrapup(ctx, model.ResolveCancelCause(ctx), nil, cancelWorkers, activeWorkers)
			return ctx.Err()
		default:
		}

		if failure := workerErr.Load(); failure != nil {
			// A genuine worker failure takes precedence over a simultaneously
			// consumed budget. Stop admission before classifying exhaustion.
			cause := model.TerminationCause(failure.(string))
			e.coordinateWrapup(ctx, cause, nil, cancelWorkers, activeWorkers)
			return cause
		}
		// Stop admitting work at the exploration boundary. A worker owns its slot
		// until its final database writes finish, not merely until the model returns.
		if !e.budgetMgr.CanClaimWorkItem(ctx) {
			if e.activeWorkerCount.Load() > 0 {
				select {
				case <-ctx.Done():
					e.coordinateWrapup(ctx, model.ResolveCancelCause(ctx), nil, cancelWorkers, activeWorkers)
					return ctx.Err()
				case <-workCompletedChan:
				case <-time.After(100 * time.Millisecond):
				}
				continue
			}
			cause := e.budgetMgr.ExhaustionCause()
			e.coordinateWrapup(ctx, cause, nil, cancelWorkers, activeWorkers)
			return cause
		}

		// Try acquiring worker concurrency token
		select {
		case token := <-workerTokens:
			// Claim next pending work item
			wi, att, sess, claimErr := e.runService.ClaimNextPendingWorkItem(ctx, e.runID)
			if claimErr != nil {
				workerTokens <- token
				slog.Error("engine: claim work item error", "run_id", e.runID, "error", claimErr)
				cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
				defer cancel()
				e.coordinateWrapup(cleanupCtx, model.TerminationCause("claim_failed"), nil, cancelWorkers, activeWorkers)
				return claimErr
			}

			if wi != nil && att != nil && sess != nil {
				// Successfully claimed! Launch worker goroutine
				activeWorkers.Add(1)
				e.activeWorkerCount.Add(1)
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
						e.activeWorkerCount.Add(-1)
						activeWorkers.Done()
						select {
						case workCompletedChan <- struct{}{}:
						default:
						}
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
			cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
			defer cancel()
			e.coordinateWrapup(cleanupCtx, model.TerminationCause("persistence_error"), nil, cancelWorkers, activeWorkers)
			return err
		}

		hasRunning := false
		hasExhausted, hasFailed := false, false
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
			} else if it.Status == model.WorkItemStatusExhausted {
				hasExhausted = true
			} else if it.Status == model.WorkItemStatusFailed {
				hasFailed = true
				hasBlocked = true
				allDone = false
			}
		}

		if allDone {
			// All work items finished
			break
		}

		if hasBlocked && !hasRunning && hasExhausted && !hasFailed {
			// Local exhaustion is a planner decision point, including its blocked
			// dependents. Never turn it into an automatic replay or an error.
			break
		}
		if hasBlocked && !hasRunning {
			// Stuck due to failed/blocked prerequisites
			slog.Warn("engine: run blocked due to failed work item dependencies", "run_id", e.runID)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
			defer cancel()
			e.coordinateWrapup(cleanupCtx, model.TerminationCause("work_item_blocked"), nil, cancelWorkers, activeWorkers)
			return errors.New("work_item_blocked")
		}

		// Wait for next work completion notification or cancellation
		select {
		case <-ctx.Done():
			e.coordinateWrapup(ctx, model.ResolveCancelCause(ctx), nil, cancelWorkers, activeWorkers)
			return ctx.Err()
		case <-workCompletedChan:
			// A worker completed, check loop again!
		case <-time.After(100 * time.Millisecond):
			// Poll safeguard
		}
	}

	if err := waitWithContext(ctx, activeWorkers); err != nil {
		e.coordinateWrapup(ctx, model.ResolveCancelCause(ctx), nil, cancelWorkers, activeWorkers)
		return err
	}

	// Check if any worker reported terminal failure
	if wErrVal := workerErr.Load(); wErrVal != nil {
		slog.Error("engine: worker reported fatal failure", "run_id", e.runID, "error", wErrVal)
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		e.coordinateWrapup(cleanupCtx, model.TerminationCause(wErrVal.(string)), nil, cancelWorkers, activeWorkers)
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
	finish := func(success bool, output json.RawMessage, reason string) {
		finishCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := e.runService.FinishWorkItem(finishCtx, wi.ID, att.ID, sess.ID, success, output, reason); err != nil {
			workerErr.CompareAndSwap(nil, "persistence_error")
			slog.Error("engine: failed to persist work completion", "run_id", e.runID, "work_item_id", wi.ID, "error", err)
		} else if !success && model.TerminationCause(reason).TargetRunStatus() == model.RunStatusFailed {
			workerErr.CompareAndSwap(nil, reason)
		}
	}
	// Construct worker tools based on work_type
	workerRegistry := tools.NewRegistry()
	switch wi.WorkType {
	case "run_security_check":
		workerRegistry.Register(tools.NewRunSecurityCheckTool(e.runID, wi.ID, att.ID, sess.ID, e.runService))
	case "aggregate_summary":
		workerRegistry.Register(tools.NewReadObservationsTool(e.runID, e.runService))
	case "inspect_http_endpoint":
		workerRegistry.Register(tools.NewGetEndpointTool(e.runID, wi.ID, e.runService))
		workerRegistry.Register(tools.NewExtractEndpointLinksTool(e.runID, wi.ID, sess.ID, e.runService))
		workerRegistry.Register(tools.NewRecordObservationToolWithSession(e.runID, att.ID, sess.ID, e.runService))
		workerRegistry.Register(tools.NewRecordObservationBatchTool(e.runID, att.ID, sess.ID, e.runService))
	case "analyze_sample", "inspect_sample_endpoint":
		workerRegistry.Register(tools.NewReadSampleToolWithScope(e.runID, func(ctx context.Context, sampleID string) (bool, error) {
			return e.runService.IsSampleInRunScope(ctx, e.runID, sampleID)
		}))
		workerRegistry.Register(tools.NewRecordObservationToolWithSession(e.runID, att.ID, sess.ID, e.runService))
		workerRegistry.Register(tools.NewRecordObservationBatchTool(e.runID, att.ID, sess.ID, e.runService))
	default:
		// Also protect against unsupported records created by an older server.
		finish(false, nil, "unsupported_work_type")
		return
	}

	workerPrompt := fmt.Sprintf(
		"Work Item: %s\nTitle: %s\nObjective: %s\nInput Data: %s",
		wi.Key, wi.Title, wi.Objective, string(wi.InputData))

	workerMessages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: fmt.Sprintf("You are Worker for task %s (%s).", wi.Key, wi.WorkType) + workerContract(wi.WorkType)},
		{Role: provider.RoleUser, Content: workerPrompt},
	}

	sr := agent.NewSessionRunner(
		sess.ID, e.runID, "worker", e.runService, e.provider, workerRegistry, e.budget,
		agent.WithModelCallGate(agent.CallGateFunc(e.TryAcquireModelCall)),
		agent.WithSessionModelCallFinishHook(func(round int, err error) {
			if e.budgetMgr != nil {
				e.budgetMgr.ReleaseModelCall()
			}
		}),
		agent.WithToolCallGate(agent.CallGateFunc(e.TryAcquireToolCall)),
	)

	res, err := sr.Run(ctx, workerMessages)
	if ctx.Err() != nil {
		cause := model.ResolveCancelCause(ctx)
		finish(false, nil, string(cause))
		return
	}

	if err != nil || res == nil || res.FinishReason != "completed" {
		finishReason := "worker_failed"
		if res != nil && res.FinishReason != "" {
			finishReason = res.FinishReason
		}
		finish(false, nil, finishReason)
		return
	}
	if wi.WorkType == "inspect_http_endpoint" {
		observations, obsErr := e.runService.ListObservationsByRun(ctx, e.runID)
		hasEvidence := false
		for _, obs := range observations {
			if obs.AttemptID == att.ID {
				hasEvidence = true
				break
			}
		}
		if obsErr != nil || !hasEvidence {
			finish(false, nil, "missing_http_evidence")
			return
		}
	}
	if wi.WorkType == "run_security_check" {
		checks, checkErr := e.runService.ListCheckResultsByAttempt(ctx, att.ID)
		if checkErr != nil || len(checks) == 0 {
			finish(false, nil, "missing_check_results")
			return
		}
	}

	outputMap := map[string]interface{}{
		"summary": res.FinalContent,
	}
	outBytes, _ := json.Marshal(outputMap)
	finish(true, outBytes, "completed")
}

func (e *Engine) coordinateWrapup(
	ctx context.Context,
	cause model.TerminationCause,
	finalOutput *string,
	cancelWorkers context.CancelFunc,
	activeWorkers *sync.WaitGroup,
) {
	e.wrapupOnce.Do(func() {
		e.budgetMgr.MarkWrapupStarted()
		// Cancellation is delivered before waiting. Database finalization is bounded
		// independently of a model/tool that fails to honor its context.
		if e.cancelWorkerCause != nil {
			e.cancelWorkerCause(cause)
		}
		if cancelWorkers != nil {
			cancelWorkers()
		}
		if activeWorkers != nil {
			// Keep the worker drain grace bounded independently from database
			// cleanup. A provider that ignores cancellation must not delay the
			// terminal transaction beyond the established five-second grace.
			drainCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := waitWithContext(drainCtx, activeWorkers); err != nil {
				slog.Warn("engine: worker drain grace expired", "run_id", e.runID)
			}
			cancel()
		}
		// Cleanup is database-only and must remain available even when the original
		// run context is cancelled. It has its own bounded deadline.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer cancel()
		if e.executionCtx != nil && e.executionCtx.Err() != nil {
			cause = model.ResolveCancelCause(e.executionCtx)
		}
		if run, err := e.runService.Get(cleanupCtx, e.runID); err == nil && run.Status == model.RunStatusStopping {
			cause = model.CauseUserStop
		}
		inventory := GenerateProgrammaticSummary(cleanupCtx, e.runID, e.runService, cause)
		summaryText := ""
		if finalOutput != nil {
			summaryText = *finalOutput + "\n\n" + inventory
		}
		if summaryText == "" {
			summaryText = inventory
			if cause == model.CauseModelBudgetExhausted || cause == model.CauseToolBudgetExhausted {
				summaryText = e.summarizeWithReservedCall(cleanupCtx, cause, summaryText)
			}
		}
		if e.executionCtx != nil && e.executionCtx.Err() != nil {
			latestCause := model.ResolveCancelCause(e.executionCtx)
			if latestCause != cause {
				// Preserve the evidence snapshot even when its collection context
				// expired while the model was summarizing. A cancelled summary is
				// discarded; no extra model request or database read is needed.
				summaryText = strings.Replace(inventory, fmt.Sprintf("**Termination Cause**: %s", cause), fmt.Sprintf("**Termination Cause**: %s", latestCause), 1)
				cause = latestCause
			}
		}
		// The model summary may use its configured window (subject to the original
		// run deadline). Start a fresh, independent database-only deadline for the
		// final terminal transaction so a slow-but-allowed summary cannot consume
		// the cleanup window.
		finalizeCtx, finalizeCancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
		defer finalizeCancel()
		if err := e.runService.FinalizeRunWrapup(finalizeCtx, e.runID, cause, &summaryText); err != nil {
			slog.Error("engine: failed to finalize run wrapup", "run_id", e.runID, "error", err)
		}
	})
}

func (e *Engine) handleCancellation(ctx context.Context) {
	e.coordinateWrapup(ctx, model.ResolveCancelCause(ctx), nil, nil, nil)
}

func (e *Engine) ensureStoppingTerminal() {
	cleanupCtx, cancel := context.WithTimeout(context.Background(), e.cleanupTimeout())
	defer cancel()
	run, err := e.runService.Get(cleanupCtx, e.runID)
	if err != nil {
		return
	}
	if run.Status == model.RunStatusStopping {
		if err := e.runService.FinalizeRunWrapup(cleanupCtx, e.runID, model.CauseUserStop, nil); err != nil {
			slog.Error("engine: failed to finalize stop", "run_id", e.runID, "error", err)
		}
	}
}

func (e *Engine) callContext() context.Context {
	if e.executionCtx != nil {
		return e.executionCtx
	}
	return context.Background()
}

func (e *Engine) cleanupTimeout() time.Duration {
	seconds := e.budget.CleanupTimeoutSeconds
	if seconds <= 0 {
		seconds = 10
	}
	return time.Duration(seconds) * time.Second
}

func (e *Engine) budgetSummary() model.BudgetSummary {
	remaining := e.budgetMgr.RemainingTime()
	seconds := int(remaining / time.Second)
	if seconds < 0 {
		seconds = 0
	}
	return model.BudgetSummary{
		RemainingModelCalls:       e.budgetMgr.RemainingModelCalls(),
		RemainingExplorationCalls: e.budgetMgr.RemainingExplorationModelCalls(),
		RemainingToolCalls:        e.budgetMgr.RemainingToolCalls(),
		RemainingSeconds:          seconds,
	}
}

func plannerFailureCause(result *agent.SessionResult, err error) model.TerminationCause {
	if errors.Is(err, agent.ErrPersistenceFailure) {
		return model.TerminationCause("persistence_error")
	}
	if errors.Is(err, provider.ErrProtocol) {
		return model.TerminationCause("protocol_error")
	}
	if err != nil {
		return model.TerminationCause("provider_error")
	}
	if result != nil && result.FinishReason != "" && result.FinishReason != "completed" {
		return model.TerminationCause(result.FinishReason)
	}
	return model.CauseInternalError
}
