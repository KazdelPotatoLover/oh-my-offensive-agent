package test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
)

type cancellationProvider struct {
	waitAt  string
	entered chan struct{}
	once    sync.Once
	mock    *provider.MockProvider
}

func (p *cancellationProvider) Call(ctx context.Context, messages []provider.ChatMessage, defs []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	planner := false
	for _, d := range defs {
		if d.Name == "submit_plan" {
			planner = true
		}
	}
	if p.waitAt == "single" || (p.waitAt == "planner" && planner) || (p.waitAt == "worker" && !planner) {
		p.once.Do(func() { close(p.entered) })
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return p.mock.Call(ctx, messages, defs, opts)
}

func TestCancellation_DeadlineAndUserStopCloseRunAndChildren(t *testing.T) {
	for _, tc := range []struct {
		name, phase      string
		userStop, single bool
	}{
		{"planner_timeout", "planner", false, false},
		{"worker_timeout", "worker", false, false},
		{"worker_user_stop", "worker", true, false},
		{"single_agent_timeout", "single", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			task := createTestTask(t, env, tc.name)
			run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			budget := model.DefaultBudgetConfig()
			budget.RunTimeoutSeconds = 1
			budget.ModelTimeoutSeconds = 10
			p := &cancellationProvider{waitAt: tc.phase, entered: make(chan struct{}), mock: provider.NewMockProvider(provider.ScenarioOrchestration)}
			if tc.single {
				err = env.manager.StartExecutor(run.ID, agent.NewAgentRunner(run.ID, env.runService, p, nil, budget))
			} else {
				err = env.manager.StartExecutor(run.ID, orchestration.NewEngine(run.ID, env.runService, p, budget))
			}
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-p.entered:
			case <-time.After(3 * time.Second):
				t.Fatal("provider did not start")
			}
			if tc.userStop {
				w := httptest.NewRecorder()
				env.router.ServeHTTP(w, httptest.NewRequest("POST", "/api/runs/"+run.ID.String()+"/stop", nil))
				if w.Code != 200 {
					t.Fatalf("stop: %d %s", w.Code, w.Body.String())
				}
			}
			final := waitForRunTerminal(t, env, run.ID, 4*time.Second)
			if err := env.manager.Wait(3 * time.Second); err != nil {
				t.Fatal(err)
			}
			reason := "timeout"
			if tc.userStop {
				reason = "user_requested_stop"
			}
			if final.Status != model.RunStatusStopped || final.FinishReason == nil || *final.FinishReason != reason {
				t.Fatalf("bad run cancellation: status=%s reason=%v", final.Status, final.FinishReason)
			}
			sessions, err := env.runService.ListSessionsByRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			stopped := false
			for _, s := range sessions {
				if s.Status == "running" || s.Status == "failed" {
					t.Fatalf("cancelled session stranded or overwritten: %+v", s)
				}
				if s.Status == "stopped" {
					stopped = true
					if s.FinishReason == nil || *s.FinishReason != reason {
						t.Fatalf("wrong session reason: %+v", s)
					}
				}
			}
			if !stopped {
				t.Fatal("no stopped session")
			}
			items, err := env.runService.ListWorkItemsByRun(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range items {
				if item.Status != model.WorkItemStatusCancelled {
					t.Fatalf("unfinished item not cancelled: %s", item.Status)
				}
				atts, err := env.runService.ListAttemptsByWorkItem(context.Background(), item.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, att := range atts {
					if att.Status != "stopped" || att.FinishReason == nil || *att.FinishReason != reason {
						t.Fatalf("attempt not stopped: %+v", att)
					}
				}
			}
			events, err := env.runService.GetEvents(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			transitions := []map[string]string{}
			for _, event := range events {
				if event.Type == "state_change" {
					var e map[string]string
					_ = json.Unmarshal(event.Payload, &e)
					transitions = append(transitions, e)
				}
			}
			if len(transitions) < 2 || transitions[len(transitions)-2]["to"] != "stopping" || transitions[len(transitions)-1]["to"] != "stopped" {
				t.Fatalf("illegal cancellation transitions: %+v", transitions)
			}
			if _, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil); err != nil {
				t.Fatalf("task slot not released: %v", err)
			}
		})
	}
}

func TestCancellation_StopWithoutExecutorAndLateWorkerResult(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "orphaned executor")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil); err != nil {
		t.Fatal(err)
	}
	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "sample", Title: "Sample", Objective: "Inspect", WorkType: "analyze_sample", InputData: map[string]interface{}{"sample_id": "sample_home"}}})
	if err != nil {
		t.Fatal(err)
	}
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wi == nil {
		t.Fatalf("claim: %v", err)
	}
	mc, err := env.runService.CreateModelCallWithSession(ctx, run.ID, &sess.ID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, &mc.ID, "pending_tool", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`)); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, httptest.NewRequest("POST", "/api/runs/"+run.ID.String()+"/stop", nil))
	if w.Code != 200 {
		t.Fatalf("stop orphan: %d %s", w.Code, w.Body.String())
	}
	final := waitForRunTerminal(t, env, run.ID, time.Second)
	if final.Status != model.RunStatusStopped {
		t.Fatal(final.Status)
	}
	if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, true, json.RawMessage(`{"late":true}`), "completed"); err != nil {
		t.Fatal(err)
	}
	if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, false, nil, "user_requested_stop"); err != nil {
		t.Fatal(err)
	}
	lateReason := "late_provider_failure"
	if err = env.runService.UpdateSessionStatus(ctx, sess.ID, "failed", &lateReason); err != nil {
		t.Fatal(err)
	}
	updated, err := env.runService.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Status != "stopped" || updated.FinishReason == nil || *updated.FinishReason != "user_requested_stop" {
		t.Fatalf("late result corrupted session: %+v", updated)
	}
	got, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil || len(items) != 1 || len(got) != 1 || got[0].Status != model.WorkItemStatusCancelled {
		t.Fatalf("work status: %+v %v", got, err)
	}
	models, _ := env.runService.ListModelCalls(ctx, run.ID)
	tools, _ := env.runService.ListAgentToolCalls(ctx, run.ID)
	if models[0].Status != "interrupted" || tools[0].Status != "interrupted" {
		t.Fatal("in-flight calls left active")
	}
	if _, err = env.runService.CompleteRunWithOutput(ctx, run.ID, "completed", "late answer"); err == nil {
		t.Fatal("late run completion accepted")
	}
}

type exitOnReleaseExecutor struct{ release <-chan struct{} }

func TestCancellation_StopRacesWithQueuedToRunning(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	for i := 0; i < 20; i++ {
		task := createTestTask(t, env, "start-stop race")
		run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		start := make(chan struct{})
		done := make(chan struct{})
		go func() {
			defer close(done)
			<-start
			_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
		}()
		close(start)
		if _, err = env.runService.Stop(ctx, run.ID); err != nil {
			t.Fatalf("stop lost race with startup: %v", err)
		}
		<-done
		if err = env.runService.FinalizeCancelledRun(ctx, run.ID, "user_requested_stop"); err != nil {
			t.Fatal(err)
		}
		if got, err := env.runService.Get(ctx, run.ID); err != nil || got.Status != model.RunStatusStopped {
			t.Fatalf("run did not stop: %+v %v", got, err)
		}
	}
}

func (e exitOnReleaseExecutor) Run(ctx context.Context) { <-e.release }

func TestCancellation_StopRacingWithExecutorExit(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	for i := 0; i < 8; i++ {
		task := createTestTask(t, env, "executor exit race")
		run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil); err != nil {
			t.Fatal(err)
		}
		release := make(chan struct{})
		if err = env.manager.StartExecutor(run.ID, exitOnReleaseExecutor{release}); err != nil {
			t.Fatal(err)
		}
		done := make(chan int, 1)
		go func() {
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, httptest.NewRequest("POST", "/api/runs/"+run.ID.String()+"/stop", nil))
			done <- w.Code
		}()
		close(release)
		if code := <-done; code != 200 {
			t.Fatalf("stop race: %d", code)
		}
		if got := waitForRunTerminal(t, env, run.ID, time.Second); got.Status != model.RunStatusStopped {
			t.Fatalf("stranded run: %s", got.Status)
		}
	}
}
