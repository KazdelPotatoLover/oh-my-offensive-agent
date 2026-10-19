package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/db"
	"agent-workbench/backend/internal/handler"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/worker"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const testDBURL = "postgres://postgres:postgres@localhost:5433/agent_workbench?sslmode=disable"
const migrationsPath = "../migrations"

type TestEnv struct {
	pool        *pgxpool.Pool
	taskService *service.TaskService
	runService  *service.RunService
	manager     *worker.Manager
	router      *chi.Mux
}

func setupTestEnv(t *testing.T) *TestEnv {
	ctx := context.Background()

	// Run migrations
	if err := db.RunMigrations(testDBURL, migrationsPath); err != nil {
		t.Fatalf("failed migrations: %v", err)
	}

	pool, err := db.Connect(ctx, testDBURL)
	if err != nil {
		t.Fatalf("failed connect to db: %v", err)
	}

	// Clean tables
	_, err = pool.Exec(ctx, "TRUNCATE tasks CASCADE")
	if err != nil {
		t.Fatalf("failed truncate: %v", err)
	}

	taskService := service.NewTaskService(pool)
	runService := service.NewRunService(pool)
	manager := worker.NewManager(pool, runService)

	taskHandler := handler.NewTaskHandler(taskService)
	runHandler := handler.NewRunHandler(runService, taskService, manager)

	r := chi.NewRouter()
	r.Route("/api", func(r chi.Router) {
		r.Get("/tasks", taskHandler.List)
		r.Post("/tasks", taskHandler.Create)
		r.Get("/tasks/{taskID}", taskHandler.Get)
		r.Put("/tasks/{taskID}", taskHandler.Update)
		r.Delete("/tasks/{taskID}", taskHandler.Delete)

		r.Get("/tasks/{taskID}/runs", runHandler.ListByTask)
		r.Post("/tasks/{taskID}/runs", runHandler.Create)

		r.Get("/runs/{runID}", runHandler.Get)
		r.Post("/runs/{runID}/stop", runHandler.Stop)
		r.Get("/runs/{runID}/events", runHandler.GetEvents)
	})

	return &TestEnv{
		pool:        pool,
		taskService: taskService,
		runService:  runService,
		manager:     manager,
		router:      r,
	}
}

func (e *TestEnv) teardown() {
	e.manager.StopAll()
	e.pool.Close()
}

func createTestTask(t *testing.T, env *TestEnv, name string) model.Task {
	body, _ := json.Marshal(map[string]interface{}{
		"name":      name,
		"objective": "Test objective for " + name,
		"scope":     map[string]interface{}{"targets": []string{"127.0.0.1"}},
		"config":    map[string]interface{}{"mode": "aggressive"},
	})
	req := httptest.NewRequest("POST", "/api/tasks", bytes.NewReader(body))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create task expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var task model.Task
	json.NewDecoder(w.Body).Decode(&task)
	return task
}

// 1. 正常完成验证 (Normal completion: 10 steps progress -> completed)
func TestScenario_NormalCompletion(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Normal Task")

	// Start run
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("start run expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var run model.Run
	json.NewDecoder(w.Body).Decode(&run)

	// Wait for worker to complete 10 steps (approx 10-11 seconds)
	t.Logf("Waiting for run %s to complete 10 steps...", run.ID)
	deadline := time.Now().Add(15 * time.Second)
	var finalRun *model.Run
	for time.Now().Before(deadline) {
		time.Sleep(1 * time.Second)
		r, err := env.runService.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("get run failed: %v", err)
		}
		if r.Status.IsTerminal() {
			finalRun = r
			break
		}
	}

	if finalRun == nil || finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected completed status, got: %+v", finalRun)
	}

	if finalRun.FinishReason == nil || *finalRun.FinishReason != "all steps completed" {
		t.Fatalf("expected finish_reason 'all steps completed', got: %v", finalRun.FinishReason)
	}

	// Verify events
	events, err := env.runService.GetEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get events failed: %v", err)
	}

	// Expected events:
	// 1: state_change (nil -> queued)
	// 2: state_change (queued -> running)
	// 3-12: progress (step 1 to 10)
	// 13: state_change (running -> completed)
	if len(events) != 13 {
		t.Fatalf("expected 13 events, got %d", len(events))
	}

	for i, e := range events {
		expectedSeq := i + 1
		if e.Seq != expectedSeq {
			t.Errorf("expected event seq %d, got %d", expectedSeq, e.Seq)
		}
	}

	t.Logf("Normal completion verified successfully with %d events!", len(events))
}

