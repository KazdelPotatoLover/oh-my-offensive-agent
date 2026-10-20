package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

func TestDiscoveryHTTPEntry_UsesRecordedHardCapsForMock(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "tiny discovery budget", Objective: "Plan within one call", Scope: json.RawMessage(`{"mode":"discovery","seeds":["http://127.0.0.1:29999/"],"discovery_policy":{"mode":"same_origin_html","origin":"http://127.0.0.1:29999","max_pages":3,"max_depth":1,"allowed_methods":["GET"]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	env.router.ServeHTTP(recorder, httptest.NewRequest("POST", "/api/tasks/"+task.ID.String()+"/runs", bytes.NewBufferString(`{"executor":"orchestrator","provider":"mock","budget":{"max_model_calls":1,"max_tool_calls":1}}`)))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	var run model.Run
	if err = json.Unmarshal(recorder.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	final := waitForRunTerminal(t, env, run.ID, 4*time.Second)
	if final.Status != model.RunStatusStopped {
		t.Fatalf("status=%s reason=%v", final.Status, final.FinishReason)
	}
	calls, err := env.runService.ListModelCalls(ctx, run.ID)
	if err != nil || len(calls) > 1 {
		t.Fatalf("model cap overridden: %d %v", len(calls), err)
	}
	toolCalls, err := env.runService.ListAgentToolCalls(ctx, run.ID)
	if err != nil || len(toolCalls) > 1 {
		t.Fatalf("tool cap overridden: %d %v", len(toolCalls), err)
	}
	for _, call := range toolCalls {
		if call.ToolName == "get_endpoint" {
			t.Fatal("tiny budget admitted an HTTP request")
		}
	}
	assertNoActiveChildren(t, env, run.ID)
}

type semanticsProviderFunc func(context.Context, []provider.ChatMessage, []provider.ToolDefinition, provider.ModelOpts) (*provider.ModelResponse, error)

func (f semanticsProviderFunc) Call(ctx context.Context, m []provider.ChatMessage, d []provider.ToolDefinition, o provider.ModelOpts) (*provider.ModelResponse, error) {
	return f(ctx, m, d, o)
}

func TestWorkerErrorAtExplorationBoundary_RemainsFailed(t *testing.T) {
	for _, scenario := range []string{"protocol_error", "provider_error"} {
		t.Run(scenario, func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			ctx := context.Background()
			task := createTestTask(t, env, "worker error at budget boundary")
			run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			p := semanticsProviderFunc(func(_ context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
				for _, d := range defs {
					if d.Name != "submit_plan" {
						continue
					}
					if msgs[len(msgs)-1].Role == provider.RoleTool {
						return &provider.ModelResponse{FinishReason: "stop", Content: "Planned."}, nil
					}
					return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: `{"items":[{"key":"review","title":"Review evidence","objective":"Review","work_type":"aggregate_summary"}]}`}}}}, nil
				}
				if scenario == "protocol_error" {
					return nil, fmt.Errorf("invalid worker response: %w", provider.ErrProtocol)
				}
				return nil, fmt.Errorf("worker provider unavailable")
			})
			budget := model.DefaultBudgetConfig()
			// Two planner requests plus the failing worker request consume all
			// three exploration slots; one slot remains reserved for wrapup.
			budget.MaxModelCalls = 4
			orchestration.NewEngine(run.ID, env.runService, p, budget).Run(ctx)
			final, err := env.runService.Get(ctx, run.ID)
			if err != nil || final.Status != model.RunStatusFailed || final.FinishReason == nil || *final.FinishReason != scenario {
				t.Fatalf("real error became a budget stop: final=%+v err=%v", final, err)
			}
			calls, err := env.runService.ListModelCalls(ctx, run.ID)
			if err != nil || len(calls) != 3 {
				t.Fatalf("unexpected extra summary request after failure: calls=%d err=%v", len(calls), err)
			}
			assertNoActiveChildren(t, env, run.ID)
		})
	}
}

func TestBudgetSummary_DeadlineConfigurationAndCancellation(t *testing.T) {
	for _, scenario := range []string{"over_five_seconds", "summary_timeout", "run_deadline", "user_stop", "user_stop_before_summary", "refusal", "summary_requests_tool", "small_budget"} {
		t.Run(scenario, func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			task := createTestTask(t, env, scenario)
			run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			p := &boundaryProvider{entered: make(chan struct{}), release: make(chan struct{}), summaryEntered: make(chan struct{})}
			if scenario != "user_stop_before_summary" {
				close(p.release)
			}
			budget := model.DefaultBudgetConfig()
			budget.MaxModelCalls = 4
			budget.RunTimeoutSeconds = 15
			budget.CleanupTimeoutSeconds = 1
			budget.SummaryTimeoutSeconds = 8
			// The summary setting must also override the exploration request timeout.
			budget.ModelTimeoutSeconds = 2
			wantReason := "model_calls_exhausted"
			switch scenario {
			case "over_five_seconds":
				p.summaryDelay = 6 * time.Second
			case "summary_timeout":
				p.summaryDelay = 3 * time.Second
				budget.SummaryTimeoutSeconds = 1
			case "run_deadline":
				p.summaryDelay = 3 * time.Second
				budget.RunTimeoutSeconds = 1
				wantReason = "timeout"
			case "user_stop":
				p.summaryDelay = 5 * time.Second
				wantReason = "user_requested_stop"
			case "user_stop_before_summary":
				wantReason = "user_requested_stop"
			case "refusal":
				p.summaryReason = "refusal"
			case "summary_requests_tool":
				p.summaryReason = "tools"
			case "small_budget":
				budget.MaxModelCalls = 1
			}
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(model.CauseShutdown)
			done := make(chan struct{})
			go func() { orchestration.NewEngine(run.ID, env.runService, p, budget).Run(ctx); close(done) }()
			if scenario == "user_stop" || scenario == "user_stop_before_summary" {
				entered := p.summaryEntered
				if scenario == "user_stop_before_summary" {
					entered = p.entered
				}
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("summary did not start")
				}
				if _, err := env.runService.Stop(context.Background(), run.ID); err != nil {
					t.Fatal(err)
				}
				cancel(model.CauseUserStop)
			}
			select {
			case <-done:
			case <-time.After(12 * time.Second):
				t.Fatal("wrapup did not finish")
			}
			final, err := env.runService.Get(context.Background(), run.ID)
			if err != nil || final.Status != model.RunStatusStopped || final.FinishReason == nil || *final.FinishReason != wantReason {
				t.Fatalf("final=%+v err=%v", final, err)
			}
			if final.FinalOutput == nil || !strings.Contains(*final.FinalOutput, "Scope limits") {
				t.Fatalf("missing persisted inventory: %+v", final)
			}
			gotSummary := strings.Contains(*final.FinalOutput, "Partial execution summary.")
			if gotSummary != (scenario == "over_five_seconds") {
				t.Fatalf("model summary saved=%t in %s", gotSummary, scenario)
			}
			calls, err := env.runService.ListModelCalls(context.Background(), run.ID)
			if err != nil || len(calls) > budget.MaxModelCalls {
				t.Fatalf("model hard cap exceeded: %d %v", len(calls), err)
			}
			if scenario == "small_budget" && p.summaries.Load() != 0 {
				t.Fatal("small budget used an unreserved summary call")
			}
			if scenario == "user_stop_before_summary" && p.summaries.Load() != 0 {
				t.Fatal("summary requested after user stop")
			}
			tools, err := env.runService.ListAgentToolCalls(context.Background(), run.ID)
			if err != nil || len(tools) != 1 {
				t.Fatalf("summary executed a tool: count=%d err=%v", len(tools), err)
			}
			assertNoActiveChildren(t, env, run.ID)
		})
	}
}

// Persisted fixtures exercise HTTP provenance and coverage without sending a request.
func TestBudgetToolExhaustion_AllToolsSucceedAndEvidenceSurvives(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "saved HTTP evidence", Objective: "Inspect saved evidence", Scope: json.RawMessage(`{"mode":"discovery","seeds":["http://127.0.0.1:29999/","http://127.0.0.1:29999/login","http://127.0.0.1:29999/unvisited"],"discovery_policy":{"mode":"same_origin_html","origin":"http://127.0.0.1:29999","max_pages":10,"max_depth":2,"allowed_methods":["GET"]}}`)})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil); err != nil {
		t.Fatal(err)
	}
	assets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	for i, a := range assets {
		if strings.HasSuffix(a.ExternalKey, "/unvisited") {
			continue
		}
		if _, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: fmt.Sprintf("fixture_%d", i), Title: "HTTP fixture", WorkType: "inspect_http_endpoint", AssetIDs: []uuid.UUID{a.ID}, InputData: map[string]any{"asset_id": a.ID.String()}}}); err != nil {
			t.Fatal(err)
		}
		wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
		if err != nil || wi == nil {
			t.Fatal(err)
		}
		args, _ := json.Marshal(map[string]any{"asset_id": a.ID.String()})
		tc, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "fixture_read", "get_endpoint", args)
		if err != nil {
			t.Fatal(err)
		}
		if err = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", `{"response":{"status_code":200,"headers":{"Content-Type":"text/html"},"body":"<title>Home</title>Welcome"}}`, nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(a.ExternalKey, "/login") {
			// A fetched page with no written observation must still count as visited.
			if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, false, nil, "model_calls_exhausted"); err != nil {
				t.Fatal(err)
			}
			continue
		}
		tool := tools.NewRecordObservationBatchTool(run.ID, att.ID, sess.ID, env.runService)
		out, err := tool.Execute(ctx, []byte(fmt.Sprintf(`{"observations":[{"obs_type":"page_observation","content":"HTTP 200, Home, HTML welcome page","data":{"status_code":200,"title":"Home","response_features":{"content_type":"text/html"},"content_summary":"Welcome"},"source_tool_call_id":%q,"asset_id":%q}]}`, tc.ID.String(), a.ID.String())))
		if err != nil || !strings.Contains(out, `"count":1`) {
			t.Fatalf("batch output=%s err=%v", out, err)
		}
		if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, true, nil, "completed"); err != nil {
			t.Fatal(err)
		}
	}
	serial := 0
	p := semanticsProviderFunc(func(_ context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
		if len(defs) == 0 {
			return &provider.ModelResponse{FinishReason: "stop", Content: "Saved partial results."}, nil
		}
		serial++
		name, args := "read_observations", "{}"
		for _, d := range defs {
			if d.Name == "submit_plan" {
				if msgs[len(msgs)-1].Role == provider.RoleTool {
					return &provider.ModelResponse{FinishReason: "stop", Content: "Planned."}, nil
				}
				name, args = "submit_plan", `{"items":[{"key":"explore","title":"Explore","objective":"Read stored evidence","work_type":"aggregate_summary"},{"key":"never_started","title":"Never started","objective":"Read stored evidence","work_type":"aggregate_summary"}]}`
			}
		}
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: fmt.Sprintf("call_%d", serial), Type: "function", Function: provider.FunctionCall{Name: name, Arguments: args}}}}, nil
	})
	budget := model.DefaultBudgetConfig()
	budget.MaxToolCalls = 5
	orchestration.NewEngine(run.ID, env.runService, p, budget).Run(ctx)
	final, err := env.runService.Get(ctx, run.ID)
	if err != nil || final.Status != model.RunStatusStopped || final.FinishReason == nil || *final.FinishReason != "tool_calls_exhausted" {
		t.Fatalf("final=%+v err=%v", final, err)
	}
	obs, _ := env.runService.ListObservationsByRun(ctx, run.ID)
	if len(obs) != 1 || !strings.Contains(string(obs[0].Data), `"content_summary"`) || obs[0].SourceToolCallID == uuid.Nil {
		t.Fatalf("lost merged observation: %+v", obs)
	}
	calls, _ := env.runService.ListAgentToolCalls(ctx, run.ID)
	// Two saved fixture reads plus exactly three calls admitted by this engine.
	if len(calls) != 5 {
		t.Fatalf("calls=%d", len(calls))
	}
	for _, call := range calls {
		if call.Status != "success" {
			t.Fatalf("tool did not succeed: %+v", call)
		}
	}
	if final.FinalOutput == nil || !strings.Contains(*final.FinalOutput, "discovered=3, planned=2, visited=2, unvisited=1") || strings.Contains(*final.FinalOutput, "status: pending") || strings.Contains(*final.FinalOutput, "status: running") {
		t.Fatalf("incorrect coverage/snapshot: %v", final.FinalOutput)
	}
	assertNoActiveChildren(t, env, run.ID)
}

func TestWorkerLocalExhaustion_LeavesRunActiveForPlanner(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "local budget")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "local", Title: "Local", WorkType: "aggregate_summary"}, {Key: "dependent", Title: "Dependent", WorkType: "aggregate_summary", Dependencies: []string{"local"}}})
	if err != nil {
		t.Fatal(err)
	}
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	reg.Register(tools.NewReadObservationsTool(run.ID, env.runService))
	p := semanticsProviderFunc(func(context.Context, []provider.ChatMessage, []provider.ToolDefinition, provider.ModelOpts) (*provider.ModelResponse, error) {
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "read", Type: "function", Function: provider.FunctionCall{Name: "read_observations", Arguments: "{}"}}}}, nil
	})
	budget := model.DefaultBudgetConfig()
	budget.MaxModelCalls = 1
	result, err := agent.NewSessionRunner(sess.ID, run.ID, "worker", env.runService, p, reg, budget).Run(ctx, nil)
	if err != nil || result.FinishReason != "model_calls_exhausted" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, false, nil, result.FinishReason); err != nil {
		t.Fatal(err)
	}
	item, _ := env.runService.GetWorkItem(ctx, wi.ID)
	parent, _ := env.runService.Get(ctx, run.ID)
	if item.Status != model.WorkItemStatusExhausted || parent.Status != model.RunStatusRunning {
		t.Fatalf("item=%s run=%s", item.Status, parent.Status)
	}
	if next, _, _, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID); err != nil || next != nil {
		t.Fatalf("dependent should be blocked: %v %v", next, err)
	}
	if _, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "planner_followup", Title: "Bounded evidence review", WorkType: "aggregate_summary"}}); err != nil {
		t.Fatal(err)
	}
	if err = env.runService.FinalizeRunWrapup(ctx, run.ID, model.CauseToolBudgetExhausted, nil); err != nil {
		t.Fatal(err)
	}
	assertNoActiveChildren(t, env, run.ID)
}
