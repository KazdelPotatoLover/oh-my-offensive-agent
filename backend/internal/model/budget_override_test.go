package model

import "testing"

func TestBudgetWithOverridesKeepsOmittedZeros(t *testing.T) {
	base := DefaultBudgetConfig()
	base.WorkerMaxModelCalls = 5
	override := &BudgetConfig{WorkerMaxModelCalls: 3, MaxParallelWorkers: 4, MaxParallelModelCalls: 2}
	got := base.WithOverrides(override)
	if got.WorkerMaxModelCalls != 3 || got.MaxParallelWorkers != 4 || got.MaxParallelModelCalls != 2 {
		t.Fatalf("positive overrides = %+v", got)
	}
	if got.MaxModelCalls != base.MaxModelCalls || got.WorkerMaxToolCalls != base.WorkerMaxToolCalls {
		t.Fatalf("zero request fields wiped the base: %+v", got)
	}
	if got.WithOverrides(nil) != got {
		t.Fatal("nil override changed the budget")
	}
}