// 2. 重复启动约束验证 (Duplicate start while active -> blocked by DB and backend)
func TestScenario_DuplicateStartBlocked(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Duplicate Test Task")

	// Start run 1
	req1 := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte("{}")))
	w1 := httptest.NewRecorder()
	env.router.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("run 1 expected 201, got %d: %s", w1.Code, w1.Body.String())
	}

	// Immediately try to start run 2 on the same task while run 1 is active
	req2 := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte("{}")))
	w2 := httptest.NewRecorder()
	env.router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("run 2 expected 409 Conflict, got %d: %s", w2.Code, w2.Body.String())
	}

	t.Logf("Duplicate start while active correctly rejected with 409 Conflict!")
}

// 3. 同时启动验证 (Concurrent start with same idempotency key vs without)
func TestScenario_ConcurrentStart(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Concurrent Task")
	idempKey := "idem-concurrent-" + uuid.New().String()

	const numConcurrent = 5
	var wg sync.WaitGroup
	results := make([]int, numConcurrent)
	runs := make([]*model.Run, numConcurrent)

	for i := 0; i < numConcurrent; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			req := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte("{}")))
			req.Header.Set("Idempotency-Key", idempKey)
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, req)
			results[idx] = w.Code
			if w.Code == http.StatusCreated || w.Code == http.StatusOK {
				var r model.Run
				json.NewDecoder(w.Body).Decode(&r)
				runs[idx] = &r
			}
		}(i)
	}
	wg.Wait()

	// All concurrent requests with the SAME idempotency key and SAME payload must return the SAME run!
	var firstRunID uuid.UUID
	for i, code := range results {
		if code != http.StatusCreated && code != http.StatusOK {
			t.Fatalf("concurrent request %d failed with code %d", i, code)
		}
		if firstRunID == uuid.Nil {
			firstRunID = runs[i].ID
		} else if runs[i].ID != firstRunID {
			t.Fatalf("expected identical run ID %s, got %s", firstRunID, runs[i].ID)
		}
	}

	t.Logf("Concurrent start with same idempotency key returned identical run %s for all %d requests!", firstRunID, numConcurrent)
}

// 4. 幂等键冲突验证 (Same idempotency key with different request params -> 409 Conflict)
func TestScenario_IdempotencyConflict(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Idempotency Conflict Task")
	idempKey := "idem-conflict-key"

	// Req 1 with fail_at_step: 3
	req1 := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte(`{"fail_at_step": 3}`)))
	req1.Header.Set("Idempotency-Key", idempKey)
	w1 := httptest.NewRecorder()
	env.router.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("req 1 expected 201, got %d: %s", w1.Code, w1.Body.String())
	}

	// Req 2 with same idempotency key but fail_at_step: 5
	req2 := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte(`{"fail_at_step": 5}`)))
	req2.Header.Set("Idempotency-Key", idempKey)
	w2 := httptest.NewRecorder()
	env.router.ServeHTTP(w2, req2)

	if w2.Code != http.StatusConflict {
		t.Fatalf("req 2 expected 409 Conflict, got %d: %s", w2.Code, w2.Body.String())
	}

	t.Logf("Idempotency conflict detected correctly with 409 status!")
}

