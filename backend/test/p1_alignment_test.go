package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

type testFakeProvider struct {
	callFunc func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error)
}

func (f *testFakeProvider) Call(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	if f.callFunc != nil {
		return f.callFunc(ctx, messages, tools, opts)
	}
	return &provider.ModelResponse{Content: "done", FinishReason: "stop"}, nil
}

// TestP1_DualLayerBudget_WorkerLocalExhaustionAndSettlement tests that:
// 1. Worker local quota exhaustion does NOT abort the Run.
// 2. Worker local exhaustion transitions to settlement phase to write back facts/summary.
// 3. WorkItem status becomes 'exhausted', while Run remains 'running'.
func TestP1_DualLayerBudget_WorkerLocalExhaustionAndSettlement(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	// 1. Create Task & Run with Run-level budget=30, Worker-local budget=1 model call
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P1 Dual Layer Budget Task",
		Objective: "Test worker-local exhaustion",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	budgetCfg := model.BudgetConfig{
		MaxModelCalls:                  30,
		MaxToolCalls:                   30,
		RunTimeoutSeconds:              120,
		WorkerMaxModelCalls:            1, // Local limit: 1 exploration model call
		WorkerMaxToolCalls:             5,
		WorkerTimeoutSeconds:           60,
		WorkerSettlementTimeoutSeconds: 15,
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Budget: &budgetCfg,
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Update run to running
	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	if err != nil {
		t.Fatalf("update run to running: %v", err)
	}

	// 2. Create WorkItem via SubmitPlan
	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "wi-local-exhaustion",
			Title:     "Inspect Target",
			Objective: "Reach local budget limit",
			WorkType:  "analyze_sample",
			InputData: map[string]interface{}{"param": "test_local_budget"},
		},
	})
	if err != nil || len(items) == 0 {
		t.Fatalf("submit plan failed: %v", err)
	}
	wi := items[0]

	// 3. Claim work item -> Attempt 1
	claimedWI, att1, sess1, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || claimedWI == nil {
		t.Fatalf("claim work item failed: %v", err)
	}
	if att1.AttemptNumber != 1 {
		t.Fatalf("expected attempt_number 1, got %d", att1.AttemptNumber)
	}

	// 4. Create fake provider that returns a tool call on round 1, then attempts another tool call
	fakeP := &testFakeProvider{
		callFunc: func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
			// If last message is settlement message, conclude
			lastMsg := messages[len(messages)-1]
			if lastMsg.Role == provider.RoleUser && strings.Contains(lastMsg.Content, "局部探索额度耗尽") {
				return &provider.ModelResponse{
					Content:      "Settlement finished: progress saved.",
					FinishReason: "stop",
				}, nil
			}
			// First round: ask to read sample
			return &provider.ModelResponse{
				Content: "",
				ToolCalls: []provider.ToolCall{
					{
						ID:   "call_read_1",
						Type: "function",
						Function: provider.FunctionCall{
							Name:      "read_sample",
							Arguments: `{"sample_id":"sample_home"}`,
						},
					},
				},
				FinishReason: "tool_calls",
			}, nil
		},
	}

	registry := tools.NewRegistry()
	registry.Register(tools.NewReadSampleToolWithScope(run.ID, func(ctx context.Context, sampleID string) (bool, error) {
		return true, nil
	}))

	bm := orchestration.NewBudgetManager(budgetCfg, time.Now())
	workerGate := orchestration.NewWorkerBudgetGate(bm, budgetCfg)

	runner := agent.NewSessionRunner(
		sess1.ID, run.ID, "worker", env.runService, fakeP, registry, budgetCfg,
		agent.WithStructuredCallGate(workerGate),
	)

	initialMsgs := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "You are worker."},
		{Role: provider.RoleUser, Content: "Analyze sample."},
	}

	res, runErr := runner.Run(ctx, initialMsgs)
	if runErr != nil {
		t.Fatalf("runner.Run error: %v", runErr)
	}

	// 5. Verify local exhaustion and settlement
	if res.FinishReason != string(model.CauseWorkerModelBudgetExhausted) && res.FinishReason != string(model.CauseWorkerSettlementComplete) {
		t.Fatalf("expected finishReason to reflect worker local exhaustion/settlement, got %s", res.FinishReason)
	}

	// Finish WorkItem with exhausted reason
	err = env.runService.FinishWorkItem(ctx, wi.ID, att1.ID, sess1.ID, false, nil, res.FinishReason)
	if err != nil {
		t.Fatalf("finish work item: %v", err)
	}

	// Check DB state: WorkItem must be exhausted, Run must be running
	dbWI, err := env.runService.GetWorkItem(ctx, wi.ID)
	if err != nil {
		t.Fatalf("get work item: %v", err)
	}
	if dbWI.Status != model.WorkItemStatusExhausted {
		t.Fatalf("expected work item status exhausted, got %s", dbWI.Status)
	}

	var runStatus model.RunStatus
	err = env.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, run.ID).Scan(&runStatus)
	if err != nil {
		t.Fatalf("get run status: %v", err)
	}
	if runStatus != model.RunStatusRunning {
		t.Fatalf("expected run status running, got %s", runStatus)
	}
}

