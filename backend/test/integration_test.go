package test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
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

const migrationsPath = "../migrations"

func resolveTestDBURL() (string, error) {
	testURL := os.Getenv("TEST_DATABASE_URL")
	if testURL == "" {
		return "", errors.New("TEST_DATABASE_URL is not set: integration tests must only run against an explicitly configured independent test database and will not fall back to DATABASE_URL")
	}
	return testURL, nil
}

func getTestDBURL(t *testing.T) string {
	url, err := resolveTestDBURL()
	if err != nil {
		t.Fatal(err)
	}
	return url
}


type TestEnv struct {
	pool        *pgxpool.Pool
	taskService *service.TaskService
	runService  *service.RunService
	manager     *worker.Manager
	router      *chi.Mux
}

func setupTestEnv(t *testing.T) *TestEnv {
	ctx := context.Background()

	dbURL := getTestDBURL(t)

	// Run migrations
	if err := db.RunMigrations(dbURL, migrationsPath); err != nil {
		t.Fatalf("failed migrations: %v", err)
	}

	pool, err := db.Connect(ctx, dbURL)
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
	// Default fast step delay in tests for snappy execution
	manager.SetDefaultWorkerOptions(worker.WithStepDelay(20 * time.Millisecond))

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
	e.manager.Close()
	e.manager.StopAll()
	_ = e.manager.Wait(3 * time.Second)
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

	// Wait for worker to complete 10 steps
	deadline := time.Now().Add(5 * time.Second)
	var finalRun *model.Run
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
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
	deadline := time.Now().Add(5 * time.Second)
	var finalRun *model.Run
	for time.Now().Before(deadline) {
		time.Sleep(30 * time.Millisecond)
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

	// Start run with 100ms step delay so we can stop it cleanly mid-flight
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	step1Reached := make(chan struct{})
	err = env.manager.Start(run.ID, nil,
		worker.WithStepDelay(50*time.Millisecond),
		worker.WithOnStepHook(func(step int) {
			if step == 1 {
				select {
				case <-step1Reached:
				default:
					close(step1Reached)
				}
			}
		}),
	)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}

	// Wait deterministically for step 1
	<-step1Reached

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
		time.Sleep(30 * time.Millisecond)
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

	// Now that the previous run is interrupted (terminal), we can start a NEW run on the same task!
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

	// Create and complete a run
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

// 9. 缺少测试配置时拒绝运行验证 (Requirement 4)
func TestRequirement4_RefusesWithoutTestDatabaseConfig(t *testing.T) {
	orig := os.Getenv("TEST_DATABASE_URL")
	defer os.Setenv("TEST_DATABASE_URL", orig)

	os.Unsetenv("TEST_DATABASE_URL")

	_, err := resolveTestDBURL()
	if err == nil {
		t.Fatal("expected resolveTestDBURL to return error when TEST_DATABASE_URL is missing")
	}
	t.Logf("Requirement 4 verified: %v", err)
}

// 10. 停止早于 Worker 注册、停止意图保存后的收尾验证 (Requirement 1)
func TestScenario_StopBeforeWorkerRegistration(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Early Stop Task")
	ctx := context.Background()

	// Create run in DB without starting manager yet
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if run.Status != model.RunStatusQueued {
		t.Fatalf("expected queued status, got %s", run.Status)
	}

	// 1. User requests stop before worker registration
	stoppedRun, err := env.runService.Stop(ctx, run.ID)
	if err != nil {
		t.Fatalf("stop run: %v", err)
	}
	if stoppedRun.Status != model.RunStatusStopping {
		t.Fatalf("expected status stopping after stop request, got %s", stoppedRun.Status)
	}

	// Inform manager (which marks pendingCancel)
	env.manager.Stop(run.ID)

	// 2. Worker is registered/started now
	err = env.manager.Start(run.ID, nil, worker.WithStepDelay(10*time.Millisecond))
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}

	// Wait for worker to exit
	err = env.manager.Wait(3 * time.Second)
	if err != nil {
		t.Fatalf("wait worker: %v", err)
	}

	// 3. Verify the run reached stopped terminal status and is NOT permanently stuck in stopping
	finalRun, err := env.runService.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if finalRun.Status != model.RunStatusStopped {
		t.Fatalf("expected run to reach terminal status stopped, got: %s", finalRun.Status)
	}
	if finalRun.FinishReason == nil || *finalRun.FinishReason != "user requested stop" {
		t.Fatalf("expected finish_reason 'user requested stop', got: %v", finalRun.FinishReason)
	}

	// Verify the task slot is released by starting another run
	newRun, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("expected to be able to start new run after stopped, got error: %v", err)
	}
	if newRun.Status != model.RunStatusQueued {
		t.Fatalf("expected new run to be queued, got: %s", newRun.Status)
	}

	t.Log("Requirement 1 verified: stop earlier than worker registration properly converges to stopped terminal state")
}

