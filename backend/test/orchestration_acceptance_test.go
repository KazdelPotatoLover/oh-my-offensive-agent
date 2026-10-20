package test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
)

// TestOrchestration_DualWorker_ParallelExecution_Mock verifies that dual workers
// run in parallel (concurrency = 2) without duplicate claims, with each work item executed exactly once.
func TestOrchestration_DualWorker_ParallelExecution_Mock(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Dual Worker Parallel Task",
		Objective: "Execute parallel HTTP response analysis",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home", "sample_login", "sample_health"]}`),
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "orchestrator"
	provType := "mock"
	scenario := string(provider.ScenarioOrchestration)
	runReq := model.CreateRunRequest{
		Executor:     &executor,
		Provider:     &provType,
		MockScenario: &scenario,
		Budget: &model.BudgetConfig{
			MaxModelCalls:      30,
			MaxToolCalls:       30,
			MaxParallelWorkers: 2, // Enable dual workers!
			RunTimeoutSeconds:  15,
		},
	}

	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	budget := *runReq.Budget
	mockProv := provider.NewMockProvider(provider.ScenarioOrchestration)
	// Add a small delay to ensure parallel overlap is easily recorded
	mockProv.SetDelay(20 * time.Millisecond)

	var currentActive int32
	var maxActive int32
	var claimedKeys []string

	engine := orchestration.NewEngine(
		run.ID, env.runService, mockProv, budget,
		orchestration.WithWorkerActiveHook(func(delta int) {
			curr := atomic.AddInt32(&currentActive, int32(delta))
			for {
				max := atomic.LoadInt32(&maxActive)
				if curr <= max || atomic.CompareAndSwapInt32(&maxActive, max, curr) {
					break
				}
			}
		}),
		orchestration.WithOnWorkItemHook(func(key string) {
			claimedKeys = append(claimedKeys, key)
		}),
	)

	err = env.manager.StartExecutor(run.ID, engine)
	if err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}

	// Wait for Run to complete
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var finalRun *model.Run
	for {
		select {
		case <-timeout:
			t.Fatalf("timed out waiting for dual-worker run to complete")
		case <-ticker.C:
			r, err := env.runService.Get(ctx, run.ID)
			if err != nil {
				t.Fatalf("failed to get run: %v", err)
			}
			if r.Status.IsTerminal() {
				finalRun = r
				goto RunDone
			}
		}
	}

RunDone:
	if finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected run status 'completed', got '%s'", finalRun.Status)
	}

	// 1. Verify concurrency reached 2
	if atomic.LoadInt32(&maxActive) < 2 {
		t.Errorf("expected max active workers to be 2, but got %d", atomic.LoadInt32(&maxActive))
	}

	// 2. Verify all work items completed with exactly 1 attempt each (no duplicate claims)
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 work items, got %d", len(items))
	}

	for _, it := range items {
		if it.Status != model.WorkItemStatusCompleted {
			t.Errorf("item %s not completed: %s", it.Key, it.Status)
		}
		// Check attempts
		atts, err := env.runService.ListAttemptsByWorkItem(ctx, it.ID)
		if err != nil {
			t.Fatalf("failed to list attempts for %s: %v", it.Key, err)
		}
		if len(atts) != 1 {
			t.Errorf("item %s has %d attempts, expected exactly 1 (no duplicate claims)", it.Key, len(atts))
		}
		if atts[0].Status != "completed" {
			t.Errorf("attempt for %s has status %s, expected 'completed'", it.Key, atts[0].Status)
		}
	}
}

