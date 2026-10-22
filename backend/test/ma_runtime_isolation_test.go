package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
)

func TestMARuntime_HTTPWorkerCount_DefaultAndReject(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()
	scope, _ := json.Marshal(map[string]any{
		"mode":  "discovery",
		"seeds": []string{"http://127.0.0.1:9/"},
	})
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name: "M1 default workers", Objective: "default exploration pool", Scope: scope,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID.String()+"/runs", bytes.NewBufferString(`{"executor":"orchestrator","provider":"mock","mock_scenario":"discovery"}`))
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("default create status %d body %s", rec.Code, rec.Body.String())
	}
	var created model.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	n, err := orchestration.WorkersFromSnapshot(created.ExecutionInput)
	if err != nil || n != 3 {
		t.Fatalf("new exploration default workers=%d err=%v", n, err)
	}
	env.manager.Stop(created.ID)

	bad := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID.String()+"/runs", bytes.NewBufferString(`{"executor":"orchestrator","budget":{"max_parallel_workers":9}}`))
	env.router.ServeHTTP(bad, badReq)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("out of range status %d body %s", bad.Code, bad.Body.String())
	}
	var body map[string]string
	_ = json.Unmarshal(bad.Body.Bytes(), &body)
	if body["code"] != "concurrency_out_of_range" {
		t.Fatalf("out of range body=%s", bad.Body.String())
	}
}

func TestMARuntime_OneFailureOneExhaustedOneSlowSiblingContinues(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(model.CauseUserStop)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name: "M1 local isolation", Objective: "a local failure must not stop the siblings",
	})
	if err != nil {
		t.Fatal(err)
	}
	budget := model.BudgetConfig{
		MaxModelCalls: 40, MaxToolCalls: 40, RunTimeoutSeconds: 30,
		MaxParallelWorkers: 4, WorkerMaxModelCalls: 1, WorkerMaxToolCalls: 4,
		WorkerTimeoutSeconds: 20, WorkerSettlementTimeoutSeconds: 5,
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	if err != nil {
		t.Fatal(err)
	}

	slowEntered := make(chan struct{})
	releaseSlow := make(chan struct{})
	var slowOnce sync.Once
	var plannerRound atomic.Int32
	fake := &testFakeProvider{callFunc: func(callCtx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
		has := func(name string) bool {
			for _, tool := range tools {
				if tool.Name == name {
					return true
				}
			}
			return false
		}
		text := ""
		for _, msg := range messages {
			text += msg.Content
		}
		if has("submit_plan") || has("suggest_completion") {
			if len(messages) > 0 && messages[len(messages)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{Content: "planner turn finished", FinishReason: "stop"}, nil
			}
			if plannerRound.Add(1) != 1 {
				return &provider.ModelResponse{Content: "waiting for the slow direction", FinishReason: "stop"}, nil
			}
			args, _ := json.Marshal(map[string]any{
				"summary": "four directions",
				"items": []map[string]any{
					{"key": "fail_item", "title": "fail", "objective": "fail locally", "work_type": "intent"},
					{"key": "exhaust_item", "title": "exhaust", "objective": "use the local quota", "work_type": "intent"},
					{"key": "slow_item", "title": "slow", "objective": "stay in a model call", "work_type": "intent"},
					{"key": "ok_item", "title": "ok", "objective": "finish while others are unfinished", "work_type": "intent"},
				},
			})
			return &provider.ModelResponse{
				FinishReason: "tool_calls",
				ToolCalls:    []provider.ToolCall{{ID: "plan", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(args)}}},
			}, nil
		}
		switch {
		case bytes.Contains([]byte(text), []byte("fail_item")):
			return nil, provider.ErrProtocol
		case bytes.Contains([]byte(text), []byte("exhaust_item")):
			if bytes.Contains([]byte(text), []byte("局部探索额度")) || (len(messages) > 0 && messages[len(messages)-1].Role == provider.RoleTool) {
				return &provider.ModelResponse{Content: "settled", FinishReason: "stop"}, nil
			}
			args, _ := json.Marshal(map[string]any{"summary": "local note", "detail": "quota", "confidence": "observed", "evidence": "status=200"})
			return &provider.ModelResponse{
				FinishReason: "tool_calls",
				ToolCalls:    []provider.ToolCall{{ID: "fact", Function: provider.FunctionCall{Name: "record_fact", Arguments: string(args)}}},
			}, nil
		case bytes.Contains([]byte(text), []byte("slow_item")):
			slowOnce.Do(func() { close(slowEntered) })
			select {
			case <-releaseSlow:
			case <-callCtx.Done():
				return nil, callCtx.Err()
			}
			return &provider.ModelResponse{Content: "slow finished", FinishReason: "stop"}, nil
		default:
			return &provider.ModelResponse{Content: "ok finished", FinishReason: "stop"}, nil
		}
	}}

	eng := orchestration.NewEngine(run.ID, env.runService, fake, budget)
	done := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel(model.CauseUserStop)
		close(releaseSlow)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()

	select {
	case <-slowEntered:
	case <-time.After(8 * time.Second):
		t.Fatal("slow worker did not enter its model call")
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("siblings did not finish while the slow worker was still in its call")
		}
		items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		status := map[string]model.WorkItemStatus{}
		for _, it := range items {
			status[it.Key] = it.Status
		}
		runNow, err := env.runService.Get(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		if status["ok_item"] == model.WorkItemStatusCompleted &&
			status["fail_item"] == model.WorkItemStatusFailed &&
			status["exhaust_item"] == model.WorkItemStatusExhausted &&
			status["slow_item"] == model.WorkItemStatusRunning &&
			runNow.Status == model.RunStatusRunning {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
}

func TestMARuntime_PauseBlocksAdmission(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "M1 pause", Objective: "do not claim while paused"})
	if err != nil {
		t.Fatal(err)
	}
	budget := model.BudgetConfig{MaxModelCalls: 20, MaxToolCalls: 20, RunTimeoutSeconds: 30, MaxParallelWorkers: 2}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{
		Key: "paused_item", Title: "paused", Objective: "must stay pending", WorkType: "intent",
	}}); err != nil {
		t.Fatal(err)
	}

	fake := &testFakeProvider{callFunc: func(callCtx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
		select {
		case <-callCtx.Done():
			return nil, callCtx.Err()
		case <-time.After(time.Hour):
			return &provider.ModelResponse{Content: "should not run while paused", FinishReason: "stop"}, nil
		}
	}}
	eng := orchestration.NewEngine(run.ID, env.runService, fake, budget, orchestration.WithDebounce(10*time.Millisecond))
	if err := eng.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(done)
	}()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
		}
	}()
	time.Sleep(400 * time.Millisecond)
	items, err := env.runService.ListWorkItemsByRun(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].Status != model.WorkItemStatusPending {
		t.Fatalf("paused admission claimed work: %+v", items)
	}
	if err := eng.Resume(ctx); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		items, err = env.runService.ListWorkItemsByRun(context.Background(), run.ID)
		if err == nil && len(items) == 1 && items[0].Status == model.WorkItemStatusRunning {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("resume did not admit the pending item: %+v", items)
}

