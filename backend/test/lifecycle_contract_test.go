package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

type boundaryProvider struct {
	entered, release chan struct{}
	once             sync.Once
	failSummary      bool
	summaryDelay     time.Duration
	summaryEntered   chan struct{}
	summaryReason    string
	ignoreCancel     bool
	summaries        atomic.Int32
	workers          atomic.Int32
}

func (p *boundaryProvider) Call(ctx context.Context, messages []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
	if len(defs) == 0 {
		p.summaries.Add(1)
		if p.summaryEntered != nil {
			close(p.summaryEntered)
		}
		if p.summaryDelay > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(p.summaryDelay):
			}
		}
		if p.failSummary {
			return nil, errors.New("summary unavailable")
		}
		if p.summaryReason != "" {
			if p.summaryReason == "tools" {
				return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "forbidden", Type: "function", Function: provider.FunctionCall{Name: "read_observations", Arguments: `{}`}}}}, nil
			}
			return &provider.ModelResponse{FinishReason: p.summaryReason, Content: "No summary"}, nil
		}
		return &provider.ModelResponse{FinishReason: "stop", Content: "Partial execution summary."}, nil
	}
	for _, d := range defs {
		if d.Name == "submit_plan" {
			if messages[len(messages)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{FinishReason: "stop", Content: "Plan submitted."}, nil
			}
			return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: `{"items":[{"key":"one","title":"First","objective":"Read observations","work_type":"aggregate_summary"},{"key":"two","title":"Second","objective":"Read observations","work_type":"aggregate_summary"}]}`}}}}, nil
		}
	}
	p.workers.Add(1)
	p.once.Do(func() { close(p.entered) })
	if p.ignoreCancel {
		<-p.release
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "late", Type: "function", Function: provider.FunctionCall{Name: "read_observations", Arguments: `{}`}}}}, nil
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-p.release:
	}
	return &provider.ModelResponse{FinishReason: "stop", Content: "Finished the allowed work."}, nil
}

func assertNoActiveChildren(t *testing.T, env *TestEnv, id uuid.UUID) {
	t.Helper()
	queries := []string{
		`SELECT COUNT(*) FROM work_items WHERE run_id=$1 AND status IN ('pending','running','blocked')`,
		`SELECT COUNT(*) FROM attempts WHERE work_item_id IN (SELECT id FROM work_items WHERE run_id=$1) AND status='running'`,
		`SELECT COUNT(*) FROM agent_sessions WHERE run_id=$1 AND status='running'`,
		`SELECT COUNT(*) FROM model_calls WHERE run_id=$1 AND status='started'`,
		`SELECT COUNT(*) FROM agent_tool_calls WHERE run_id=$1 AND status IN ('pending','running')`,
		`SELECT COUNT(*) FROM planner_rounds WHERE run_id=$1 AND status='running'`,
	}
	for _, q := range queries {
		var n int
		if err := env.pool.QueryRow(context.Background(), q, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n != 0 {
			t.Fatalf("%d active children remain: %s", n, q)
		}
	}
}

func TestLifecycle_LastAllowedCallAndSummaryFallback(t *testing.T) {
	for _, failSummary := range []bool{false, true} {
		t.Run(map[bool]string{false: "summary", true: "fallback"}[failSummary], func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			task := createTestTask(t, env, "last allowed call")
			run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			p := &boundaryProvider{entered: make(chan struct{}), release: make(chan struct{}), failSummary: failSummary}
			budget := model.DefaultBudgetConfig()
			budget.MaxModelCalls = 4
			budget.RunTimeoutSeconds = 15
			done := make(chan struct{})
			go func() {
				orchestration.NewEngine(run.ID, env.runService, p, budget).Run(context.Background())
				close(done)
			}()
			select {
			case <-p.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("worker never started")
			}
			// A negative assertion while the final permitted exploration call is held.
			select {
			case <-done:
				t.Fatal("run ended while last allowed call was in flight")
			case <-time.After(100 * time.Millisecond):
			}
			before, err := env.runService.Get(context.Background(), run.ID)
			if err != nil || before.Status != model.RunStatusRunning {
				t.Fatalf("premature terminal status: %+v, %v", before, err)
			}
			close(p.release)
			select {
			case <-done:
			case <-time.After(4 * time.Second):
				t.Fatal("wrapup did not finish")
			}
			final, err := env.runService.Get(context.Background(), run.ID)
			if err != nil || final.Status != model.RunStatusStopped || final.FinishReason == nil || *final.FinishReason != "model_calls_exhausted" {
				t.Fatalf("final=%+v err=%v", final, err)
			}
			if p.summaries.Load() != 1 || p.workers.Load() != 1 {
				t.Fatalf("summaries=%d workers=%d", p.summaries.Load(), p.workers.Load())
			}
			if final.FinalOutput == nil || !strings.Contains(*final.FinalOutput, "Completed Work Items") {
				t.Fatal("missing deterministic partial inventory")
			}
			var completed, calls int
			if err = env.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM work_items WHERE run_id=$1 AND status='completed'`, run.ID).Scan(&completed); err != nil {
				t.Fatal(err)
			}
			if err = env.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM model_calls WHERE run_id=$1`, run.ID).Scan(&calls); err != nil {
				t.Fatal(err)
			}
			if completed != 1 || calls != 4 {
				t.Fatalf("completed=%d calls=%d", completed, calls)
			}
			assertNoActiveChildren(t, env, run.ID)
		})
	}
}

