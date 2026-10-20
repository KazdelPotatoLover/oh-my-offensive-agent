package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/checker"
	"agent-workbench/backend/internal/fixture"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// helper to setup fixture server and environment
func setupPhase8Env(t *testing.T) (*TestEnv, *fixture.Server, func()) {
	t.Helper()
	env := setupTestEnv(t)
	srv, err := fixture.NewServer("0")
	if err != nil {
		t.Fatalf("failed to create fixture server: %v", err)
	}
	srv.Start()

	oldPort := os.Getenv("FIXTURE_PORT")
	_ = os.Setenv("FIXTURE_PORT", srv.Port())

	cleanup := func() {
		_ = srv.Close()
		if oldPort != "" {
			_ = os.Setenv("FIXTURE_PORT", oldPort)
		} else {
			_ = os.Unsetenv("FIXTURE_PORT")
		}
		env.teardown()
	}
	return env, srv, cleanup
}

// 1. 一项检查通过 (Check passes)
func TestPhase8_RulePass(t *testing.T) {
	env, srv, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	// Configure /login to return X-Content-Type-Options: nosniff
	srv.SetCustomHeader("/login", "X-Content-Type-Options", "nosniff")

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Pass Check Task",
		Objective: "Verify X-Content-Type-Options passes when header is present",
		Scope:     json.RawMessage(`{"endpoints": ["http_login"]}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil || len(runAssets) == 0 {
		t.Fatalf("list run assets: %v", err)
	}
	loginAsset := runAssets[0]

	// 1a. Plan and execute inspect_login to fetch endpoint and record HTTP tool call evidence
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_login",
			Title:     "Inspect Login",
			Objective: "Fetch login endpoint",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{loginAsset.ID},
			InputData: map[string]interface{}{"asset_id": loginAsset.ID.String()},
		},
	})
	if err != nil {
		t.Fatalf("submit inspect plan: %v", err)
	}

	wiInspect, attInspect, sessInspect, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim inspect item: %v", err)
	}

	httpTool := tools.NewGetEndpointTool(run.ID, wiInspect.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": loginAsset.ID.String()})

	tcInspect, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_get_login", "get_endpoint", getArgs)
	if err != nil {
		t.Fatalf("create tool call: %v", err)
	}
	getOut, err := httpTool.Execute(ctx, getArgs)
	if err != nil {
		t.Fatalf("execute get_endpoint: %v", err)
	}
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"fetched"}`), "completed")

	// 1b. Plan and execute run_security_check on loginAsset
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_login",
			Title:        "Check Login Headers",
			Objective:    "Check headers on login",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_login"},
			AssetIDs:     []uuid.UUID{loginAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": loginAsset.ID.String(),
				"check_id": "http_x_content_type_options",
			},
		},
	})
	if err != nil {
		t.Fatalf("submit check plan: %v", err)
	}

	wiCheck, attCheck, sessCheck, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim check item: %v", err)
	}

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": loginAsset.ID.String(),
		"check_id": "http_x_content_type_options",
	})
	checkOut, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("execute run_security_check: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheck.ID, attCheck.ID, sessCheck.ID, true, json.RawMessage(checkOut), "completed")

	// Verify check result in database
	dbResults, err := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list check results: %v", err)
	}
	if len(dbResults) != 1 {
		t.Fatalf("expected 1 check result, got %d", len(dbResults))
	}
	if dbResults[0].Status != model.CheckStatusPass {
		t.Fatalf("expected status 'pass', got %q (%s)", dbResults[0].Status, dbResults[0].Explanation)
	}
	if !strings.Contains(dbResults[0].Explanation, "nosniff") {
		t.Fatalf("expected explanation to mention nosniff, got: %s", dbResults[0].Explanation)
	}

	// Verify no findings generated for passed check
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list findings: %v", err)
	}
	if len(findings) != 0 {
		t.Fatalf("expected 0 findings for passed check, got %d", len(findings))
	}
}