func TestMARuntime_DependencyUnblocksAfterPrerequisite(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "M1 dependency", Objective: "B waits for A"})
	if err != nil {
		t.Fatal(err)
	}
	budget := model.BudgetConfig{
		MaxModelCalls: 20, MaxToolCalls: 20, RunTimeoutSeconds: 20,
		MaxParallelWorkers: 2, WorkerMaxModelCalls: 4, WorkerTimeoutSeconds: 15,
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	releaseA := make(chan struct{})
	var releaseAOnce sync.Once
	releaseAFn := func() { releaseAOnce.Do(func() { close(releaseA) }) }
	bStarted := make(chan struct{})
	var bOnce sync.Once
	var plannerRound atomic.Int32
	fake := &testFakeProvider{callFunc: func(callCtx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
		has := func(name string) bool {
			for _, tool := range tools {
				if tool.Name == name {
					return true
				}
			}
			return false
		}
		text := ""
		for _, msg := range messages {
			text += msg.Content
		}
		if has("submit_plan") {
			if len(messages) > 0 && messages[len(messages)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{Content: "planned", FinishReason: "stop"}, nil
			}
			if plannerRound.Add(1) == 1 {
				args, _ := json.Marshal(map[string]any{
					"summary": "dependent directions",
					"items": []map[string]any{
						{"key": "dep_a", "title": "A", "objective": "finish first", "work_type": "intent"},
						{"key": "dep_b", "title": "B", "objective": "start after A", "work_type": "intent", "dependencies": []string{"dep_a"}},
					},
				})
				return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(args)}}}}, nil
			}
			return &provider.ModelResponse{Content: "waiting", FinishReason: "stop"}, nil
		}
		if bytes.Contains([]byte(text), []byte("dep_b")) {
			bOnce.Do(func() { close(bStarted) })
			return &provider.ModelResponse{Content: "B done", FinishReason: "stop"}, nil
		}
		select {
		case <-releaseA:
		case <-callCtx.Done():
			return nil, callCtx.Err()
		}
		return &provider.ModelResponse{Content: "A done", FinishReason: "stop"}, nil
	}}
	eng := orchestration.NewEngine(run.ID, env.runService, fake, budget, orchestration.WithDebounce(10*time.Millisecond))
	done := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(done)
	}()
	defer func() {
		releaseAFn()
		cancel()
		select {
		case <-done:
		case <-time.After(4 * time.Second):
		}
	}()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
		if err != nil {
			t.Fatal(err)
		}
		var aRunning, bPending bool
		for _, it := range items {
			if it.Key == "dep_a" && it.Status == model.WorkItemStatusRunning {
				aRunning = true
			}
			if it.Key == "dep_b" && it.Status == model.WorkItemStatusPending {
				bPending = true
			}
		}
		if aRunning && bPending {
			break
		}
		if time.Now().Add(50 * time.Millisecond).After(deadline) {
			t.Fatalf("dependency was not held: %+v", items)
		}
		time.Sleep(30 * time.Millisecond)
	}
	releaseAFn()
	select {
	case <-bStarted:
	case <-time.After(6 * time.Second):
		t.Fatal("B did not start after A finished")
	}
}

