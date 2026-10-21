package test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

func contractDiscoveryRun(t *testing.T, env *TestEnv, origin string) (*model.Run, uuid.UUID) {
	t.Helper()
	scope, _ := json.Marshal(map[string]interface{}{"mode": "discovery", "seeds": []string{origin + "/"}, "discovery_policy": map[string]interface{}{"mode": "same_origin_html", "origin": origin, "max_pages": 3, "max_depth": 1, "allowed_methods": []string{"GET"}, "request_interval_ms": 1}})
	task, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{Name: "Planner contract", Objective: "Inspect only the seed; extract links and report actual evidence.", Scope: scope})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := env.runService.ListAssetsByRun(context.Background(), run.ID)
	if err != nil || len(assets) != 1 {
		t.Fatalf("seed assets: %v", err)
	}
	return run, assets[0].ID
}

func TestPlannerContract_RejectsNonExecutablePlansAtomically(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	fixture, cleanup := setupTestFixture(t)
	defer cleanup()
	run, asset := contractDiscoveryRun(t, env, "http://127.0.0.1:"+fixture.Port())
	ctx := context.Background()
	if _, err := env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil); err != nil {
		t.Fatal(err)
	}
	valid := service.PlanItemSpec{Key: "seed", Title: "Seed", Objective: "Read seed", WorkType: "inspect_http_endpoint", AssetIDs: []uuid.UUID{asset}, InputData: map[string]interface{}{"asset_id": asset.String()}}
	for _, kind := range []string{"crawl_page", "frontier_reconcile", "analyze_sample", "inspect_http_endpoint"} {
		invalid := valid
		invalid.Key = "invalid"
		invalid.WorkType = kind
		if kind == "inspect_http_endpoint" {
			invalid.InputData = map[string]interface{}{"asset_id": uuid.NewString()}
		}
		if _, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{valid, invalid}); err == nil {
			t.Fatalf("accepted non-executable %s", kind)
		}
		items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
		if err != nil || len(items) != 0 {
			t.Fatalf("partial plan persisted: items=%d error=%v", len(items), err)
		}
	}
	if _, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{valid}); err != nil {
		t.Fatal(err)
	}
}

type repairingPlannerProvider struct {
	evidenceSource            string
	t                         *testing.T
	asset                     uuid.UUID
	plannerCalls, workerCalls int
	noEvidence                bool
}