// 2. 一项产生需要关注的结果 (Check produces needs_attention and candidate finding)
func TestPhase8_RuleNeedsAttention_GeneratesCandidateFinding(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	// Target root home / (missing X-Content-Type-Options by default)
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Needs Attention Task",
		Objective: "Verify needs_attention when security header is absent",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// Step 1: inspect home endpoint
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_home",
			Title:     "Inspect Home",
			Objective: "Fetch home",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	wiInspect, attInspect, sessInspect, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	httpTool := tools.NewGetEndpointTool(run.ID, wiInspect.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcInspect, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_get_home", "get_endpoint", getArgs)
	getOut, _ := httpTool.Execute(ctx, getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"home"}`), "completed")

	// Step 2: run check
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_home",
			Title:        "Check Home",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_home"},
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "http_x_content_type_options",
			},
		},
	})
	wiCheck, attCheck, sessCheck, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_x_content_type_options",
	})
	checkOut, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("execute check: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheck.ID, attCheck.ID, sessCheck.ID, true, json.RawMessage(checkOut), "completed")

	var res struct {
		FindingsCount int `json:"findings_count"`
	}
	_ = json.Unmarshal([]byte(checkOut), &res)
	if res.FindingsCount != 1 {
		t.Fatalf("expected 1 finding count in response, got %d", res.FindingsCount)
	}

	// Verify CheckResult status is needs_attention
	dbResults, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(dbResults) != 1 || dbResults[0].Status != model.CheckStatusNeedsAttention {
		t.Fatalf("expected needs_attention check result, got: %+v", dbResults)
	}

	// Verify Candidate Finding created
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	if err != nil || len(findings) != 1 {
		t.Fatalf("expected 1 candidate finding, got %d (err: %v)", len(findings), err)
	}
	f := findings[0]
	if f.Status != model.FindingStatusPendingReview {
		t.Fatalf("expected status 'pending_review', got %q", f.Status)
	}
	if f.Category != "configuration_risk" {
		t.Fatalf("expected category 'configuration_risk', got %q", f.Category)
	}
	if f.Severity != "low" {
		t.Fatalf("expected severity 'low', got %q", f.Severity)
	}
	if f.CheckResultID != dbResults[0].ID {
		t.Fatalf("finding check_result_id %s mismatch with %s", f.CheckResultID, dbResults[0].ID)
	}
	if f.SourceToolCallID == nil || *f.SourceToolCallID != tcInspect.ID {
		t.Fatalf("finding source_tool_call_id %v mismatch with %s", f.SourceToolCallID, tcInspect.ID)
	}
	if !strings.Contains(f.Title, "X-Content-Type-Options") {
		t.Fatalf("unexpected finding title: %s", f.Title)
	}
}

// 3. 一项不适用 (Check is not_applicable)
func TestPhase8_RuleNotApplicable(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	// /health returns Content-Type: application/json -> CSP check is not applicable!
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Not Applicable Task",
		Objective: "Verify not_applicable on JSON endpoint for HTML CSP check",
		Scope:     json.RawMessage(`{"endpoints": ["http_health"]}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	healthAsset := runAssets[0]

	// Inspect health endpoint
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_health",
			Title:     "Inspect Health",
			Objective: "Fetch health",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{healthAsset.ID},
			InputData: map[string]interface{}{"asset_id": healthAsset.ID.String()},
		},
	})
	wiInspect, attInspect, sessInspect, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	httpTool := tools.NewGetEndpointTool(run.ID, wiInspect.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": healthAsset.ID.String()})
	tcInspect, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_get_health", "get_endpoint", getArgs)
	getOut, _ := httpTool.Execute(ctx, getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"health"}`), "completed")

	// Run CSP check on JSON endpoint
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_health_csp",
			Title:        "Check CSP on Health",
			Objective:    "Check CSP",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_health"},
			AssetIDs:     []uuid.UUID{healthAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": healthAsset.ID.String(),
				"check_id": "http_content_security_policy",
			},
		},
	})
	wiCheck, attCheck, sessCheck, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": healthAsset.ID.String(),
		"check_id": "http_content_security_policy",
	})
	checkOut, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("execute check: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheck.ID, attCheck.ID, sessCheck.ID, true, json.RawMessage(checkOut), "completed")

	dbResults, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(dbResults) != 1 {
		t.Fatalf("expected 1 result, got %d", len(dbResults))
	}
	if dbResults[0].Status != model.CheckStatusNotApplicable {
		t.Fatalf("expected status 'not_applicable', got %q (%s)", dbResults[0].Status, dbResults[0].Explanation)
	}
	if !strings.Contains(dbResults[0].Explanation, "application/json") {
		t.Fatalf("expected explanation to mention non-HTML format, got: %s", dbResults[0].Explanation)
	}

	if _, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "check_health_hsts", Title: "Check HSTS", Objective: "Check HSTS", WorkType: "run_security_check", AssetIDs: []uuid.UUID{healthAsset.ID}, InputData: map[string]interface{}{"asset_id": healthAsset.ID.String(), "check_id": "http_strict_transport_security"}}}); err != nil {
		t.Fatal(err)
	}
	wiHSTS, attHSTS, sessHSTS, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wiHSTS == nil {
		t.Fatal(err)
	}
	checkTool = tools.NewRunSecurityCheckTool(run.ID, wiHSTS.ID, attHSTS.ID, sessHSTS.ID, env.runService)

	// Also verify HSTS on plain HTTP endpoint returns not_applicable
	checkArgsHSTS, _ := json.Marshal(map[string]string{
		"asset_id": healthAsset.ID.String(),
		"check_id": "http_strict_transport_security",
	})
	_, err = checkTool.Execute(ctx, checkArgsHSTS)
	if err != nil {
		t.Fatalf("execute HSTS check: %v", err)
	}

	dbResults2, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(dbResults2) != 2 {
		t.Fatalf("expected 2 results, got %d", len(dbResults2))
	}
	if dbResults2[1].Status != model.CheckStatusNotApplicable {
		t.Fatalf("expected HSTS status 'not_applicable' for HTTP, got %q", dbResults2[1].Status)
	}
}

// 4. 响应截断或证据缺失，结果为证据不足 (Truncated response or missing evidence yields insufficient_evidence)
func TestPhase8_InsufficientEvidence_TruncatedAndMissing(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Evidence Test",
		Objective: "Test truncation and missing evidence",
		Scope:     json.RawMessage(`{"endpoints": ["http_home", "http_login"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	var homeAsset, loginAsset model.Asset
	for _, a := range runAssets {
		if strings.Contains(a.ExternalKey, "/login") {
			loginAsset = a
		} else {
			homeAsset = a
		}
	}

	// 4a. Missing evidence: loginAsset was never inspected with get_endpoint
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "check_missing",
			Title:     "Check Missing",
			Objective: "Check",
			WorkType:  "run_security_check",
			AssetIDs:  []uuid.UUID{loginAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": loginAsset.ID.String(),
				"check_id": "http_security_headers",
			},
		},
	})
	wiCheckMissing, attCheckMissing, sessCheckMissing, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheckMissing.ID, attCheckMissing.ID, sessCheckMissing.ID, env.runService)
	checkArgsMissing, _ := json.Marshal(map[string]string{
		"asset_id": loginAsset.ID.String(),
		"check_id": "http_security_headers",
	})
	outMissing, err := checkTool.Execute(ctx, checkArgsMissing)
	if err != nil {
		t.Fatalf("execute check with missing evidence: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheckMissing.ID, attCheckMissing.ID, sessCheckMissing.ID, true, json.RawMessage(outMissing), "completed")

	crMissing, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(crMissing) != 1 || crMissing[0].Status != model.CheckStatusInsufficientEvidence {
		t.Fatalf("expected insufficient_evidence for missing evidence, got: %+v", crMissing)
	}

	// 4b. Truncated evidence: simulated truncated get_endpoint output
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_trunc",
			Title:     "Inspect Trunc",
			Objective: "Inspect",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	wiTrunc, attTrunc, sessTrunc, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	truncPayload := map[string]interface{}{
		"integrity": map[string]interface{}{
			"saved_bytes":  65536,
			"is_truncated": true,
			"note":         "saved partial response: response body exceeded limit",
		},
		"request": map[string]interface{}{
			"method":   "GET",
			"url":      "http://127.0.0.1:29090/",
			"asset_id": homeAsset.ID.String(),
		},
		"response": map[string]interface{}{
			"status_code": 200,
			"headers":     map[string]string{"Content-Type": "text/html"},
			"body":        "partial...",
		},
	}
	truncJSON, _ := json.Marshal(truncPayload)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcTrunc, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessTrunc.ID, nil, "call_get_trunc", "get_endpoint", getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcTrunc.ID, "success", string(truncJSON), nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiTrunc.ID, attTrunc.ID, sessTrunc.ID, true, json.RawMessage(`{"summary":"trunc"}`), "completed")

	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_trunc",
			Title:        "Check Trunc",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_trunc"},
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "http_security_headers",
			},
		},
	})
	wiCheckTrunc, attCheckTrunc, sessCheckTrunc, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkToolTrunc := tools.NewRunSecurityCheckTool(run.ID, wiCheckTrunc.ID, attCheckTrunc.ID, sessCheckTrunc.ID, env.runService)
	checkArgsTrunc, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_security_headers",
	})
	outTrunc, err := checkToolTrunc.Execute(ctx, checkArgsTrunc)
	if err != nil {
		t.Fatalf("execute check with truncated evidence: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheckTrunc.ID, attCheckTrunc.ID, sessCheckTrunc.ID, true, json.RawMessage(outTrunc), "completed")

	crAll, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(crAll) != 2 {
		t.Fatalf("expected 2 results, got %d", len(crAll))
	}
	if crAll[1].Status != model.CheckStatusInsufficientEvidence {
		t.Fatalf("expected status 'insufficient_evidence' for truncated evidence, got %q (%s)", crAll[1].Status, crAll[1].Explanation)
	}
	if !strings.Contains(crAll[1].Explanation, "truncated") {
		t.Fatalf("expected explanation to mention truncated, got: %s", crAll[1].Explanation)
	}
}

// 5. 检查错误不能变成通过 (Check execution error must NOT become pass)
func TestPhase8_ErrorCannotBecomePass(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Error Check Task",
		Objective: "Verify execution errors never become pass",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// Save corrupt JSON as tool output
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_corrupt",
			Title:     "Inspect Corrupt",
			Objective: "Inspect",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	wiInspect, attInspect, sessInspect, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcInspect, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_corrupt", "get_endpoint", getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", "{corrupted_payload_invalid_json: true", nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"corrupt"}`), "completed")

	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_corrupt",
			Title:        "Check Corrupt",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_corrupt"},
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "http_x_content_type_options",
			},
		},
	})
	wiCheck, attCheck, sessCheck, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_x_content_type_options",
	})
	outCorrupt, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("execute check: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheck.ID, attCheck.ID, sessCheck.ID, true, json.RawMessage(outCorrupt), "completed")

	cr, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(cr) != 1 {
		t.Fatalf("expected 1 result, got %d", len(cr))
	}
	if cr[0].Status != model.CheckStatusError {
		t.Fatalf("expected status 'error', got %q (%s)", cr[0].Status, cr[0].Explanation)
	}
	if cr[0].Status == model.CheckStatusPass {
		t.Fatal("CRITICAL: execution error was falsely represented as pass!")
	}
}

// 6. 重复检查的去重策略明确 (Deduplication policy verified)
func TestPhase8_DeduplicationStrategy(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Dedup Task",
		Objective: "Verify check result deduplication",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// Inspect home
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_home",
			Title:     "Inspect",
			Objective: "Inspect",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	wiInspect, attInspect, sessInspect, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	httpTool := tools.NewGetEndpointTool(run.ID, wiInspect.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcInspect, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_get_home", "get_endpoint", getArgs)
	getOut, _ := httpTool.Execute(ctx, getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"home"}`), "completed")

	// Plan check item
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_home_1",
			Title:        "Check 1",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_home"},
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "http_x_content_type_options",
			},
		},
	})
	wiCheck, attCheck, sessCheck, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_x_content_type_options",
	})

	// Run 1st time
	out1, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("first execute: %v", err)
	}
	var res1 struct {
		RecordedCount     int `json:"recorded_count"`
		DeduplicatedCount int `json:"deduplicated_count"`
	}
	_ = json.Unmarshal([]byte(out1), &res1)
	if res1.RecordedCount != 1 || res1.DeduplicatedCount != 0 {
		t.Fatalf("expected recorded=1, dedup=0, got recorded=%d, dedup=%d", res1.RecordedCount, res1.DeduplicatedCount)
	}

	// Run 2nd time with identical evidence
	out2, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("second execute: %v", err)
	}
	var res2 struct {
		RecordedCount     int `json:"recorded_count"`
		DeduplicatedCount int `json:"deduplicated_count"`
	}
	_ = json.Unmarshal([]byte(out2), &res2)
	if res2.RecordedCount != 0 || res2.DeduplicatedCount != 1 {
		t.Fatalf("expected recorded=0, dedup=1 on second run, got recorded=%d, dedup=%d", res2.RecordedCount, res2.DeduplicatedCount)
	}

	// Verify only 1 row in database
	dbResults, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(dbResults) != 1 {
		t.Fatalf("expected exactly 1 check result row in database, got %d", len(dbResults))
	}
	findings, _ := env.runService.ListFindingsByRun(ctx, run.ID)
	if len(findings) != 1 {
		t.Fatalf("expected exactly 1 finding row in database, got %d", len(findings))
	}
}

