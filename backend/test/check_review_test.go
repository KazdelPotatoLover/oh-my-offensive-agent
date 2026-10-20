package test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

func TestCheckReview_SourceBindingAndDeterministicOutcomes(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()
	ctx := context.Background()
	makeRun := func(name string) *model.Run {
		t.Helper()
		task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: name, Objective: "Review checks", Scope: json.RawMessage(`{"endpoints":["http_home","http_login"]}`)})
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
		return run
	}
	run := makeRun("source binding")
	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil || len(assets) != 2 {
		t.Fatalf("assets=%d err=%v", len(assets), err)
	}
	target, other := assets[0].ID, assets[1].ID
	if _, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "check", Title: "Check", Objective: "Check", WorkType: "run_security_check", AssetIDs: []uuid.UUID{target}, InputData: map[string]any{"asset_id": target.String(), "check_id": "http_x_content_type_options"}}}); err != nil {
		t.Fatal(err)
	}
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wi == nil {
		t.Fatal(err)
	}
	makeEvidence := func(runID, asset, bodyAsset uuid.UUID) uuid.UUID {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"asset_id": asset.String()})
		tc, err := env.runService.CreateAgentToolCall(ctx, runID, nil, uuid.NewString(), "get_endpoint", args)
		if err != nil {
			t.Fatal(err)
		}
		output, _ := json.Marshal(map[string]any{"request": map[string]any{"url": "http://127.0.0.1/", "asset_id": bodyAsset.String(), "method": "GET"}, "response": map[string]any{"status_code": 200, "headers": map[string]string{"Content-Type": "text/html"}}})
		if err = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", string(output), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		return tc.ID
	}
	valid := makeEvidence(run.ID, target, target)
	foreign := makeEvidence(makeRun("foreign source").ID, target, target)
	wrongAsset := makeEvidence(run.ID, other, other)
	tampered := makeEvidence(run.ID, target, other)
	input := service.CheckResultInput{AssetID: target, CheckID: "http_x_content_type_options", Status: model.CheckStatusPass, SourceToolCallID: &valid,
		Explanation: "caller claims success", GenerateFinding: true, FindingSeverity: "critical"}
	for _, source := range []uuid.UUID{foreign, wrongAsset, tampered} {
		bad := input
		bad.SourceToolCallID = &source
		if _, err := env.runService.RecordCheckResults(ctx, run.ID, &att.ID, &sess.ID, &wi.ID, []service.CheckResultInput{bad}); err == nil {
			t.Fatalf("accepted bad source %s", source)
		}
	}
	fakeSession := uuid.New()
	if _, err := env.runService.RecordCheckResults(ctx, run.ID, &att.ID, &fakeSession, &wi.ID, []service.CheckResultInput{input}); err == nil {
		t.Fatal("accepted mismatched worker session")
	}
	bad := input
	bad.SourceToolCallID = &foreign
	if _, err := env.runService.RecordCheckResults(ctx, run.ID, &att.ID, &sess.ID, &wi.ID, []service.CheckResultInput{input, bad}); err == nil {
		t.Fatal("invalid batch accepted")
	}
	existing, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(existing) != 0 {
		t.Fatal("invalid batch committed partial results")
	}
	out, err := env.runService.RecordCheckResults(ctx, run.ID, &att.ID, &sess.ID, &wi.ID, []service.CheckResultInput{input})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.CheckResults) != 1 || out.CheckResults[0].Status != model.CheckStatusNeedsAttention || len(out.Findings) != 1 || out.Findings[0].Severity != "low" || out.Findings[0].Status != model.FindingStatusPendingReview {
		t.Fatalf("caller overrode deterministic outcome: %+v", out)
	}
	missing := input
	missing.SourceToolCallID = nil
	for i := 0; i < 2; i++ {
		out, err = env.runService.RecordCheckResults(ctx, run.ID, &att.ID, &sess.ID, &wi.ID, []service.CheckResultInput{missing})
		if err != nil {
			t.Fatal(err)
		}
		if len(out.CheckResults) != 1 || out.CheckResults[0].Status != model.CheckStatusInsufficientEvidence || len(out.Findings) != 0 {
			t.Fatal("missing evidence became finding")
		}
		if i == 1 && out.DeduplicatedCount != 1 {
			t.Fatal("missing-evidence result not deduplicated")
		}
	}
	// A check must not create records under a completed Attempt while the Run is active.
	if err = env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, true, json.RawMessage(`{}`), "completed"); err != nil {
		t.Fatal(err)
	}
	if _, err = env.runService.RecordCheckResults(ctx, run.ID, &att.ID, &sess.ID, &wi.ID, []service.CheckResultInput{input}); err == nil {
		t.Fatal("late write accepted for completed attempt")
	}
}

