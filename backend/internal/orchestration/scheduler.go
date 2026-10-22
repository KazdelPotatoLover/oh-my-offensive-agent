package orchestration

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/jackc/pgx/v5/pgconn"
)

func (e *Engine) notifyDispatcher() {
	if e.dispatchNotify == nil {
		return
	}
	select {
	case e.dispatchNotify <- struct{}{}:
	default:
	}
}

func (e *Engine) markPlannerDirty() {
	e.plannerDirty.Store(true)
	e.notifyPlanner()
}

func (e *Engine) markIdleDecision() {
	e.idleDecision.Store(true)
	e.notifyPlanner()
}

func (e *Engine) startDispatcher(ctx context.Context) {
	if !e.dispatcherStarted.CompareAndSwap(false, true) {
		return
	}
	e.admission.Store(true)
	if e.workerTokens == nil {
		e.workerTokens = make(chan struct{}, e.maxWorkers)
		for i := 0; i < e.maxWorkers; i++ {
			e.workerTokens <- struct{}{}
		}
	}
	e.dispatcherDone = make(chan struct{})
	go e.dispatcherLoop(ctx)
}

func (e *Engine) dispatcherLoop(ctx context.Context) {
	defer close(e.dispatcherDone)
	for {
		if !e.admission.Load() || ctx.Err() != nil {
			return
		}
		if failure := e.workerErr.Load(); failure != nil {
			e.fatalCause.Store(failure)
			e.markIdleDecision()
			return
		}
		if e.isPaused.Load() {
			if !e.waitDispatch(ctx, 50*time.Millisecond) {
				return
			}
			continue
		}
		if e.budgetMgr != nil && !e.budgetMgr.CanClaimWorkItem(ctx) {
			if e.activeWorkerCount.Load() == 0 {
				e.fatalCause.Store(string(e.budgetMgr.ExhaustionCause()))
				e.markIdleDecision()
			}
			if !e.waitDispatch(ctx, 100*time.Millisecond) {
				return
			}
			continue
		}

		select {
		case <-ctx.Done():
			return
		case token := <-e.workerTokens:
			if !e.admission.Load() || e.isPaused.Load() || ctx.Err() != nil {
				e.workerTokens <- token
				if ctx.Err() != nil || !e.admission.Load() {
					return
				}
				continue
			}
			if e.budgetMgr.WorkerModelCallsRemaining(e.plannerReserve, e.mainReserve) == 0 {
				e.workerTokens <- token
				// Let Planner review persisted results, without creating empty
				// attempts that cannot make even one model request.
				if e.workerBudgetBlocked.CompareAndSwap(false, true) {
					e.markIdleDecision()
				}
				if !e.waitDispatch(ctx, 100*time.Millisecond) {
					return
				}
				continue
			}
			e.workerBudgetBlocked.Store(false)
			wi, att, sess, claimErr := e.runService.ClaimNextPendingWorkItem(ctx, e.runID)
			if claimErr != nil {
				e.workerTokens <- token
				if ctx.Err() != nil || errors.Is(claimErr, context.Canceled) || errors.Is(claimErr, context.DeadlineExceeded) {
					return
				}
				if errors.Is(claimErr, service.ErrAttemptConflict) || isUniqueViolation(claimErr) {
					continue
				}
				slog.Error("engine: claim work item error", "run_id", e.runID, "error", claimErr)
				e.workerErr.Store("claim_failed")
				e.fatalCause.Store("claim_failed")
				e.markIdleDecision()
				return
			}
			if wi == nil || att == nil || sess == nil {
				e.workerTokens <- token
				if !e.waitDispatch(ctx, 100*time.Millisecond) {
					return
				}
				continue
			}
			e.workers.Add(1)
			e.activeWorkerCount.Add(1)
			if e.onWorkItemHook != nil {
				e.onWorkItemHook(wi.Key)
			}
			if e.onWorkerActiveHook != nil {
				e.onWorkerActiveHook(1)
			}
			e.activeAttempts.Store(wi.ID, att.ID)
			go e.runClaimedWorker(ctx, wi, att, sess, token)
		}
	}
}