// 7. 候选发现能追溯到检查和原始HTTP证据 (Candidate finding traces back to CheckResult and original HTTP evidence)
func TestPhase8_FindingTraceability(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Traceability Task",
		Objective: "Verify complete audit trail from finding to http evidence",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// 1. Fetch endpoint
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_home",
			Title:     "Inspect",
			Objective: "Inspect",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	wiInspect, attInspect, sessInspect, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	httpTool := tools.NewGetEndpointTool(run.ID, wiInspect.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcInspect, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_trace_home", "get_endpoint", getArgs)
	getOut, _ := httpTool.Execute(ctx, getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"home"}`), "completed")

	// 2. Check suite (evaluates all rules: x-content-type-options, csp, hsts)
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_home_suite",
			Title:        "Check Suite",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_home"},
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "http_security_headers",
			},
		},
	})
	wiCheck, attCheck, sessCheck, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_security_headers",
	})
	outCheck, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("execute check suite: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheck.ID, attCheck.ID, sessCheck.ID, true, json.RawMessage(outCheck), "completed")

	// 3. Trace back from Finding
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	if err != nil || len(findings) == 0 {
		t.Fatalf("expected candidate findings, got %d (err: %v)", len(findings), err)
	}

	for _, f := range findings {
		// A. Finding -> CheckResult
		if f.CheckResultID == uuid.Nil {
			t.Fatalf("finding %s has nil check_result_id", f.ID)
		}

		// Query check result
		var cr model.CheckResult
		err := env.pool.QueryRow(ctx, `
			SELECT id, run_id, asset_id, check_id, status, source_tool_call_id
			FROM check_results WHERE id = $1`, f.CheckResultID).Scan(
			&cr.ID, &cr.RunID, &cr.AssetID, &cr.CheckID, &cr.Status, &cr.SourceToolCallID)
		if err != nil {
			t.Fatalf("failed to query check_result for finding %s: %v", f.ID, err)
		}
		if cr.Status != model.CheckStatusNeedsAttention {
			t.Fatalf("finding formed from non-needs_attention check result: status=%s", cr.Status)
		}
		if cr.AssetID != homeAsset.ID {
			t.Fatalf("asset mismatch: finding=%s, cr=%s, asset=%s", f.AssetID, cr.AssetID, homeAsset.ID)
		}

		// B. CheckResult -> AgentToolCall (raw HTTP evidence)
		if cr.SourceToolCallID == nil {
			t.Fatalf("check result %s has nil source_tool_call_id", cr.ID)
		}
		var tc model.AgentToolCall
		err = env.pool.QueryRow(ctx, `
			SELECT id, tool_name, arguments, output, status
			FROM agent_tool_calls WHERE id = $1`, *cr.SourceToolCallID).Scan(
			&tc.ID, &tc.ToolName, &tc.Arguments, &tc.Output, &tc.Status)
		if err != nil {
			t.Fatalf("failed to query source tool call %s: %v", *cr.SourceToolCallID, err)
		}
		if tc.ToolName != "get_endpoint" {
			t.Fatalf("expected tool_name 'get_endpoint', got %q", tc.ToolName)
		}
		if tc.Status != "success" {
			t.Fatalf("expected tool status 'success', got %q", tc.Status)
		}

		// C. Verify HTTP evidence contents
		var parsedOutput struct {
			Request struct {
				URL     string `json:"url"`
				AssetID string `json:"asset_id"`
			} `json:"request"`
			Response struct {
				StatusCode int               `json:"status_code"`
				Headers    map[string]string `json:"headers"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(tc.Output), &parsedOutput); err != nil {
			t.Fatalf("failed to unmarshal HTTP evidence: %v", err)
		}
		if parsedOutput.Request.AssetID != homeAsset.ID.String() {
			t.Fatalf("HTTP evidence asset mismatch: expected %s, got %s", homeAsset.ID, parsedOutput.Request.AssetID)
		}
		if parsedOutput.Response.StatusCode != 200 {
			t.Fatalf("expected status code 200 in HTTP evidence, got %d", parsedOutput.Response.StatusCode)
		}
	}
}