// 11. 停止与进度写入竞争时的收尾逻辑验证 (Requirement 1 & 6)
func TestScenario_StopProgressWriteRace(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Race Stop Task")
	ctx := context.Background()

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	step1Triggered := make(chan struct{})
	releaseStep1 := make(chan struct{})

	// Start worker with deterministic synchronization hook
	err = env.manager.Start(run.ID, nil,
		worker.WithStepDelay(20*time.Millisecond),
		worker.WithOnStepHook(func(step int) {
			if step == 1 {
				select {
				case <-step1Triggered:
				default:
					close(step1Triggered)
				}
				<-releaseStep1
			}
		}),
	)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}

	// Wait deterministically for step 1
	select {
	case <-step1Triggered:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for step 1 hook")
	}

	// Request stop while worker is at step 1
	stoppedRun, err := env.runService.Stop(ctx, run.ID)
	if err != nil {
		t.Fatalf("stop run: %v", err)
	}
	if stoppedRun.Status != model.RunStatusStopping {
		t.Fatalf("expected status stopping, got %s", stoppedRun.Status)
	}
	env.manager.Stop(run.ID)

	// Release worker to execute progress write or step 2
	close(releaseStep1)

	// Wait for worker to exit
	err = env.manager.Wait(3 * time.Second)
	if err != nil {
		t.Fatalf("worker did not exit cleanly: %v", err)
	}

	// Verify run reached terminal stopped status
	finalRun, err := env.runService.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if finalRun.Status != model.RunStatusStopped {
		t.Fatalf("expected status stopped, got: %s", finalRun.Status)
	}

	// Verify events: ensure no events were written after 'stopped'
	events, err := env.runService.GetEvents(ctx, run.ID)
	if err != nil {
		t.Fatalf("get events: %v", err)
	}
	lastEvent := events[len(events)-1]
	if lastEvent.Type != "state_change" {
		t.Fatalf("expected last event to be state_change (stopped), got %s", lastEvent.Type)
	}
	var payload map[string]interface{}
	json.Unmarshal(lastEvent.Payload, &payload)
	if payload["to"] != "stopped" {
		t.Fatalf("expected last event target to be stopped, got: %v", payload["to"])
	}

	t.Log("Requirement 1 & 6 verified: stop racing with progress write safely exits and enters stopped terminal state")
}