func TestLifecycle_TerminalParentRepairAndLateWrites(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "repair terminal children")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "one", Title: "One", Objective: "Work"}, {Key: "two", Title: "Two", Objective: "Work"}}); err != nil {
		t.Fatal(err)
	}
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wi == nil {
		t.Fatal(err)
	}
	mc, err := env.runService.CreateModelCallWithSession(ctx, run.ID, &sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	tc, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, &mc.ID, "late", "read_observations", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	reason := "model_calls_exhausted"
	if _, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusFailed, &reason); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err = env.runService.FinalizeRunWrapup(ctx, run.ID, model.CauseUserStop, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, true, json.RawMessage(`{"summary":"late"}`), "completed"); err != nil {
		t.Fatal(err)
	}
	if err = env.runService.UpdateModelCall(ctx, mc.ID, "completed", nil, nil, 0, 0, 0, nil); err != nil {
		t.Fatal(err)
	}
	if err = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", "late", nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil); err == nil {
		t.Fatal("late active session admitted")
	}
	if _, err = env.runService.CreateModelCall(ctx, run.ID, 2); err == nil {
		t.Fatal("late active model call admitted")
	}
	if _, err = env.runService.CreateAgentToolCall(ctx, run.ID, nil, "new", "read_observations", json.RawMessage(`{}`)); err == nil {
		t.Fatal("late active tool call admitted")
	}
	final, _ := env.runService.Get(ctx, run.ID)
	if final.Status != model.RunStatusFailed || final.FinishReason == nil || *final.FinishReason != reason {
		t.Fatal("terminal outcome overwritten")
	}
	stored, _ := env.runService.ListModelCalls(ctx, run.ID)
	if len(stored) != 1 || stored[0].Status != "interrupted" {
		t.Fatal("late model response overwrote interruption")
	}
	storedTools, _ := env.runService.ListAgentToolCalls(ctx, run.ID)
	if len(storedTools) != 1 || storedTools[0].Status != "interrupted" {
		t.Fatal("late tool response overwrote interruption")
	}
	assertNoActiveChildren(t, env, run.ID)
}

