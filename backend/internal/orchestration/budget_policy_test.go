package orchestration

import (
	"context"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"github.com/google/uuid"
)

func TestBudgetPolicyLegacyZeroKeepsFallbacks(t *testing.T) {
	bm := NewBudgetManager(model.BudgetConfig{}, time.Now())
	if !bm.HasModelLimit() || !bm.HasToolLimit() || !bm.HasDeadline() {
		t.Fatal("policy-less manager must keep historical limits")
	}
	if bm.RemainingModelCalls() != 30 || bm.RemainingToolCalls() != 30 {
		t.Fatalf("legacy remaining model=%d tool=%d", bm.RemainingModelCalls(), bm.RemainingToolCalls())
	}
	eng := NewEngine(uuid.New(), nil, nil, model.BudgetConfig{})
	if !eng.budgetMgr.HasDeadline() || eng.budgetMgr.RemainingModelCalls() != 30 {
		t.Fatal("NewEngine without a policy must not adopt unlimited semantics")
	}
}

func TestBudgetPolicyV1ZeroDoesNotInventCaps(t *testing.T) {
	policy := model.BuiltinExplorationPolicy()
	cfg := policy.ProjectBudget()
	bm := NewBudgetManagerFromPolicy(policy, cfg, time.Now())
	if bm.HasModelLimit() || bm.HasToolLimit() || bm.HasDeadline() {
		t.Fatal("v1 zeros became hidden caps")
	}
	for i := 0; i < 40; i++ {
		if err := bm.AcquireModelCall(context.Background(), false); err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		bm.ReleaseModelCall()
		if err := bm.AcquireToolCall(context.Background()); err != nil {
			t.Fatalf("tool %d: %v", i, err)
		}
	}
	if !bm.CanClaimWorkItem(context.Background()) {
		t.Fatal("unlimited run was treated as exhausted")
	}
	if bm.RemainingModelCalls() != -1 || bm.RemainingToolCalls() != -1 {
		t.Fatalf("remaining model=%d tool=%d", bm.RemainingModelCalls(), bm.RemainingToolCalls())
	}
	if bm.ExhaustionCause() != model.CauseNeedsInput {
		t.Fatalf("unlimited exhaustion classified as %s", bm.ExhaustionCause())
	}
	bm.UpdateEffectiveBudget(model.BudgetConfig{MaxModelCalls: 10, MaxToolCalls: 10, RunTimeoutSeconds: 30})
	if bm.HasModelLimit() || bm.HasToolLimit() || bm.HasDeadline() || bm.RemainingModelCalls() != -1 {
		t.Fatal("grant-shaped update created a limit on an unlimited dimension")
	}
	bm.SetStartTime(time.Now())
	if bm.HasDeadline() {
		t.Fatal("SetStartTime armed a deadline for an unlimited task")
	}
	eng := NewEngineWithPolicy(uuid.New(), nil, nil, cfg, &policy)
	if eng.budgetMgr.HasDeadline() || eng.plannerHeartbeat != 10*time.Minute {
		t.Fatal("v1 engine armed a task timer or dropped planner coalescing")
	}
}

func TestBudgetPolicyExplicitCapsStillReject(t *testing.T) {
	policy := model.BuiltinExplorationPolicy()
	policy.HardLimits.ModelCalls = 2
	policy.HardLimits.ToolCalls = 1
	policy.TaskTimeoutSeconds = 1
	cfg := policy.ProjectBudget()
	bm := NewBudgetManagerFromPolicy(policy, cfg, time.Now().Add(-2*time.Second))
	if !bm.HasDeadline() || bm.CanClaimWorkItem(context.Background()) {
		t.Fatal("short explicit deadline still admitted work")
	}
	if bm.ExhaustionCause() != model.CauseTimeout {
		t.Fatalf("deadline cause=%s", bm.ExhaustionCause())
	}

	fresh := model.BuiltinExplorationPolicy()
	fresh.HardLimits.ModelCalls = 3
	limited := NewBudgetManagerFromPolicy(fresh, fresh.ProjectBudget(), time.Now())
	for i := 0; i < 2; i++ {
		if err := limited.AcquireModelCall(context.Background(), false); err != nil {
			t.Fatalf("exploration acquire %d: %v", i, err)
		}
		limited.ReleaseModelCall()
	}
	if err := limited.AcquireModelCall(context.Background(), false); err == nil {
		t.Fatal("explicit model cap admitted past the exploration reserve")
	}
	if err := limited.AcquireModelCall(context.Background(), true); err != nil {
		t.Fatalf("wrapup reserve was not kept: %v", err)
	}
}