// TestP1_ContinueWorkItem_AndConversationResumption tests that:
// 1. ContinueWorkItem increments attempt_number and updates current_attempt_id.
// 2. ExpectedAttemptID mismatch rejects with ErrAttemptConflict.
// 3. LoadWorkItemConversation loads Attempt 1's messages without duplicating them in DB.
// 4. Attempt 2 continues the work item to completion.
func TestP1_ContinueWorkItem_AndConversationResumption(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P1 Continue Task",
		Objective: "Test work item continuation",
	})

	budgetCfg := model.DefaultBudgetConfig()
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Budget: &budgetCfg,
	}, nil)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)

	items, _ := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "wi-continue",
			Title:     "Inspect Target",
			Objective: "Continue after exhaustion",
			WorkType:  "analyze_sample",
		},
	})
	wi := items[0]

	// Claim Attempt 1
	_, att1, sess1, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	// Insert messages in Attempt 1
	callID := "call_step_1"
	toolCallsJSON, _ := json.Marshal([]provider.ToolCall{
		{ID: callID, Type: "function", Function: provider.FunctionCall{Name: "read_sample", Arguments: `{"sample_id":"s1"}`}},
	})
	_, _ = env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess1.ID, "user", "Start task", nil, nil)
	_, _ = env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess1.ID, "assistant", "", nil, toolCallsJSON)
	_, _ = env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess1.ID, "tool", `{"status":"ok","data":"sample data 1"}`, &callID, nil)

	// Finish Attempt 1 as exhausted
	_ = env.runService.FinishWorkItem(ctx, wi.ID, att1.ID, sess1.ID, false, nil, string(model.CauseWorkerModelBudgetExhausted))

	// Verify conflict if expectedAttemptID is wrong
	wrongID := uuid.New()
	_, err := env.runService.ContinueWorkItem(ctx, run.ID, wi.ID, wrongID, "retry after budget increase")
	if err == nil {
		t.Fatalf("expected ErrAttemptConflict for wrong expectedAttemptID, got nil")
	}

	// Continue with correct expectedAttemptID (att1.ID)
	wi2, err := env.runService.ContinueWorkItem(ctx, run.ID, wi.ID, att1.ID, "Planner decided to resume exploration")
	if err != nil {
		t.Fatalf("continue work item failed: %v", err)
	}
	if wi2.Status != model.WorkItemStatusPending {
		t.Fatalf("expected work item status pending, got %s", wi2.Status)
	}

	// Claim continued work item for Attempt 2
	claimedWi2, att2, sess2, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || claimedWi2 == nil {
		t.Fatalf("claim continued work item failed: %v", err)
	}
	if att2.AttemptNumber != 2 {
		t.Fatalf("expected attempt number 2, got %d", att2.AttemptNumber)
	}
	if *claimedWi2.CurrentAttemptID != att2.ID {
		t.Fatalf("expected current_attempt_id to be %s, got %s", att2.ID, *claimedWi2.CurrentAttemptID)
	}

	// LoadWorkItemConversation should retrieve all messages from Attempt 1
	history, err := env.runService.LoadWorkItemConversation(ctx, wi.ID)
	if err != nil {
		t.Fatalf("load work item conversation: %v", err)
	}
	if len(history) != 3 {
		t.Fatalf("expected 3 historical messages, got %d", len(history))
	}
	if history[0].Role != provider.RoleUser || history[1].Role != provider.RoleAssistant || history[2].Role != provider.RoleTool {
		t.Fatalf("unexpected message roles in history: %v", history)
	}

	// Run Attempt 2 using loaded history and verify no duplicate insertion
	msgCountBefore := 0
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE run_id=$1`, run.ID).Scan(&msgCountBefore)

	fakeP := &testFakeProvider{
		callFunc: func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
			return &provider.ModelResponse{
				Content:      "Completed analysis based on prior sample data.",
				FinishReason: "stop",
			}, nil
		},
	}

	registry := tools.NewRegistry()
	bm := orchestration.NewBudgetManager(budgetCfg, time.Now())
	workerGate := orchestration.NewWorkerBudgetGate(bm, budgetCfg)

	runner2 := agent.NewSessionRunner(
		sess2.ID, run.ID, "worker", env.runService, fakeP, registry, budgetCfg,
		agent.WithStructuredCallGate(workerGate),
		agent.WithInitialMessagesPersisted(true), // Do not re-insert history into DB!
	)

	// Append continuation prompt to history
	history = append(history, provider.ChatMessage{
		Role:    provider.RoleUser,
		Content: "继续执行本意图。已有成果已保留在上下文中，切勿重复发起已经成功的网络采集或探测动作。",
	})

	res, err := runner2.Run(ctx, history)
	if err != nil || res == nil || res.FinishReason != "completed" {
		t.Fatalf("attempt 2 runner failed: err=%v, res=%+v", err, res)
	}

	// Verify messages count in DB: only the final assistant reply was inserted in session 2!
	msgCountAfter := 0
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE run_id=$1`, run.ID).Scan(&msgCountAfter)
	if msgCountAfter != msgCountBefore+1 {
		t.Fatalf("expected exactly 1 new message in DB (assistant completion), before=%d, after=%d", msgCountBefore, msgCountAfter)
	}

	// Complete WorkItem
	outData, _ := json.Marshal(map[string]interface{}{"summary": res.FinalContent})
	err = env.runService.FinishWorkItem(ctx, wi.ID, att2.ID, sess2.ID, true, outData, "completed")
	if err != nil {
		t.Fatalf("finish work item: %v", err)
	}

	finalWI, _ := env.runService.GetWorkItem(ctx, wi.ID)
	if finalWI.Status != model.WorkItemStatusCompleted {
		t.Fatalf("expected completed status, got %s", finalWI.Status)
	}
}