// 8. 停止后不能继续写入新的业务结果 (Cannot write new results after stop)
func TestPhase8_TerminalStateProtection_NoWritesAfterStop(t *testing.T) {
	env, _, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Stop Protection Task",
		Objective: "Verify no check results or findings written after run stopped",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// Stop the run
	_, err := env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusStopping, nil)
	if err != nil {
		t.Fatalf("stop run: %v", err)
	}
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusStopping, model.RunStatusStopped, nil)
	if err != nil {
		t.Fatalf("stopped run: %v", err)
	}

	// Attempt to record check results on stopped run
	inputs := []service.CheckResultInput{
		{
			AssetID:      homeAsset.ID,
			CheckID:      "http_x_content_type_options",
			CheckName:    "X-Content-Type-Options",
			CheckVersion: "1.0.0",
			Status:       model.CheckStatusPass,
			Explanation:  "Trying to write after stop",
		},
	}

	_, err = env.runService.RecordCheckResults(ctx, run.ID, nil, nil, nil, inputs)
	if err == nil {
		t.Fatal("expected RecordCheckResults to fail on stopped run, but succeeded")
	}
	if !strings.Contains(err.Error(), "cannot record check results") {
		t.Fatalf("unexpected error message: %v", err)
	}

	// Verify database remains empty of check results
	results, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(results) != 0 {
		t.Fatalf("expected 0 check results in DB after rejected write, got %d", len(results))
	}
}