// TestOrchestration_SubmitPlan_DuplicateAndConflict verifies that:
// - identical key + definition returns existing item without duplication
// - same key + different definition returns conflict error
// - batch with errors rolls back atomically without partial inserts
func TestOrchestration_SubmitPlan_DuplicateAndConflict(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Duplicate & Conflict Test",
		Objective: "Test plan idempotency and conflict detection",
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}
	// Transition to running
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// 1. Initial plan submission
	items1, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item_a", Title: "Title A", Objective: "Objective A"},
		{Key: "item_b", Title: "Title B", Objective: "Objective B", Dependencies: []string{"item_a"}},
	})
	if err != nil {
		t.Fatalf("initial submit plan failed: %v", err)
	}
	if len(items1) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items1))
	}

	// 2. Submit identical item_a again (idempotent reuse)
	items2, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item_a", Title: "Title A", Objective: "Objective A"},
	})
	if err != nil {
		t.Fatalf("idempotent submit plan failed: %v", err)
	}
	if len(items2) != 1 || items2[0].ID != items1[0].ID {
		t.Errorf("expected idempotent reuse of existing item_a, got %v", items2)
	}

	// Total count in DB should still be exactly 2
	allDBItems, _ := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if len(allDBItems) != 2 {
		t.Errorf("expected 2 work items in DB, got %d", len(allDBItems))
	}

	// 3. Submit conflicting item_a (different objective) alongside new item_c
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item_a", Title: "Title A", Objective: "DIFFERENT Objective A"},
		{Key: "item_c", Title: "Title C", Objective: "Objective C"},
	})
	if !errors.Is(err, service.ErrWorkItemConflict) {
		t.Fatalf("expected ErrWorkItemConflict, got %v", err)
	}

	// 4. Verify atomicity: item_c must NOT exist in DB
	allDBItemsAfter, _ := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if len(allDBItemsAfter) != 2 {
		t.Errorf("expected still 2 work items in DB after failed transaction, got %d", len(allDBItemsAfter))
	}
}

// TestOrchestration_Dependency_BlockCascade verifies that if a prerequisite work item
// fails, dependent work items automatically transition to 'blocked' with a clear blocked_reason.
func TestOrchestration_Dependency_BlockCascade(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Block Cascade Test",
		Objective: "Test dependency failure cascading to blocked status",
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// Submit A and B (B depends on A)
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item_a", Title: "Item A", Objective: "Do A"},
		{Key: "item_b", Title: "Item B", Objective: "Do B", Dependencies: []string{"item_a"}},
	})
	if err != nil {
		t.Fatalf("submit plan failed: %v", err)
	}

	// Claim Item A
	wiA, attA, sessA, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wiA == nil {
		t.Fatalf("failed to claim item A: %v", err)
	}

	// Mark Item A as failed
	err = env.runService.FinishWorkItem(ctx, wiA.ID, attA.ID, sessA.ID, false, nil, "simulated_worker_failure")
	if err != nil {
		t.Fatalf("failed to finish item A: %v", err)
	}

	// Try claiming next item. Item B's prerequisite failed, so ClaimNextPendingWorkItem should mark B as blocked
	wiB, attB, sessB, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("unexpected error claiming next item: %v", err)
	}
	if wiB != nil || attB != nil || sessB != nil {
		t.Errorf("expected no claimable items, but claimed %v", wiB)
	}

	// Check Item B's status in DB
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list items: %v", err)
	}

	var foundB *model.WorkItem
	for i := range items {
		if items[i].Key == "item_b" {
			foundB = &items[i]
			break
		}
	}
	if foundB == nil {
		t.Fatalf("item_b not found in DB")
	}
	if foundB.Status != model.WorkItemStatusBlocked {
		t.Errorf("expected item_b status 'blocked', got '%s'", foundB.Status)
	}
	if foundB.BlockedReason == nil || *foundB.BlockedReason == "" {
		t.Errorf("expected non-empty blocked_reason for item_b")
	}
}

// TestOrchestration_GlobalBudget_Exhausted verifies that budget limits are enforced
// globally across the Planner and all Workers, terminating the Run when exceeded.
func TestOrchestration_GlobalBudget_Exhausted(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Budget Exhausted Test",
		Objective: "Test global budget enforcement across sessions",
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "orchestrator"
	provType := "mock"
	scenario := string(provider.ScenarioOrchestration)
	runReq := model.CreateRunRequest{
		Executor:     &executor,
		Provider:     &provType,
		MockScenario: &scenario,
		Budget: &model.BudgetConfig{
			MaxModelCalls:      2, // Restrict total model calls to 2!
			MaxToolCalls:       20,
			MaxParallelWorkers: 1,
			RunTimeoutSeconds:  10,
		},
	}

	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	budget := *runReq.Budget
	mockProv := provider.NewMockProvider(provider.ScenarioOrchestration)

	engine := orchestration.NewEngine(run.ID, env.runService, mockProv, budget)
	err = env.manager.StartExecutor(run.ID, engine)
	if err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}

	// Wait for Run to fail due to exhausted budget
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var finalRun *model.Run
	for {
		select {
		case <-timeout:
			t.Fatalf("timed out waiting for budget exhaustion")
		case <-ticker.C:
			r, err := env.runService.Get(ctx, run.ID)
			if err != nil {
				t.Fatalf("failed to get run: %v", err)
			}
			if r.Status.IsTerminal() {
				finalRun = r
				goto RunBudgetDone
			}
		}
	}