// TestP1_UnexecutedToolCallProtocolClosure tests that when an attempt was interrupted
// with unexecuted tool calls in a batch, LoadWorkItemConversation synthesizes closure messages.
func TestP1_UnexecutedToolCallProtocolClosure(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Protocol Closure Task"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "wi-closure", Title: "Test", Objective: "Test", WorkType: "analyze_sample"},
	})
	if err != nil || len(items) == 0 {
		t.Fatalf("submit plan failed: %v", err)
	}
	wi := items[0]

	sessID := uuid.New()
	_, _ = env.pool.Exec(ctx, `INSERT INTO agent_sessions (id, run_id, role, work_item_id, status) VALUES ($1, $2, 'worker', $3, 'running')`, sessID, run.ID, wi.ID)

	// Assistant emitted 2 tool calls
	tc1 := "call_1"
	tc2 := "call_2"
	batchJSON, _ := json.Marshal([]provider.ToolCall{
		{ID: tc1, Type: "function", Function: provider.FunctionCall{Name: "tool_a", Arguments: `{}`}},
		{ID: tc2, Type: "function", Function: provider.FunctionCall{Name: "tool_b", Arguments: `{}`}},
	})
	_, _ = env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sessID, "assistant", "", nil, batchJSON)

	// Only tc1 got executed before exhaustion
	_, _ = env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sessID, "tool", `{"result": "a_done"}`, &tc1, nil)
	// tc2 was NEVER executed (interrupted by budget)

	reason := "worker_tool_budget_exhausted"
	if err := env.runService.UpdateSessionStatus(ctx, sessID, "exhausted", &reason); err != nil {
		t.Fatal(err)
	}
	// LoadWorkItemConversation must synthesize closure for tc2
	conversation, err := env.runService.LoadWorkItemConversation(ctx, wi.ID)
	if err != nil {
		t.Fatalf("load conversation: %v", err)
	}

	if len(conversation) != 3 {
		t.Fatalf("expected 3 messages (assistant, tool1, synthesized tool2), got %d", len(conversation))
	}
	if conversation[2].Role != provider.RoleTool || conversation[2].ToolCallID != tc2 {
		t.Fatalf("expected synthesized tool message for %s, got %+v", tc2, conversation[2])
	}
}