// 9. Planner & Worker 闭环与报告展示 (Closed-loop execution with report & catalog)
func TestPhase8_ClosedLoop_OrchestrationWithSummary(t *testing.T) {
	env, srv, cleanup := setupPhase8Env(t)
	defer cleanup()

	ctx := context.Background()

	// Configure /login with X-Content-Type-Options: nosniff
	srv.SetCustomHeader("/login", "X-Content-Type-Options", "nosniff")

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 8 Closed Loop Orchestration",
		Objective: "Perform deterministic security checks on HTTP endpoints",
		Scope:     json.RawMessage(`{"endpoints": ["http_home", "http_login", "http_health"]}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Budget: &model.BudgetConfig{
			MaxModelCalls:      20,
			MaxToolCalls:       20,
			RunTimeoutSeconds:  30,
			MaxParallelWorkers: 1,
		},
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	var homeAsset, loginAsset, healthAsset model.Asset
	for _, a := range runAssets {
		if strings.Contains(a.ExternalKey, "/login") {
			loginAsset = a
		} else if strings.Contains(a.ExternalKey, "/health") {
			healthAsset = a
		} else {
			homeAsset = a
		}
	}

	// Step 1: Inspect endpoints with get_endpoint
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_home",
			Title:     "Inspect Home",
			Objective: "Fetch home",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
		{
			Key:       "inspect_login",
			Title:     "Inspect Login",
			Objective: "Fetch login",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{loginAsset.ID},
			InputData: map[string]interface{}{"asset_id": loginAsset.ID.String()},
		},
	})

	httpTool := tools.NewGetEndpointTool(run.ID, uuid.Nil, env.runService)

	// Execute home inspection
	wiHome, attHome, sessHome, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	getArgsHome, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcHome, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessHome.ID, nil, "call_home", "get_endpoint", getArgsHome)
	outHome, _ := httpTool.Execute(ctx, getArgsHome)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcHome.ID, "success", outHome, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiHome.ID, attHome.ID, sessHome.ID, true, json.RawMessage(`{"summary":"home fetched"}`), "completed")

	// Execute login inspection
	wiLogin, attLogin, sessLogin, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	getArgsLogin, _ := json.Marshal(map[string]string{"asset_id": loginAsset.ID.String()})
	tcLogin, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessLogin.ID, nil, "call_login", "get_endpoint", getArgsLogin)
	outLogin, _ := httpTool.Execute(ctx, getArgsLogin)
	_ = env.runService.UpdateAgentToolCall(ctx, tcLogin.ID, "success", outLogin, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiLogin.ID, attLogin.ID, sessLogin.ID, true, json.RawMessage(`{"summary":"login fetched"}`), "completed")

	// Step 2: Run security checks on home and login
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_home",
			Title:        "Check Home",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_home"},
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "http_security_headers",
			},
		},
		{
			Key:          "check_login",
			Title:        "Check Login",
			Objective:    "Check",
			WorkType:     "run_security_check",
			Dependencies: []string{"inspect_login"},
			AssetIDs:     []uuid.UUID{loginAsset.ID},
			InputData: map[string]interface{}{
				"asset_id": loginAsset.ID.String(),
				"check_id": "http_x_content_type_options",
			},
		},
	})

	wiCheckHome, attCheckHome, sessCheckHome, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	checkToolHome := tools.NewRunSecurityCheckTool(run.ID, wiCheckHome.ID, attCheckHome.ID, sessCheckHome.ID, env.runService)
	checkArgsHome, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_security_headers",
	})
	outCheckHome, err := checkToolHome.Execute(ctx, checkArgsHome)
	if err != nil {
		t.Fatalf("check home execute: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheckHome.ID, attCheckHome.ID, sessCheckHome.ID, true, json.RawMessage(outCheckHome), "completed")

	wiCheckLogin, attCheckLogin, sessCheckLogin, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	checkToolLogin := tools.NewRunSecurityCheckTool(run.ID, wiCheckLogin.ID, attCheckLogin.ID, sessCheckLogin.ID, env.runService)
	checkArgsLogin, _ := json.Marshal(map[string]string{
		"asset_id": loginAsset.ID.String(),
		"check_id": "http_x_content_type_options",
	})
	outCheckLogin, err := checkToolLogin.Execute(ctx, checkArgsLogin)
	if err != nil {
		t.Fatalf("check login execute: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheckLogin.ID, attCheckLogin.ID, sessCheckLogin.ID, true, json.RawMessage(outCheckLogin), "completed")

	// Notice: healthAsset was NOT checked -> it should appear in Unchecked Assets!

	// Generate programmatic summary report
	summary := orchestration.GenerateProgrammaticSummary(ctx, run.ID, env.runService, model.CauseCompleted)

	// Verify report sections
	if !strings.Contains(summary, "Deterministic Check Results") {
		t.Fatalf("expected summary to contain 'Deterministic Check Results', got:\n%s", summary)
	}
	if !strings.Contains(summary, "Candidate Findings (Pending Review)") {
		t.Fatalf("expected summary to contain 'Candidate Findings (Pending Review)', got:\n%s", summary)
	}
	if !strings.Contains(summary, "Unchecked Assets") {
		t.Fatalf("expected summary to contain 'Unchecked Assets', got:\n%s", summary)
	}
	if !strings.Contains(summary, healthAsset.ID.String()) {
		t.Fatalf("expected summary to list unchecked health asset %s, got:\n%s", healthAsset.ID, summary)
	}
	if !strings.Contains(summary, "**Termination Cause**: completed") {
		t.Fatalf("expected summary to retain termination cause, got:\n%s", summary)
	}

	// Also verify HTTP Catalog endpoint
	req := httptest.NewRequest("GET", "/api/checks/catalog", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/checks/catalog, got %d: %s", w.Code, w.Body.String())
	}
	var catRes struct {
		Checks []checker.CheckDefinition `json:"checks"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &catRes); err != nil {
		t.Fatalf("unmarshal catalog: %v", err)
	}
	if len(catRes.Checks) < 4 {
		t.Fatalf("expected at least 4 registered checks in catalog, got %d", len(catRes.Checks))
	}

	// Also verify HTTP Run Checks & Findings endpoints
	reqChecks := httptest.NewRequest("GET", fmt.Sprintf("/api/runs/%s/checks", run.ID), nil)
	wChecks := httptest.NewRecorder()
	env.router.ServeHTTP(wChecks, reqChecks)
	if wChecks.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/runs/{id}/checks, got %d: %s", wChecks.Code, wChecks.Body.String())
	}

	reqFindings := httptest.NewRequest("GET", fmt.Sprintf("/api/runs/%s/findings", run.ID), nil)
	wFindings := httptest.NewRecorder()
	env.router.ServeHTTP(wFindings, reqFindings)
	if wFindings.Code != http.StatusOK {
		t.Fatalf("expected 200 from /api/runs/{id}/findings, got %d: %s", wFindings.Code, wFindings.Body.String())
	}
}