// 12. 显式状态转换规则及非法转换禁止验证 (Requirement 2)
func TestScenario_InvalidStateTransitionsRejected(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "State Machine Task")
	ctx := context.Background()

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// 1. queued -> completed: FORBIDDEN
	reason := "skip"
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusCompleted, &reason)
	if !errors.Is(err, service.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for queued->completed, got: %v", err)
	}

	// 2. queued -> running: VALID
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatalf("queued->running should succeed, got: %v", err)
	}

	// 3. running -> queued: FORBIDDEN (cannot rewind)
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusQueued, nil)
	if !errors.Is(err, service.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for running->queued, got: %v", err)
	}

	// 4. running -> completed: VALID
	doneReason := "done"
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusCompleted, &doneReason)
	if err != nil {
		t.Fatalf("running->completed should succeed, got: %v", err)
	}

	// 5. completed -> running: FORBIDDEN (terminal cannot be reactivated)
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusCompleted, model.RunStatusRunning, nil)
	if !errors.Is(err, service.ErrInvalidTransition) && !errors.Is(err, service.ErrTerminalState) {
		t.Fatalf("expected ErrInvalidTransition or ErrTerminalState for completed->running, got: %v", err)
	}

	// 6. completed -> queued: FORBIDDEN
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusCompleted, model.RunStatusQueued, nil)
	if !errors.Is(err, service.ErrInvalidTransition) && !errors.Is(err, service.ErrTerminalState) {
		t.Fatalf("expected ErrInvalidTransition or ErrTerminalState for completed->queued, got: %v", err)
	}

	// 6b. When DB row is terminal, attempting transition from running->failed triggers ErrTerminalState
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusFailed, nil)
	if !errors.Is(err, service.ErrTerminalState) {
		t.Fatalf("expected ErrTerminalState when DB row is already terminal, got: %v", err)
	}

	// Test stopping -> running on a second run
	run2, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}
	_, err = env.runService.TransitionState(ctx, run2.ID, model.RunStatusQueued, model.RunStatusStopping, nil)
	if err != nil {
		t.Fatalf("queued->stopping should succeed, got: %v", err)
	}

	// 7. stopping -> running: FORBIDDEN
	_, err = env.runService.TransitionState(ctx, run2.ID, model.RunStatusStopping, model.RunStatusRunning, nil)
	if !errors.Is(err, service.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for stopping->running, got: %v", err)
	}

	// 8. stopping -> completed: FORBIDDEN
	_, err = env.runService.TransitionState(ctx, run2.ID, model.RunStatusStopping, model.RunStatusCompleted, &doneReason)
	if !errors.Is(err, service.ErrInvalidTransition) {
		t.Fatalf("expected ErrInvalidTransition for stopping->completed, got: %v", err)
	}

	// stopping -> stopped: VALID
	stopReason := "stopped"
	_, err = env.runService.TransitionState(ctx, run2.ID, model.RunStatusStopping, model.RunStatusStopped, &stopReason)
	if err != nil {
		t.Fatalf("stopping->stopped should succeed, got: %v", err)
	}

	// 9. stopped -> running: FORBIDDEN (terminal)
	_, err = env.runService.TransitionState(ctx, run2.ID, model.RunStatusStopped, model.RunStatusRunning, nil)
	if !errors.Is(err, service.ErrInvalidTransition) && !errors.Is(err, service.ErrTerminalState) {
		t.Fatalf("expected ErrInvalidTransition or ErrTerminalState for stopped->running, got: %v", err)
	}

	t.Log("Requirement 2 verified: explicit state machine transition rules and illegal transition rejections")
}