func TestMARuntime_ModelLimitQueuesWithoutFalseFailure(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(model.CauseUserStop)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "M1 model limit", Objective: "queue model calls"})
	if err != nil {
		t.Fatal(err)
	}
	budget := model.BudgetConfig{
		MaxModelCalls: 40, MaxToolCalls: 20, RunTimeoutSeconds: 20,
		MaxParallelWorkers: 4, WorkerMaxModelCalls: 3, WorkerTimeoutSeconds: 15,
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var maxQueued atomic.Int32
	var plannerRound atomic.Int32
	fake := &testFakeProvider{callFunc: func(callCtx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
		has := func(name string) bool {
			for _, tool := range tools {
				if tool.Name == name {
					return true
				}
			}
			return false
		}
		if has("submit_plan") {
			if len(messages) > 0 && messages[len(messages)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{Content: "planned", FinishReason: "stop"}, nil
			}
			if plannerRound.Add(1) == 1 {
				items := make([]map[string]any, 4)
				for i := range items {
					items[i] = map[string]any{
						"key": fmt.Sprintf("slot_%d", i), "title": fmt.Sprintf("slot %d", i),
						"objective": fmt.Sprintf("block in model call %d", i), "work_type": "intent",
					}
				}
				args, _ := json.Marshal(map[string]any{"summary": "four blockers", "items": items})
				return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(args)}}}}, nil
			}
			select {
			case <-callCtx.Done():
				return nil, callCtx.Err()
			case <-time.After(time.Hour):
				return &provider.ModelResponse{Content: "idle", FinishReason: "stop"}, nil
			}
		}
		select {
		case <-callCtx.Done():
			return nil, callCtx.Err()
		case <-time.After(time.Hour):
			return &provider.ModelResponse{Content: "blocked", FinishReason: "stop"}, nil
		}
	}}
	eng := orchestration.NewEngine(run.ID, env.runService, fake, budget,
		orchestration.WithModelConcurrency(2),
		orchestration.WithModelWaitHook(func(queued int) {
			for {
				old := maxQueued.Load()
				if int32(queued) <= old || maxQueued.CompareAndSwap(old, int32(queued)) {
					return
				}
			}
		}),
	)
	done := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(done)
	}()
	deadline := time.Now().Add(8 * time.Second)
	for maxQueued.Load() < 1 && time.Now().Before(deadline) {
		if eng.ModelCapacity().InFlight > 2 {
			t.Fatalf("model inflight exceeded limit: %+v", eng.ModelCapacity())
		}
		time.Sleep(20 * time.Millisecond)
	}
	if maxQueued.Load() < 1 {
		t.Fatalf("model queue was not observed: %+v", eng.ModelCapacity())
	}
	cancel(model.CauseUserStop)
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("cancel did not wake queued model calls")
	}
	final, err := env.runService.Get(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if final.FinishReason == nil || *final.FinishReason == "protocol_error" || *final.FinishReason == "internal_error" {
		t.Fatalf("cancel was reported as a false failure: status=%s reason=%v", final.Status, final.FinishReason)
	}
}

func TestMARuntime_OneRunningAttemptConstraint(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "M1 attempt constraint", Objective: "one running attempt"})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET status='running' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{
		Key: "once", Title: "once", Objective: "single attempt", WorkType: "intent",
	}})
	if err != nil || len(items) != 1 {
		t.Fatalf("submit: %v items=%d", err, len(items))
	}
	if _, err := env.pool.Exec(ctx, `INSERT INTO attempts (work_item_id, attempt_number, status) VALUES ($1, 1, 'running')`, items[0].ID); err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(ctx, `INSERT INTO attempts (work_item_id, attempt_number, status) VALUES ($1, 2, 'running')`, items[0].ID)
	if err == nil {
		t.Fatal("second running attempt was accepted")
	}
	if !isUnique(err) {
		t.Fatalf("expected unique violation, got %v", err)
	}
}

func isUnique(err error) bool {
	return err != nil && (bytes.Contains([]byte(err.Error()), []byte("23505")) || bytes.Contains([]byte(err.Error()), []byte("duplicate")))
}