func (e *Engine) runClaimedWorker(ctx context.Context, wi *model.WorkItem, att *model.Attempt, sess *model.AgentSession, token struct{}) {
	defer func() {
		e.activeAttempts.Delete(wi.ID)
		if e.onWorkerActiveHook != nil {
			e.onWorkerActiveHook(-1)
		}
		e.workerTokens <- token
		remaining := e.activeWorkerCount.Add(-1)
		e.workers.Done()
		e.notifyDispatcher()
		e.markPlannerDirty()
		if remaining == 0 {
			e.markIdleDecision()
		}
	}()
	e.executeWorkerSession(ctx, wi, att, sess, &e.workerErr)
}

func (e *Engine) waitDispatch(ctx context.Context, fallback time.Duration) bool {
	timer := time.NewTimer(fallback)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-e.dispatchNotify:
		return e.admission.Load()
	case <-timer.C:
		return e.admission.Load() && ctx.Err() == nil
	}
}

func (e *Engine) plannerShouldEvaluate(ctx context.Context) bool {
	if e.fatalCause.Load() != nil || e.workerErr.Load() != nil {
		return true
	}
	if e.plannerDirty.Load() || e.idleDecision.Load() {
		return true
	}
	if !e.lastPlannerAt.IsZero() && time.Since(e.lastPlannerAt) >= e.plannerHeartbeat {
		return true
	}
	if e.activeWorkerCount.Load() > 0 {
		return false
	}
	if e.budgetMgr != nil && !e.budgetMgr.CanClaimWorkItem(ctx) {
		return true
	}
	items, err := e.runService.ListWorkItemsByRun(ctx, e.runID)
	if err != nil {
		return true
	}
	pending := 0
	for _, it := range items {
		if it.Status == model.WorkItemStatusPending {
			pending++
		}
	}
	if pending > 0 && e.admission.Load() && !e.isPaused.Load() {
		return false
	}
	if len(items) == 0 {
		return e.epoch > 1
	}
	return true
}

func (e *Engine) waitUntilPlannerNeeded(ctx context.Context) error {
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if e.plannerShouldEvaluate(ctx) {
			// Dirty flags and persisted changes can make us ready before we
			// receive a channel notification. Coalesce every wake path before
			// starting (and charging for) a planner round, as ARTEX does.
			if err := e.coalescePlannerSignals(ctx); err != nil {
				return err
			}
			e.plannerDirty.Store(false)
			e.idleDecision.Store(false)
			return nil
		}
		if err := e.waitPlannerSignal(ctx); err != nil {
			return err
		}
	}
}

func (e *Engine) waitPlannerSignal(ctx context.Context) error {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-e.notifyChan:
		return nil
	case <-timer.C:
		latest, err := e.runService.LatestExplorationChange(ctx, e.runID, e.epoch)
		if err != nil {
			return err
		}
		if latest > e.changeCursor {
			e.plannerDirty.Store(true)
		}
		return nil
	}
}

func (e *Engine) coalescePlannerSignals(ctx context.Context) error {
	timer := time.NewTimer(e.debounce)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-e.notifyChan:
		case <-timer.C:
			return ctx.Err()
		}
	}
}

func (e *Engine) consumeFatal() (model.TerminationCause, bool) {
	if v := e.fatalCause.Load(); v != nil {
		if cause, ok := v.(model.TerminationCause); ok {
			return cause, true
		}
		if text, ok := v.(string); ok && text != "" {
			return model.TerminationCause(text), true
		}
	}
	if v := e.workerErr.Load(); v != nil {
		if text, ok := v.(string); ok && text != "" {
			return model.TerminationCause(text), true
		}
	}
	return "", false
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}