// 13. 完整执行输入快照及历史数据完整性验证 (Requirement 3)
func TestScenario_ExecutionInputSnapshotPreserved(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	// 1. Create a task with specific objective, scope, and config
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Snapshot Task",
		Objective: "Initial Objective",
		Scope:     json.RawMessage(`{"env":"production","services":["auth","api"]}`),
		Config:    json.RawMessage(`{"max_depth":3,"timeout_sec":30}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 2. Create Run 1 with fail_at_step: 4
	failAt := 4
	run1, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{FailAtStep: &failAt}, nil)
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}

	// Inspect execution_input of run1
	var input1 map[string]interface{}
	if err := json.Unmarshal(run1.ExecutionInput, &input1); err != nil {
		t.Fatalf("unmarshal execution_input: %v", err)
	}

	if input1["objective"] != "Initial Objective" {
		t.Fatalf("expected objective 'Initial Objective', got: %v", input1["objective"])
	}
	params, _ := input1["parameters"].(map[string]interface{})
	if params["fail_at_step"] != float64(4) {
		t.Fatalf("expected fail_at_step 4, got: %v", params["fail_at_step"])
	}

	// Transition run1 to terminal state so task slot is released
	reason := "completed"
	_, _ = env.runService.TransitionState(ctx, run1.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	_, _ = env.runService.TransitionState(ctx, run1.ID, model.RunStatusRunning, model.RunStatusCompleted, &reason)

	// 3. Modify the Task (change objective, scope, config)
	newObj := "Modified Objective"
	newScope := json.RawMessage(`{"env":"staging"}`)
	newCfg := json.RawMessage(`{"max_depth":10}`)
	_, err = env.taskService.Update(ctx, task.ID, model.UpdateTaskRequest{
		Objective: &newObj,
		Scope:     &newScope,
		Config:    &newCfg,
	})
	if err != nil {
		t.Fatalf("update task: %v", err)
	}

	// 4. Verify historical Run 1 input snapshot is STILL UNCHANGED
	refetchedRun1, err := env.runService.Get(ctx, run1.ID)
	if err != nil {
		t.Fatalf("get run 1: %v", err)
	}
	var refetchedInput1 map[string]interface{}
	json.Unmarshal(refetchedRun1.ExecutionInput, &refetchedInput1)
	if refetchedInput1["objective"] != "Initial Objective" {
		t.Fatalf("historical Run 1 input objective was corrupted! expected 'Initial Objective', got %v", refetchedInput1["objective"])
	}

	// 5. Create Run 2 on modified task and verify it captures the new objective
	run2, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}
	var input2 map[string]interface{}
	json.Unmarshal(run2.ExecutionInput, &input2)
	if input2["objective"] != "Modified Objective" {
		t.Fatalf("expected Run 2 to have 'Modified Objective', got: %v", input2["objective"])
	}

	// 6. Test historical missing data handling in migration 004
	// Insert a simulated legacy run that has no execution_input (or runs the migration UPDATE)
	var legacyRunID uuid.UUID
	err = env.pool.QueryRow(ctx,
		`INSERT INTO runs (task_id, config_snapshot, status, fail_at_step, execution_input) 
		 VALUES ($1, '{"mode":"legacy"}', 'completed', 2, '{}') RETURNING id`, task.ID).Scan(&legacyRunID)
	if err != nil {
		t.Fatalf("insert legacy run: %v", err)
	}

	// Execute migration 004 update logic on this legacy row
	_, err = env.pool.Exec(ctx, `
		UPDATE runs
		SET execution_input = jsonb_build_object(
			'objective', '[historical_unavailable]',
			'scope', jsonb_build_object('_unavailable', true, 'reason', 'not captured prior to migration 004'),
			'config', COALESCE(config_snapshot, '{}'::jsonb),
			'parameters', jsonb_build_object('fail_at_step', fail_at_step),
			'_historical_missing', true,
			'_note', 'Historical execution input was not captured at run creation time prior to migration 004'
		) WHERE id = $1`, legacyRunID)
	if err != nil {
		t.Fatalf("run migration update: %v", err)
	}

	legacyRun, err := env.runService.Get(ctx, legacyRunID)
	if err != nil {
		t.Fatalf("get legacy run: %v", err)
	}
	var legacyInput map[string]interface{}
	json.Unmarshal(legacyRun.ExecutionInput, &legacyInput)
	if legacyInput["_historical_missing"] != true {
		t.Fatalf("expected _historical_missing = true, got %v", legacyInput["_historical_missing"])
	}
	if legacyInput["objective"] != "[historical_unavailable]" {
		t.Fatalf("expected [historical_unavailable], got %v", legacyInput["objective"])
	}

	t.Log("Requirement 3 verified: execution input snapshot is immutable across task updates, and historical missing data is explicitly marked")
}

// 14. Worker 管理器等待退出与按序关闭验证 (Requirement 5)
func TestScenario_WorkerManagerShutdownSequence(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()
	task := createTestTask(t, env, "Shutdown Test Task")

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	stepTriggered := make(chan struct{})
	mgr := worker.NewManager(env.pool, env.runService)

	// Start worker that pauses at step 1
	err = mgr.Start(run.ID, nil,
		worker.WithStepDelay(100*time.Millisecond),
		worker.WithOnStepHook(func(step int) {
			if step == 1 {
				select {
				case <-stepTriggered:
				default:
					close(stepTriggered)
				}
			}
		}),
	)
	if err != nil {
		t.Fatalf("start worker: %v", err)
	}

	<-stepTriggered

	// Step 1: Stop receiving work
	mgr.Close()

	// Verify that starting new work is immediately rejected
	dummyID := uuid.New()
	err = mgr.Start(dummyID, nil)
	if !errors.Is(err, worker.ErrManagerClosed) {
		t.Fatalf("expected ErrManagerClosed when starting after Close(), got: %v", err)
	}

	// Step 2: Cancel executions
	mgr.StopAll()

	// Step 3: Wait for all workers to exit (with wait limit)
	err = mgr.Wait(3 * time.Second)
	if err != nil {
		t.Fatalf("wait for workers failed: %v", err)
	}

	// Step 4: Verify the run reached stopped terminal status
	finalRun, err := env.runService.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if finalRun.Status != model.RunStatusStopped {
		t.Fatalf("expected run to be stopped, got: %s", finalRun.Status)
	}

	t.Log("Requirement 5 verified: worker manager orderly shutdown: stop receiving work -> cancel execution -> wait exit -> close")
}