func (p *repairingPlannerProvider) Call(ctx context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	has := func(name string) bool {
		for _, d := range defs {
			if d.Name == name {
				return true
			}
		}
		return false
	}
	call := func(id, name, args string) *provider.ModelResponse {
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: id, Type: "function", Function: provider.FunctionCall{Name: name, Arguments: args}}}}
	}
	if has("suggest_completion") {
		if len(msgs) > 0 && msgs[len(msgs)-1].Role == provider.RoleTool {
			return &provider.ModelResponse{FinishReason: "stop", Content: "Evidence-backed run completed"}, nil
		}
		return call("finish", "suggest_completion", `{"final_summary":"Seed verified with persisted HTTP evidence; discovery limited to the seed."}`), nil
	}
	if has("submit_plan") {
		p.plannerCalls++
		if p.plannerCalls == 1 {
			// Reproduce the real provider's invented type, without relying on the prompt.
			return call("bad_plan", "submit_plan", `{"items":[{"key":"seed","title":"Seed","objective":"Fetch seed","work_type":"crawl_page"}]}`), nil
		}
		if p.plannerCalls == 2 {
			last := msgs[len(msgs)-1]
			if last.Role != provider.RoleTool || !strings.Contains(last.Content, "unsupported work_type") {
				p.t.Error("Planner did not receive actionable rejection")
			}
			return call("corrected_plan", "submit_plan", fmt.Sprintf(`{"items":[{"key":"seed","title":"Seed","objective":"Fetch seed and record evidence","work_type":"inspect_http_endpoint","asset_ids":[%q],"input_data":{"asset_id":%q}}]}`, p.asset.String(), p.asset.String())), nil
		}
		return &provider.ModelResponse{FinishReason: "stop", Content: "Executable plan submitted"}, nil
	}
	if !has("get_endpoint") || has("read_sample") {
		p.t.Error("HTTP Worker was routed to the wrong tool registry")
		return nil, fmt.Errorf("wrong worker capabilities")
	}
	if p.noEvidence {
		return &provider.ModelResponse{FinishReason: "stop", Content: "I could not inspect this target."}, nil
	}
	p.workerCalls++
	switch p.workerCalls {
	case 1:
		return call("fetch_seed", "get_endpoint", fmt.Sprintf(`{"asset_id":%q}`, p.asset.String())), nil
	case 2:
		if strings.Contains(msgs[len(msgs)-1].Content, "Error") {
			p.t.Error("HTTP fetch failed")
		}
		var result struct {
			Execution struct {
				Source string `json:"source_tool_call_id"`
			} `json:"_execution"`
		}
		if err := json.Unmarshal([]byte(msgs[len(msgs)-1].Content), &result); err != nil {
			return nil, fmt.Errorf("invalid model-visible evidence JSON: %w", err)
		}
		p.evidenceSource = result.Execution.Source
		if p.evidenceSource == "" || p.evidenceSource == p.asset.String() {
			p.t.Error("model-visible evidence reference missing or confused with asset identity")
		}
		return call("extract_seed", "extract_endpoint_links", fmt.Sprintf(`{"source_tool_call_id":%q}`, p.evidenceSource)), nil
	case 3:
		return call("observe_seed", "record_observation", fmt.Sprintf(`{"obs_type":"http_status","content":"Seed returned HTTP 200","asset_id":%q,"source_tool_call_id":%q}`, p.asset.String(), p.evidenceSource)), nil
	default:
		return &provider.ModelResponse{FinishReason: "stop", Content: "HTTP seed verified; links extracted and observation recorded"}, nil
	}
}

func TestPlannerContract_RepairsPlanAndUsesHTTPEvidence(t *testing.T) {
	for _, noEvidence := range []bool{false, true} {
		t.Run(fmt.Sprintf("no_evidence_%v", noEvidence), func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			fixture, cleanup := setupTestFixture(t)
			defer cleanup()
			run, asset := contractDiscoveryRun(t, env, "http://127.0.0.1:"+fixture.Port())
			p := &repairingPlannerProvider{t: t, asset: asset, noEvidence: noEvidence}
			engine := orchestration.NewEngine(run.ID, env.runService, p, model.DefaultBudgetConfig())
			if err := env.manager.StartExecutor(run.ID, engine); err != nil {
				t.Fatal(err)
			}
			final := waitForRunTerminal(t, env, run.ID, 5*time.Second)
			if noEvidence {
				if final.Status != model.RunStatusFailed && final.Status != model.RunStatusStopped {
					t.Fatal("unverified worker text incorrectly completed run")
				}
				items, _ := env.runService.ListWorkItemsByRun(context.Background(), run.ID)
				if len(items) != 1 || items[0].Status != model.WorkItemStatusFailed {
					t.Fatalf("unbacked HTTP work was not failed: %+v", items)
				}
			} else {
				if final.Status != model.RunStatusCompleted {
					t.Fatalf("repair failed: %s %v", final.Status, final.FinishReason)
				}
				obs, err := env.runService.ListObservationsByRun(context.Background(), run.ID)
				if err != nil {
					t.Fatal(err)
				}
				hasHTTPStatus := false
				for _, o := range obs {
					if o.ObsType == "http_status" && o.AssetID != nil && *o.AssetID == asset {
						hasHTTPStatus = true
					}
				}
				if !hasHTTPStatus {
					t.Fatalf("missing persisted HTTP status evidence; observations=%d", len(obs))
				}
			}
			calls, err := env.runService.ListAgentToolCalls(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			rejected := false
			for _, c := range calls {
				if c.CallID == "bad_plan" && c.Status == "rejected" {
					rejected = true
				}
				if c.ToolName == "read_sample" {
					t.Fatal("live HTTP task invoked sample reader")
				}
			}
			if !rejected {
				t.Fatal("invalid plan was not explicitly rejected")
			}
		})
	}
}