// 10. Planner 不能安排未注册检查 (Planner rejects unregistered checks)
func TestPhase8_PlannerRejectsUnregisteredCheck(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Planner Catalog Enforcement",
		Objective: "Ensure planner can only schedule registered checks",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	planTool := tools.NewSubmitPlanTool(run.ID, env.runService, nil)

	// Submit with unregistered check_id
	badPlan := map[string]interface{}{
		"summary": "Plan with fake check",
		"items": []map[string]interface{}{
			{
				"key":       "fake_check",
				"title":     "Fake Check",
				"objective": "Run nonexistent check",
				"work_type": "run_security_check",
				"input_data": map[string]string{
					"asset_id": homeAsset.ID.String(),
					"check_id": "nonexistent_check_xyz",
				},
				"asset_ids": []string{homeAsset.ID.String()},
			},
		},
	}
	badBytes, _ := json.Marshal(badPlan)

	err := planTool.Validate(badBytes)
	if err == nil {
		t.Fatal("expected SubmitPlanTool.Validate to reject unregistered check_id, but passed")
	}
	if !strings.Contains(err.Error(), "unknown check_id") {
		t.Fatalf("expected error mentioning unknown check_id, got: %v", err)
	}

	// Also verify SubmitPlan at service layer rejects it
	items := []service.PlanItemSpec{
		{
			Key:       "fake_check",
			Title:     "Fake Check",
			Objective: "Run nonexistent check",
			WorkType:  "run_security_check",
			InputData: map[string]interface{}{
				"asset_id": homeAsset.ID.String(),
				"check_id": "nonexistent_check_xyz",
			},
			AssetIDs: []uuid.UUID{homeAsset.ID},
		},
	}
	_, err = env.runService.SubmitPlan(ctx, run.ID, items)
	if err == nil {
		t.Fatal("expected SubmitPlan service layer to reject unregistered check_id, but passed")
	}
	if !strings.Contains(err.Error(), "unknown check_id") {
		t.Fatalf("expected service error mentioning unknown check_id, got: %v", err)
	}
}