func TestLifecycle_StartupRecoveryPreservesStopIntent(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "stopping at restart")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.Stop(ctx, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.RecoverActiveRuns(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.RecoverActiveRuns(ctx); err != nil {
		t.Fatal(err)
	}
	final, _ := env.runService.Get(ctx, run.ID)
	if final.Status != model.RunStatusStopped || final.FinishReason == nil || *final.FinishReason != "user_requested_stop" {
		t.Fatalf("lost stop intent: %+v", final)
	}
	assertNoActiveChildren(t, env, run.ID)
}

func TestLifecycle_BoundedDrainAndLateProviderResponse(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	task := createTestTask(t, env, "uncooperative provider")
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	p := &boundaryProvider{entered: make(chan struct{}), release: make(chan struct{}), ignoreCancel: true}
	budget := model.DefaultBudgetConfig()
	ctx, cancel := context.WithCancelCause(context.Background())
	defer cancel(model.CauseShutdown)
	done, workerDone := make(chan struct{}), make(chan struct{})
	engine := orchestration.NewEngine(run.ID, env.runService, p, budget, orchestration.WithWorkerActiveHook(func(delta int) {
		if delta < 0 {
			close(workerDone)
		}
	}))
	go func() { engine.Run(ctx); close(done) }()
	select {
	case <-p.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker not entered")
	}
	// Release the deliberately uncooperative stub even when this test fails.
	defer func() {
		close(p.release)
		select {
		case <-workerDone:
		case <-time.After(3 * time.Second):
			t.Error("late worker did not exit")
		}
		assertNoActiveChildren(t, env, run.ID)
		// Only the initial submit_plan tool is present; the late response must not
		// execute its requested read_observations call after terminalization.
		var calls int
		if err := env.pool.QueryRow(context.Background(), `SELECT COUNT(*) FROM agent_tool_calls WHERE run_id=$1`, run.ID).Scan(&calls); err != nil {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatalf("unexpected tool calls: %d", calls)
		}
	}()
	cancel(model.CauseTimeout)
	select {
	case <-done:
	case <-time.After(7 * time.Second):
		t.Fatal("drain exceeded its grace period")
	}
	final, err := env.runService.Get(context.Background(), run.ID)
	if err != nil || final.Status != model.RunStatusStopped || final.FinishReason == nil || *final.FinishReason != "timeout" {
		t.Fatalf("final=%+v error=%v", final, err)
	}
	assertNoActiveChildren(t, env, run.ID)

}

func TestLifecycle_ConcurrentFinalizationPreservesWinner(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "competing finalizers")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 3)
	for _, cause := range []model.TerminationCause{model.CauseTimeout, model.CauseUserStop, model.CauseCompleted} {
		go func(c model.TerminationCause) { <-start; errs <- env.runService.FinalizeRunWrapup(ctx, run.ID, c, nil) }(cause)
	}
	close(start)
	for i := 0; i < 3; i++ {
		if err := <-errs; err != nil {
			t.Fatal(err)
		}
	}
	winner, err := env.runService.Get(ctx, run.ID)
	if err != nil || !winner.Status.IsTerminal() || winner.FinishReason == nil {
		t.Fatalf("no winner: %+v, %v", winner, err)
	}
	for _, cause := range []model.TerminationCause{model.CauseTimeout, model.CauseUserStop, model.CauseCompleted} {
		if err := env.runService.FinalizeRunWrapup(ctx, run.ID, cause, nil); err != nil {
			t.Fatal(err)
		}
	}
	final, _ := env.runService.Get(ctx, run.ID)
	if final.Status != winner.Status || *final.FinishReason != *winner.FinishReason {
		t.Fatal("winner overwritten")
	}
	assertNoActiveChildren(t, env, run.ID)
}

func TestLifecycle_RejectedExecutorDoesNotLeaveQueuedRun(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	task := createTestTask(t, env, "closed manager admission")
	env.manager.Close()
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID.String()+"/runs", strings.NewReader(`{}`)))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	runs, err := env.runService.ListByTask(context.Background(), task.ID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("runs=%d error=%v", len(runs), err)
	}
	if runs[0].Status != model.RunStatusInterrupted || runs[0].FinishReason == nil || *runs[0].FinishReason != "shutdown" {
		t.Fatalf("stranded rejected execution: %+v", runs[0])
	}
	assertNoActiveChildren(t, env, runs[0].ID)
}