func TestWorkerGateLogicalTurnsThenIndependentSettlement(t *testing.T) {
	policy := model.BuiltinExplorationPolicy()
	policy.Agents.Worker.MaxTurns = 2
	policy.Agents.Worker.SettlementMaxTurns = 3
	policy.Agents.Worker.RunTimeoutSeconds = 0
	cfg := policy.ProjectBudget()
	cfg.WorkerTimeoutSeconds = 0
	bm := NewBudgetManagerFromPolicy(policy, cfg, time.Now())
	gate := NewWorkerBudgetGateForPolicy(bm, cfg, policy)

	acquireExplore := func() {
		t.Helper()
		res := gate.TryAcquireModel(context.Background(), false)
		if !res.Allowed {
			t.Fatalf("explore acquire denied: %+v", res)
		}
		gate.NoteLogicalTurn(false)
		for i := 0; i < 2; i++ {
			tool := gate.TryAcquireTool(context.Background(), "get_endpoint", false)
			if !tool.Allowed {
				t.Fatalf("tool %d denied: %+v", i, tool)
			}
		}
	}
	acquireExplore()
	acquireExplore()
	stopped := gate.TryAcquireModel(context.Background(), false)
	if stopped.Allowed || !stopped.IsSettlement || stopped.Cause != model.CauseWorkerModelBudgetExhausted {
		t.Fatalf("third explore acquire = %+v", stopped)
	}
	blocked := gate.TryAcquireTool(context.Background(), "get_endpoint", true)
	if blocked.Allowed {
		t.Fatal("settlement allowed a new exploration tool")
	}
	if fact := gate.TryAcquireTool(context.Background(), "record_fact", true); !fact.Allowed {
		t.Fatalf("settlement blocked record_fact: %+v", fact)
	}
	for i := 0; i < 3; i++ {
		res := gate.TryAcquireModel(context.Background(), true)
		if !res.Allowed {
			t.Fatalf("settlement acquire %d denied: %+v", i, res)
		}
		gate.NoteLogicalTurn(true)
	}
	done := gate.TryAcquireModel(context.Background(), true)
	if done.Allowed || done.Cause != model.CauseWorkerSettlementComplete {
		t.Fatalf("settlement stop = %+v", done)
	}
}

func TestWorkerGateRequestCapIsNotMaxTurns(t *testing.T) {
	policy := model.BuiltinExplorationPolicy()
	policy.HardLimits.WorkerModelCalls = 2
	cfg := policy.ProjectBudget()
	cfg.WorkerTimeoutSeconds = 0
	gate := NewWorkerBudgetGateForPolicy(NewBudgetManagerFromPolicy(policy, cfg, time.Now()), cfg, policy)
	for i := 0; i < 2; i++ {
		res := gate.TryAcquireModel(context.Background(), false)
		if !res.Allowed {
			t.Fatalf("request %d denied: %+v", i, res)
		}
	}
	stopped := gate.TryAcquireModel(context.Background(), false)
	if stopped.Allowed || !stopped.IsSettlement {
		t.Fatalf("request cap did not enter settlement: %+v", stopped)
	}
}

func TestLegacyWorkerGateKeepsSmallFallbacks(t *testing.T) {
	bm := NewBudgetManager(model.BudgetConfig{MaxModelCalls: 100, MaxToolCalls: 100, RunTimeoutSeconds: 600}, time.Now())
	gate := NewWorkerBudgetGate(bm, model.BudgetConfig{})
	for i := 0; i < 5; i++ {
		if !gate.TryAcquire() {
			t.Fatalf("legacy gate stopped at call %d", i)
		}
	}
	if gate.TryAcquire() {
		t.Fatal("legacy worker gate ignored the 5-call fallback")
	}
}