type checkReviewProvider struct {
	asset uuid.UUID
	mock  *provider.MockProvider
}

func (p *checkReviewProvider) Call(ctx context.Context, messages []provider.ChatMessage, defs []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	for _, def := range defs {
		if def.Name == "submit_plan" {
			if messages[len(messages)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{FinishReason: "stop", Content: "Plan applied."}, nil
			}
			name := "submit_plan"
			var args any = map[string]any{"items": []map[string]any{
				{"key": "inspect", "title": "Inspect seed", "objective": "Fetch page and persist evidence", "work_type": "inspect_http_endpoint", "asset_ids": []uuid.UUID{p.asset}, "input_data": map[string]any{"asset_id": p.asset}},
				{"key": "check", "title": "Check seed", "objective": "Review persisted security headers", "work_type": "run_security_check", "dependencies": []string{"inspect"}, "asset_ids": []uuid.UUID{p.asset}, "input_data": map[string]any{"asset_id": p.asset, "check_id": "http_security_headers"}},
				{"key": "check_repeat", "title": "Repeat check", "objective": "Verify idempotent check execution", "work_type": "run_security_check", "dependencies": []string{"check"}, "asset_ids": []uuid.UUID{p.asset}, "input_data": map[string]any{"asset_id": p.asset, "check_id": "http_security_headers"}},
			}}
			for _, other := range defs {
				if other.Name == "suggest_completion" {
					name = "suggest_completion"
					args = map[string]any{"final_summary": "Checks executed; configuration findings remain pending review."}
				}
			}
			encoded, _ := json.Marshal(args)
			return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: uuid.NewString(), Type: "function", Function: provider.FunctionCall{Name: name, Arguments: string(encoded)}}}}, nil
		}
	}
	return p.mock.Call(ctx, messages, defs, opts)
}

func TestCheckReview_EngineRunsChecksWithoutRefetch(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()
	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Engine check integration", Objective: "Review seed security headers", Scope: json.RawMessage(`{"endpoints":["http_home"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil || len(assets) != 1 {
		t.Fatal(err)
	}
	p := &checkReviewProvider{asset: assets[0].ID, mock: provider.NewMockProvider(provider.ScenarioHTTPTraceability)}
	budget := model.DefaultBudgetConfig()
	bounded, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	orchestration.NewEngine(run.ID, env.runService, p, budget).Run(bounded)
	final, err := env.runService.Get(ctx, run.ID)
	if err != nil || final.Status != model.RunStatusCompleted {
		t.Fatalf("final=%+v error=%v", final, err)
	}
	checks, err := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if err != nil || len(checks) != 3 {
		t.Fatalf("checks=%d error=%v", len(checks), err)
	}
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	if err != nil || len(findings) != 2 {
		t.Fatalf("findings=%d error=%v", len(findings), err)
	}
	for _, f := range findings {
		if f.Status != model.FindingStatusPendingReview {
			t.Fatal("unreviewed finding confirmed")
		}
	}
	var fetches int
	if err = env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM agent_tool_calls WHERE run_id=$1 AND tool_name='get_endpoint'`, run.ID).Scan(&fetches); err != nil {
		t.Fatal(err)
	}
	if fetches != 1 {
		t.Fatalf("check refetched the page: %d HTTP tools", fetches)
	}
	assertNoActiveChildren(t, env, run.ID)
}