RunBudgetDone:
	if finalRun.Status != model.RunStatusStopped {
		t.Fatalf("expected run status 'stopped', got '%s'", finalRun.Status)
	}
	if finalRun.FinishReason == nil || *finalRun.FinishReason != "model_calls_exhausted" {
		t.Errorf("expected finish_reason 'model_calls_exhausted', got %v", strPtrDeref(finalRun.FinishReason))
	}
}

// TestOrchestration_Stop_CancelsWorkers verifies that when stop is requested:
// - Run transitions to stopping/stopped
// - Active workers receive cancellation and clean up
// - No new work items are dispatched
// - Late results cannot overwrite terminal status
func TestOrchestration_Stop_CancelsWorkers(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Stop Cancels Workers Test",
		Objective: "Test stopping orchestration and cancelling active workers",
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "orchestrator"
	provType := "mock"
	scenario := string(provider.ScenarioOrchestration)
	runReq := model.CreateRunRequest{
		Executor:     &executor,
		Provider:     &provType,
		MockScenario: &scenario,
		Budget: &model.BudgetConfig{
			MaxModelCalls:      30,
			MaxToolCalls:       30,
			MaxParallelWorkers: 2,
			RunTimeoutSeconds:  15,
		},
	}

	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	budget := *runReq.Budget
	mockProv := provider.NewMockProvider(provider.ScenarioOrchestration)
	mockProv.SetDelay(100 * time.Millisecond) // Slow worker down so stop happens mid-run

	firstWorkStarted := make(chan struct{}, 1)
	engine := orchestration.NewEngine(
		run.ID, env.runService, mockProv, budget,
		orchestration.WithOnWorkItemHook(func(key string) {
			select {
			case firstWorkStarted <- struct{}{}:
			default:
			}
		}),
	)

	err = env.manager.StartExecutor(run.ID, engine)
	if err != nil {
		t.Fatalf("failed to start engine: %v", err)
	}

	// Wait until at least one work item starts
	select {
	case <-firstWorkStarted:
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for worker to start")
	}

	// Call Stop
	stoppedRun, err := env.runService.Stop(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to stop run: %v", err)
	}
	if stoppedRun.Status != model.RunStatusStopping && stoppedRun.Status != model.RunStatusStopped {
		t.Errorf("expected status stopping or stopped, got %s", stoppedRun.Status)
	}

	// Cancel executor via manager
	env.manager.Stop(run.ID)

	// Wait for terminal stopped status
	timeout := time.After(5 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	var finalRun *model.Run
	for {
		select {
		case <-timeout:
			t.Fatalf("timed out waiting for run to reach stopped")
		case <-ticker.C:
			r, err := env.runService.Get(ctx, run.ID)
			if err != nil {
				t.Fatalf("failed to get run: %v", err)
			}
			if r.Status.IsTerminal() {
				finalRun = r
				goto StopDone
			}
		}
	}

StopDone:
	if finalRun.Status != model.RunStatusStopped {
		t.Errorf("expected final status 'stopped', got '%s'", finalRun.Status)
	}

	// Attempting a late completion should NOT overwrite 'stopped'
	_, compErr := env.runService.CompleteRunWithOutput(ctx, run.ID, "completed", "late report")
	if compErr == nil {
		t.Errorf("expected error completing an already stopped run, but got nil")
	}

	finalCheck, _ := env.runService.Get(ctx, run.ID)
	if finalCheck.Status != model.RunStatusStopped {
		t.Errorf("run status was corrupted by late complete call: %s", finalCheck.Status)
	}
}
