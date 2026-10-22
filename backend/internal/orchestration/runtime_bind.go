package orchestration

import (
	"context"
	"encoding/json"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
)

// ProviderSource is the optional adapter Agent 2 can implement on a
// RuntimeResolver. A resolver that only returns ModelCaller keeps the run's
// existing Provider until that adapter is integrated.
type ProviderSource interface {
	Provider(ctx context.Context, scope contract.ExecutionScope, profile string) (provider.Provider, error)
}

func (e *Engine) configureRuntimeLimits(raw json.RawMessage) {
	limits := RuntimeLimitsFromSnapshot(raw)
	if e.modelLimit <= 0 {
		e.modelLimit = limits.ModelConcurrency
	}
	if e.modelLimit <= 0 {
		e.modelLimit = e.maxWorkers + 1
	}
	if e.modelLimit <= 0 {
		e.modelLimit = 1
	}
	if e.plannerReserve == 0 {
		e.plannerReserve = limits.PlannerReserve
	}
	if e.mainReserve == 0 {
		e.mainReserve = limits.MainReserve
	}
	if e.modelLimiter != nil {
		e.modelLimiter.SetLimit(e.modelLimit)
		if e.onModelWaitHook != nil {
			e.modelLimiter.SetQueueHook(e.onModelWaitHook)
		}
	}
}

func (e *Engine) publishCapacity(ctx context.Context) {
	if e.runService == nil {
		return
	}
	payload, err := json.Marshal(e.WorkerCapacity())
	if err != nil {
		return
	}
	_, _ = e.runService.InsertRunEventWithContext(ctx, e.runID, nil, nil, nil, "worker_capacity", payload)
}

func (e *Engine) providerFor(ctx context.Context, role contract.Role, profile string) provider.Provider {
	if e.runtime == nil {
		return e.provider
	}
	if source, ok := e.runtime.(ProviderSource); ok {
		scope := contract.ExecutionScope{RunID: e.runID, EpochNumber: e.epoch, Role: role}
		if role != contract.RoleWorker {
			if resolved, err := source.Provider(ctx, scope, profile); err == nil && resolved != nil {
				return resolved
			}
		}
	}
	return e.provider
}

func (e *Engine) plannerGate() agent.StructuredCallGate {
	return plannerCallGate{engine: e}
}

func (e *Engine) releaseModelAdmission() {
	if e.budgetMgr != nil {
		e.budgetMgr.ReleaseModelCall()
	}
	if e.modelLimiter != nil {
		e.modelLimiter.Release()
	}
}

type plannerCallGate struct {
	engine *Engine
}

func (g plannerCallGate) TryAcquireModel(ctx context.Context, isSettlement bool) agent.BudgetGateResult {
	if g.engine.modelLimiter != nil {
		if err := g.engine.modelLimiter.Acquire(ctx); err != nil {
			return agent.BudgetGateResult{Allowed: false, Scope: agent.ScopeGlobal, Cause: model.ResolveCancelCause(ctx)}
		}
	}
	if err := g.engine.budgetMgr.AcquireModelCall(ctx, false); err != nil {
		if g.engine.modelLimiter != nil {
			g.engine.modelLimiter.Release()
		}
		cause := model.CauseModelBudgetExhausted
		if ctx != nil && ctx.Err() != nil {
			cause = model.ResolveCancelCause(ctx)
		}
		return agent.BudgetGateResult{Allowed: false, Scope: agent.ScopeGlobal, Cause: cause}
	}
	return agent.BudgetGateResult{Allowed: true, Scope: agent.ScopeGlobal}
}

func (g plannerCallGate) TryAcquireTool(ctx context.Context, toolName string, isSettlement bool) agent.BudgetGateResult {
	if err := g.engine.budgetMgr.AcquireToolCall(ctx); err != nil {
		cause := model.CauseToolBudgetExhausted
		if ctx != nil && ctx.Err() != nil {
			cause = model.ResolveCancelCause(ctx)
		}
		return agent.BudgetGateResult{Allowed: false, Scope: agent.ScopeGlobal, Cause: cause}
	}
	return agent.BudgetGateResult{Allowed: true, Scope: agent.ScopeGlobal}
}
