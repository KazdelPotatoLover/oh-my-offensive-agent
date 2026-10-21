package agent

import (
	"context"

	"agent-workbench/backend/internal/model"
)

type BudgetScope string

const (
	ScopeGlobal      BudgetScope = "global"
	ScopeWorkerLocal BudgetScope = "worker_local"
)

type BudgetGateResult struct {
	Allowed      bool
	Scope        BudgetScope
	Cause        model.TerminationCause
	IsSettlement bool
}

// StructuredCallGate provides typed, dual-layer quota control separating
// Run-level global caps from Worker-local per-attempt budgets and settlement.
type StructuredCallGate interface {
	TryAcquireModel(ctx context.Context, isSettlement bool) BudgetGateResult
	TryAcquireTool(ctx context.Context, toolName string, isSettlement bool) BudgetGateResult
}
