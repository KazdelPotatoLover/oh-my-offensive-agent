package test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
)

// TestOrchestration_SingleWorker_ClosedLoop_Mock tests the complete single-worker
// closed-loop lifecycle: Planner Round 1 -> 3 analyze workers + 1 aggregate worker -> Planner Round 2 -> Completed Run.
func TestOrchestration_SingleWorker_ClosedLoop_Mock(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Single Worker Closed Loop",
		Objective: "Analyze all HTTP response samples and aggregate findings",
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
			MaxParallelWorkers: 1,
			RunTimeoutSeconds:  15,
		},
	}

	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	budget := *runReq.Budget
	mockProv := provider.NewMockProvider(provider.ScenarioOrchestration)

	var rounds []int
	var workItemsExecuted []string
	engine := orchestration.NewEngine(
		run.ID, env.runService, mockProv, budget,
		orchestration.WithOnPlannerRoundHook(func(round int) {
			rounds = append(rounds, round)
		}),
		orchestration.WithOnWorkItemHook(func(key string) {
			workItemsExecuted = append(workItemsExecuted, key)
		}),
	)

	// Launch via manager
	err = env.manager.StartExecutor(run.ID, engine)
	if err != nil {
		t.Fatalf("failed to start engine executor: %v", err)
	}

	// Wait for Run to reach terminal status
	var finalRun *model.Run
	timeout := time.After(10 * time.Second)
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-timeout:
			t.Fatalf("timed out waiting for orchestration run to complete")
		case <-ticker.C:
			r, err := env.runService.Get(ctx, run.ID)
			if err != nil {
				t.Fatalf("failed to get run: %v", err)
			}
			if r.Status.IsTerminal() {
				finalRun = r
				goto RunFinished
			}
		}
	}

RunFinished:
	if finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected run status 'completed', got '%s' (finish_reason: %v)",
			finalRun.Status, strPtrDeref(finalRun.FinishReason))
	}

	if finalRun.FinalOutput == nil || *finalRun.FinalOutput == "" {
		t.Errorf("expected non-empty final_output in completed run")
	}

	// 1. Verify Planner Rounds
	roundsInDB, err := env.runService.ListPlannerRounds(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list planner rounds: %v", err)
	}
	// Persisted graph changes may wake the event-driven Planner while workers
	// are active. Check bounded, contiguous completed rounds, not one schedule.
	if len(roundsInDB) < 2 || len(roundsInDB) > model.DefaultBudgetConfig().MaxPlannerRounds {
		t.Fatalf("expected bounded planner rounds, got %d", len(roundsInDB))
	}
	for i, round := range roundsInDB {
		if round.Round != i+1 || round.Status != "completed" {
			t.Errorf("expected completed round %d, got round=%d status=%s", i+1, round.Round, round.Status)
		}
	}

	// 2. Verify Work Items
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list work items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 work items, got %d", len(items))
	}

	itemMap := make(map[string]model.WorkItem)
	for _, it := range items {
		itemMap[it.Key] = it
		if it.Status != model.WorkItemStatusCompleted {
			t.Errorf("work item '%s' status expected 'completed', got '%s'", it.Key, it.Status)
		}
		if it.CurrentAttemptID == nil {
			t.Errorf("work item '%s' missing current_attempt_id", it.Key)
		}
	}

	// 3. Verify Dependencies: aggregate_summary depends on the other 3
	aggItem := itemMap["aggregate_summary"]
	deps, err := env.runService.ListWorkDependenciesByWorkItem(ctx, aggItem.ID)
	if err != nil {
		t.Fatalf("failed to list dependencies: %v", err)
	}
	if len(deps) != 3 {
		t.Errorf("expected 3 dependencies for aggregate_summary, got %d", len(deps))
	}

	// 4. Verify Observations
	obsList, err := env.runService.ListObservationsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list observations: %v", err)
	}
	if len(obsList) != 3 {
		t.Fatalf("expected 3 observations, got %d", len(obsList))
	}

	// Verify each observation has a valid source_tool_call_id
	toolCalls, err := env.runService.ListAgentToolCalls(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list tool calls: %v", err)
	}
	tcMap := make(map[uuid.UUID]model.AgentToolCall)
	for _, tc := range toolCalls {
		tcMap[tc.ID] = tc
	}

	for _, obs := range obsList {
		if obs.SourceToolCallID == uuid.Nil {
			t.Errorf("observation %s has nil source_tool_call_id", obs.ID)
			continue
		}
		sourceTC, exists := tcMap[obs.SourceToolCallID]
		if !exists {
			t.Errorf("observation %s references non-existent source tool call %s", obs.ID, obs.SourceToolCallID)
			continue
		}
		if sourceTC.ToolName != "read_sample" {
			t.Errorf("observation %s expected source tool 'read_sample', got '%s'", obs.ID, sourceTC.ToolName)
		}
		if sourceTC.Status != "success" {
			t.Errorf("observation %s source tool call status expected 'success', got '%s'", obs.ID, sourceTC.Status)
		}
	}

	// 5. Verify Sessions
	sessions, err := env.runService.ListSessionsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list sessions: %v", err)
	}
	if len(sessions) != len(roundsInDB)+len(items) {
		t.Fatalf("expected one session per planner round and worker, got %d", len(sessions))
	}

	for _, sess := range sessions {
		if sess.Status != "completed" {
			t.Errorf("session %s status expected 'completed', got '%s'", sess.ID, sess.Status)
		}
		// Check that messages in each session start at seq 1 and are contiguous
		msgs, err := env.runService.ListAgentMessagesBySession(ctx, sess.ID)
		if err != nil {
			t.Fatalf("failed to list messages for session %s: %v", sess.ID, err)
		}
		if len(msgs) == 0 {
			t.Errorf("session %s has 0 messages", sess.ID)
			continue
		}
		for i, m := range msgs {
			if m.Seq != i+1 {
				t.Errorf("session %s message %d expected seq=%d, got %d", sess.ID, i, i+1, m.Seq)
			}
		}
	}

	// 6. Verify RunEvents
	events, err := env.runService.GetEvents(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to get events: %v", err)
	}
	if len(events) == 0 {
		t.Fatalf("expected run events, got 0")
	}

	hasWorkClaimed := false
	hasFinalAnswer := false
	for _, ev := range events {
		if ev.Type == "work_item_claimed" {
			hasWorkClaimed = true
		}
		if ev.Type == "final_answer" {
			hasFinalAnswer = true
		}
	}
	if !hasWorkClaimed {
		t.Errorf("missing 'work_item_claimed' event in run_events")
	}
	if !hasFinalAnswer {
		t.Errorf("missing 'final_answer' event in run_events")
	}
}

func strPtrDeref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