// 5. 指定步骤失败验证 (Specified step failure)
func TestScenario_SpecifiedFailure(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Fail Step Task")

	// Start run with fail_at_step = 3
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte(`{"fail_at_step": 3}`)))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var run model.Run
	json.NewDecoder(w.Body).Decode(&run)

	// Wait for failure
	deadline := time.Now().Add(6 * time.Second)
	var finalRun *model.Run
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		r, err := env.runService.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("get run failed: %v", err)
		}
		if r.Status.IsTerminal() {
			finalRun = r
			break
		}
	}

	if finalRun == nil || finalRun.Status != model.RunStatusFailed {
		t.Fatalf("expected failed status, got: %+v", finalRun)
	}

	expectedReason := "failed at step 3 (simulated)"
	if finalRun.FinishReason == nil || *finalRun.FinishReason != expectedReason {
		t.Fatalf("expected finish_reason '%s', got: %v", expectedReason, finalRun.FinishReason)
	}

	// Verify events
	events, err := env.runService.GetEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get events failed: %v", err)
	}

	// Expected events:
	// 1: state_change (queued)
	// 2: state_change (running)
	// 3: progress (step 1)
	// 4: progress (step 2)
	// 5: state_change (failed)
	// (step 3 fails before progress 3)
	lastEvent := events[len(events)-1]
	if lastEvent.Type != "state_change" {
		t.Fatalf("expected last event to be state_change, got %s", lastEvent.Type)
	}

	t.Logf("Specified failure at step 3 verified successfully!")
}

// 6. 停止操作验证 (Stop: running -> stopping -> stopped)
func TestScenario_StopRun(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Stop Task")

	// Start run
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var run model.Run
	json.NewDecoder(w.Body).Decode(&run)

	// Wait 1.5 seconds so it is running and has made 1-2 steps
	time.Sleep(1500 * time.Millisecond)

	// Request stop
	stopReq := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/stop", run.ID), nil)
	stopW := httptest.NewRecorder()
	env.router.ServeHTTP(stopW, stopReq)

	if stopW.Code != http.StatusOK {
		t.Fatalf("stop run expected 200, got %d: %s", stopW.Code, stopW.Body.String())
	}

	var stoppingRun model.Run
	json.NewDecoder(stopW.Body).Decode(&stoppingRun)
	if stoppingRun.Status != model.RunStatusStopping {
		t.Fatalf("expected status stopping immediately after stop request, got: %s", stoppingRun.Status)
	}

	// Wait for worker to exit and transition to stopped
	deadline := time.Now().Add(4 * time.Second)
	var finalRun *model.Run
	for time.Now().Before(deadline) {
		time.Sleep(300 * time.Millisecond)
		r, err := env.runService.Get(context.Background(), run.ID)
		if err != nil {
			t.Fatalf("get run failed: %v", err)
		}
		if r.Status == model.RunStatusStopped {
			finalRun = r
			break
		}
	}

	if finalRun == nil || finalRun.Status != model.RunStatusStopped {
		t.Fatalf("expected status stopped after worker exit, got: %+v", finalRun)
	}

	expectedReason := "user requested stop"
	if finalRun.FinishReason == nil || *finalRun.FinishReason != expectedReason {
		t.Fatalf("expected finish_reason '%s', got: %v", expectedReason, finalRun.FinishReason)
	}

	// Verify events contain both stopping and stopped
	events, err := env.runService.GetEvents(context.Background(), run.ID)
	if err != nil {
		t.Fatalf("get events failed: %v", err)
	}

	var hasStopping, hasStopped bool
	for _, e := range events {
		if e.Type == "state_change" {
			var payload map[string]interface{}
			json.Unmarshal(e.Payload, &payload)
			if payload["to"] == "stopping" {
				hasStopping = true
			}
			if payload["to"] == "stopped" {
				hasStopped = true
			}
		}
	}

	if !hasStopping || !hasStopped {
		t.Fatalf("expected both stopping and stopped events, got stopping=%v, stopped=%v", hasStopping, hasStopped)
	}

	t.Logf("Stop operation verified: running -> stopping -> stopped!")
}