// TestP1_DependencyUnblocking_UponContinuationAndCompletion tests that:
// 1. When WorkItem A is exhausted, dependent WorkItem B is blocked.
// 2. When WorkItem A is continued and completed, WorkItem B is automatically unblocked back to pending.
func TestP1_DependencyUnblocking_UponContinuationAndCompletion(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Dep Unblock Task"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)

	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item-A", Title: "Step A", Objective: "Step A", WorkType: "analyze_sample"},
		{Key: "item-B", Title: "Step B", Objective: "Step B", WorkType: "analyze_sample", Dependencies: []string{"item-A"}},
	})
	if err != nil || len(items) != 2 {
		t.Fatalf("submit plan failed: %v", err)
	}
	wiA := items[0]
	wiB := items[1]

	// Claim Item A
	_, attA1, sessA1, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	// Verify Item B cannot be claimed yet because A is running
	claimedB, _, _, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if claimedB != nil {
		t.Fatalf("item B should not be claimable while A is running")
	}

	// Finish Item A as exhausted
	_ = env.runService.FinishWorkItem(ctx, wiA.ID, attA1.ID, sessA1.ID, false, nil, string(model.CauseWorkerModelBudgetExhausted))

	// Claiming again evaluates blocked items: Item B should be marked blocked
	_, _, _, _ = env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	dbB, _ := env.runService.GetWorkItem(ctx, wiB.ID)
	if dbB.Status != model.WorkItemStatusBlocked {
		t.Fatalf("expected item B to be blocked, got %s", dbB.Status)
	}

	// Continue Item A -> transitions to pending
	_, err = env.runService.ContinueWorkItem(ctx, run.ID, wiA.ID, attA1.ID, "continue A")
	if err != nil {
		t.Fatalf("continue A: %v", err)
	}

	// Claim Item A for Attempt 2
	claimedA2, attA2, sessA2, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || claimedA2 == nil {
		t.Fatalf("claim continued A failed: %v", err)
	}

	// Complete Item A
	err = env.runService.FinishWorkItem(ctx, wiA.ID, attA2.ID, sessA2.ID, true, json.RawMessage(`{"status":"done"}`), "completed")
	if err != nil {
		t.Fatalf("finish A completed: %v", err)
	}

	// Check Item B: it must be automatically unblocked back to pending!
	dbBAfter, _ := env.runService.GetWorkItem(ctx, wiB.ID)
	if dbBAfter.Status != model.WorkItemStatusPending {
		t.Fatalf("expected item B to be automatically unblocked to pending, got %s", dbBAfter.Status)
	}
	if dbBAfter.BlockedReason != nil {
		t.Fatalf("expected item B blocked_reason to be cleared, got %v", *dbBAfter.BlockedReason)
	}

	// Now Item B can be claimed!
	claimedB2, _, _, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || claimedB2 == nil || claimedB2.ID != wiB.ID {
		t.Fatalf("expected item B to be claimed, got: %+v, err: %v", claimedB2, err)
	}
}

// TestP1_PlannerTodo_PersistenceAcrossRounds tests persistent cross-round PlannerTodo store.
func TestP1_PlannerTodo_PersistenceAcrossRounds(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Planner Todo Task"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)

	// Round 1: Planner creates 2 todos
	t1, err := env.runService.AddOrUpdatePlannerTodo(ctx, run.ID, "check-login", "Test login endpoint with SQLi", "pending", nil)
	if err != nil {
		t.Fatalf("add todo 1: %v", err)
	}
	if t1.Version != 1 {
		t.Fatalf("expected version 1, got %d", t1.Version)
	}

	t2, err := env.runService.AddOrUpdatePlannerTodo(ctx, run.ID, "check-admin", "Check admin access", "pending", []string{"check-login"})
	if err != nil {
		t.Fatalf("add todo 2: %v", err)
	}
	if t2.ItemKey != "check-admin" {
		t.Fatalf("expected check-admin, got %s", t2.ItemKey)
	}

	// Round 2: Planner updates check-login to completed
	t1Updated, err := env.runService.AddOrUpdatePlannerTodo(ctx, run.ID, "check-login", "Test login endpoint with SQLi", "completed", nil)
	if err != nil {
		t.Fatalf("update todo 1: %v", err)
	}
	if t1Updated.Version != 2 || t1Updated.Status != "completed" {
		t.Fatalf("expected version 2 and status completed, got version=%d status=%s", t1Updated.Version, t1Updated.Status)
	}

	// Drop todo 2
	err = env.runService.DropPlannerTodo(ctx, run.ID, "check-admin")
	if err != nil {
		t.Fatalf("drop todo 2: %v", err)
	}

	// List todos
	todos, err := env.runService.ListPlannerTodos(ctx, run.ID)
	if err != nil {
		t.Fatalf("list todos: %v", err)
	}
	if len(todos) != 2 {
		t.Fatalf("expected 2 todos, got %d", len(todos))
	}
	if todos[0].Status != "completed" || todos[1].Status != "dropped" {
		t.Fatalf("unexpected statuses: %s, %s", todos[0].Status, todos[1].Status)
	}
}