// 7. 重启后的 interrupted 状态及历史保留验证
func TestScenario_RestartInterruptedAndHistory(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Restart Recovery Task")

	// Manually insert active runs into DB (simulating previous server crash / shutdown)
	ctx := context.Background()

	// 1. A queued run
	var queuedRunID uuid.UUID
	err := env.pool.QueryRow(ctx,
		`INSERT INTO runs (task_id, config_snapshot, status) 
		 VALUES ($1, '{}', 'queued') RETURNING id`, task.ID).Scan(&queuedRunID)
	if err != nil {
		t.Fatalf("insert queued run: %v", err)
	}
	_, err = env.pool.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, 1, 'state_change', '{"from":null,"to":"queued"}')`, queuedRunID)
	if err != nil {
		t.Fatalf("insert event: %v", err)
	}

	// Call MarkInterruptedRuns (simulating server startup)
	if err := db.MarkInterruptedRuns(ctx, env.pool); err != nil {
		t.Fatalf("mark interrupted runs: %v", err)
	}

	// Verify run is now interrupted
	r, err := env.runService.Get(ctx, queuedRunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if r.Status != model.RunStatusInterrupted {
		t.Fatalf("expected status interrupted, got %s", r.Status)
	}
	if r.FinishReason == nil || *r.FinishReason != "server restarted" {
		t.Fatalf("expected finish_reason 'server restarted', got: %v", r.FinishReason)
	}

	// Verify event sequence: seq 1 = queued, seq 2 = interrupted
	events, err := env.runService.GetEvents(ctx, queuedRunID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}
	if events[1].Seq != 2 || events[1].Type != "state_change" {
		t.Fatalf("expected event 2 to be state_change, got: %+v", events[1])
	}

	// Now that the previous run is interrupted (terminal), we should be able to start a NEW run on the same task!
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/tasks/%s/runs", task.ID), bytes.NewReader([]byte("{}")))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("start new run after interrupted expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// Verify task history preserves both the old interrupted run and the new run
	listReq := httptest.NewRequest("GET", fmt.Sprintf("/api/tasks/%s/runs", task.ID), nil)
	listW := httptest.NewRecorder()
	env.router.ServeHTTP(listW, listReq)
	if listW.Code != http.StatusOK {
		t.Fatalf("list runs expected 200, got %d", listW.Code)
	}

	var resp struct {
		Runs []model.Run `json:"runs"`
	}
	json.NewDecoder(listW.Body).Decode(&resp)
	if len(resp.Runs) != 2 {
		t.Fatalf("expected 2 runs in history, got %d", len(resp.Runs))
	}

	t.Logf("Restart interrupted state and history preservation verified successfully!")
}

// 8. 终态 Run 不接受迟到进度，不允许回到活动状态验证
func TestScenario_TerminalRunRejectsLateProgressAndReactivation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Terminal Invariant Task")

	// Create and fail a run immediately
	r, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Move to running, then to completed
	reason := "done"
	_, err = env.runService.TransitionState(context.Background(), r.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatalf("transition to running: %v", err)
	}
	_, err = env.runService.TransitionState(context.Background(), r.ID, model.RunStatusRunning, model.RunStatusCompleted, &reason)
	if err != nil {
		t.Fatalf("transition to completed: %v", err)
	}

	// 1. Attempt to insert late progress event into terminal run
	err = env.runService.InsertProgressEvent(context.Background(), r.ID, 5, 10, "late step")
	if err == nil {
		t.Fatalf("expected InsertProgressEvent on terminal run to fail, but it succeeded")
	}

	// 2. Attempt to transition terminal run back to active (e.g. running)
	_, err = env.runService.TransitionState(context.Background(), r.ID, model.RunStatusCompleted, model.RunStatusRunning, nil)
	if err == nil {
		t.Fatalf("expected TransitionState from completed to running to fail, but it succeeded")
	}

	// 3. Attempt to stop an already completed run
	_, err = env.runService.Stop(context.Background(), r.ID)
	if err == nil {
		t.Fatalf("expected Stop on completed run to fail, but it succeeded")
	}

	t.Logf("Terminal run invariant strictly verified: late progress rejected and no reactivation allowed!")
}
