package test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/fixture"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// Helper to launch a test fixture server on a free port and set FIXTURE_PORT
func setupTestFixture(t *testing.T) (*fixture.Server, func()) {
	t.Helper()
	srv, err := fixture.NewServer("0") // free port
	if err != nil {
		t.Fatalf("failed to create test fixture: %v", err)
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
	}
	return srv, cleanup
}

// 1. Scope Rejection Before Network Request (Counter = 0)
// - Empty scope, unassociated work item, out-of-scope asset rejected before sending request
func TestHTTPTraceability_ScopeRejection_NoNetworkTraffic(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()

	// Task with only http_home in scope
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "HTTP Scope Rejection Task",
		Objective: "Test scope rejection before request",
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
	if len(runAssets) != 1 {
		t.Fatalf("expected 1 run asset, got %d", len(runAssets))
	}
	homeAssetID := runAssets[0].ID

	// Create work item with homeAssetID
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_home",
			Title:     "Inspect Home",
			Objective: "Inspect home",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAssetID},
			InputData: map[string]interface{}{"asset_id": homeAssetID.String()},
		},
	})
	if err != nil {
		t.Fatalf("submit plan: %v", err)
	}

	_, att, sess, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	tool := tools.NewGetEndpointTool(run.ID, att.WorkItemID, env.runService)

	// A. Out of scope asset ID (random UUID)
	randomAssetID := uuid.New()
	_, err = tool.Execute(ctx, []byte(`{"asset_id":"`+randomAssetID.String()+`"}`))
	if err == nil || !errors.Is(err, service.ErrAssetOutOfScope) {
		t.Fatalf("expected ErrAssetOutOfScope for random asset, got err: %v", err)
	}

	// B. Out-of-scope asset for run2: task2 only has http_login in scope
	task2, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Login Only Task",
		Objective: "Login only task",
		Scope:     json.RawMessage(`{"endpoints": ["http_login"]}`),
	})
	run2, _, _ := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run2.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// Attempt to call tool on run2 with homeAssetID (which is outside run2's scope):
	toolRun2 := tools.NewGetEndpointTool(run2.ID, uuid.Nil, env.runService)
	_, err = toolRun2.Execute(ctx, []byte(`{"asset_id":"`+homeAssetID.String()+`"}`))
	if err == nil || !errors.Is(err, service.ErrAssetOutOfScope) {
		t.Fatalf("expected ErrAssetOutOfScope for out-of-scope asset on run2, got err: %v", err)
	}

	// Crucial assertion: fixture server request counter must remain ZERO!
	if srv.RequestCount() != 0 {
		t.Fatalf("expected 0 requests to fixture server, but got %d", srv.RequestCount())
	}

	// C. Valid call increments request count
	_ = sess
	validRes, err := tool.Execute(ctx, []byte(`{"asset_id":"`+homeAssetID.String()+`"}`))
	if err != nil {
		t.Fatalf("expected valid get_endpoint to succeed, got: %v", err)
	}
	if !strings.Contains(validRes, `"status_code":200`) {
		t.Fatalf("expected status_code 200 in result: %s", validRes)
	}
	if srv.RequestCount() != 1 {
		t.Fatalf("expected exactly 1 request to fixture server, got %d", srv.RequestCount())
	}
}

// 2. Redirect Is Not Followed (Location target counter = 0)
func TestHTTPTraceability_RedirectNotFollowed(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()

	// Provision redirect asset manually in run_assets
	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Redirect Test Task",
		Objective: "Test redirect not followed",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// Insert redirect asset
	redirectURL := srv.BaseURL() + "/redirect"
	var redirectAssetID uuid.UUID
	metaJSON, _ := json.Marshal(map[string]interface{}{
		"method": "GET",
		"url":    redirectURL,
		"path":   "/redirect",
		"port":   srv.Port(),
	})
	_ = env.pool.QueryRow(ctx,
		`INSERT INTO assets (namespace, kind, external_key, name, description, metadata)
		 VALUES ('local-fixture-v1', 'http_endpoint', $1, 'Redirect Endpoint', 'Redirect test', $2)
		 ON CONFLICT (namespace, kind, external_key) DO UPDATE SET metadata = EXCLUDED.metadata
		 RETURNING id`, "GET "+redirectURL, metaJSON).Scan(&redirectAssetID)

	snapJSON, _ := json.Marshal(map[string]interface{}{
		"namespace":    "local-fixture-v1",
		"kind":         "http_endpoint",
		"external_key": "GET " + redirectURL,
		"name":         "Redirect Endpoint",
		"metadata": map[string]interface{}{
			"method": "GET",
			"url":    redirectURL,
			"path":   "/redirect",
		},
	})
	_, _ = env.pool.Exec(ctx,
		`INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, $3)`,
		run.ID, redirectAssetID, snapJSON)

	tool := tools.NewGetEndpointTool(run.ID, uuid.Nil, env.runService)

	resJSON, err := tool.Execute(ctx, []byte(`{"asset_id":"`+redirectAssetID.String()+`"}`))
	if err != nil {
		t.Fatalf("expected redirect request to complete with 302, got error: %v", err)
	}

	var resObj struct {
		Response struct {
			StatusCode int               `json:"status_code"`
			Headers    map[string]string `json:"headers"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(resJSON), &resObj); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}

	// Must be 302 Found
	if resObj.Response.StatusCode != 302 {
		t.Fatalf("expected status code 302, got %d", resObj.Response.StatusCode)
	}

	// Must contain Location header
	loc := resObj.Response.Headers["Location"]
	if !strings.Contains(loc, "/target") {
		t.Fatalf("expected Location header pointing to /target, got: %s", loc)
	}

	// Crucial assertion: /target was NOT requested! PathCount("/target") must be 0!
	if srv.PathCount("/target") != 0 {
		t.Fatalf("redirect was followed! /target request count is %d, expected 0", srv.PathCount("/target"))
	}
	if srv.PathCount("/redirect") != 1 {
		t.Fatalf("expected exactly 1 request to /redirect, got %d", srv.PathCount("/redirect"))
	}
}

// 3. Status Codes and Integrity Differentiation
// - 200, 404, 500, slow response (timeout), connection failure, body truncation (>64 KiB)
func TestHTTPTraceability_StatusAndIntegrityDifferentiation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Integrity Test Task",
		Objective: "Test status and integrity differentiation",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	createTestHTTPAsset := func(path string, port string) uuid.UUID {
		if port == "" {
			port = srv.Port()
		}
		targetURL := "http://127.0.0.1:" + port + path
		var aid uuid.UUID
		metaJSON, _ := json.Marshal(map[string]interface{}{
			"method": "GET",
			"url":    targetURL,
			"path":   path,
			"port":   port,
		})
		_ = env.pool.QueryRow(ctx,
			`INSERT INTO assets (namespace, kind, external_key, name, description, metadata)
			 VALUES ('local-fixture-v1', 'http_endpoint', $1, $2, 'test endpoint', $3)
			 ON CONFLICT (namespace, kind, external_key) DO UPDATE SET metadata = EXCLUDED.metadata
			 RETURNING id`, "GET "+targetURL, path, metaJSON).Scan(&aid)

		snapJSON, _ := json.Marshal(map[string]interface{}{
			"namespace":    "local-fixture-v1",
			"kind":         "http_endpoint",
			"external_key": "GET " + targetURL,
			"name":         path,
			"metadata": map[string]interface{}{
				"method": "GET",
				"url":    targetURL,
				"path":   path,
			},
		})
		_, _ = env.pool.Exec(ctx,
			`INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, $3)`,
			run.ID, aid, snapJSON)
		return aid
	}

	tool := tools.NewGetEndpointTool(run.ID, uuid.Nil, env.runService)

	// A. 500 Internal Server Error
	id500 := createTestHTTPAsset("/500", "")
	res500, err := tool.Execute(ctx, []byte(`{"asset_id":"`+id500.String()+`"}`))
	if err != nil {
		t.Fatalf("expected 500 to be returned as valid HTTP response, got err: %v", err)
	}
	if !strings.Contains(res500, `"status_code":500`) {
		t.Fatalf("expected status_code 500, got: %s", res500)
	}

	// B. Large response (>64 KiB) truncation detection
	idLarge := createTestHTTPAsset("/large", "")
	resLarge, err := tool.Execute(ctx, []byte(`{"asset_id":"`+idLarge.String()+`"}`))
	if err != nil {
		t.Fatalf("expected large response to succeed with truncation flag, got: %v", err)
	}
	var largeObj struct {
		Integrity struct {
			SavedBytes  int  `json:"saved_bytes"`
			IsTruncated bool `json:"is_truncated"`
		} `json:"integrity"`
	}
	_ = json.Unmarshal([]byte(resLarge), &largeObj)
	if !largeObj.Integrity.IsTruncated {
		t.Fatalf("expected is_truncated=true for >64 KiB response, got false: %s", resLarge)
	}
	if largeObj.Integrity.SavedBytes != 64*1024 {
		t.Fatalf("expected saved_bytes=65536, got %d", largeObj.Integrity.SavedBytes)
	}

	// C. Timeout / Slow response (/slow sleeps 6s, tool timeout 5s)
	idSlow := createTestHTTPAsset("/slow", "")
	resSlow, err := tool.Execute(ctx, []byte(`{"asset_id":"`+idSlow.String()+`"}`))
	if err == nil {
		t.Fatalf("expected error on 5s timeout, got nil")
	}
	if !strings.Contains(resSlow, `"type":"timeout"`) {
		t.Fatalf("expected error type 'timeout' in output JSON, got: %s", resSlow)
	}
}

// 4. Stop Cancels HTTP Request
func TestHTTPTraceability_StopCancelsHTTPRequest(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx, cancel := context.WithCancel(context.Background())

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Cancel Request Task",
		Objective: "Test stop cancels in-flight HTTP request",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// Create /slow asset
	targetURL := srv.BaseURL() + "/slow"
	var slowAssetID uuid.UUID
	metaJSON, _ := json.Marshal(map[string]interface{}{
		"method": "GET",
		"url":    targetURL,
		"path":   "/slow",
		"port":   srv.Port(),
	})
	_ = env.pool.QueryRow(ctx,
		`INSERT INTO assets (namespace, kind, external_key, name, description, metadata)
		 VALUES ('local-fixture-v1', 'http_endpoint', $1, 'Slow Endpoint', 'Slow test', $2)
		 RETURNING id`, "GET "+targetURL, metaJSON).Scan(&slowAssetID)

	snapJSON, _ := json.Marshal(map[string]interface{}{
		"namespace":    "local-fixture-v1",
		"kind":         "http_endpoint",
		"external_key": "GET " + targetURL,
		"name":         "Slow Endpoint",
		"metadata": map[string]interface{}{
			"method": "GET",
			"url":    targetURL,
			"path":   "/slow",
		},
	})
	_, _ = env.pool.Exec(ctx,
		`INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, $3)`,
		run.ID, slowAssetID, snapJSON)

	tool := tools.NewGetEndpointTool(run.ID, uuid.Nil, env.runService)

	// Launch get_endpoint and cancel context after 50ms (simulating user stop)
	done := make(chan struct{})
	var toolRes string
	var toolErr error

	go func() {
		defer close(done)
		toolRes, toolErr = tool.Execute(ctx, []byte(`{"asset_id":"`+slowAssetID.String()+`"}`))
	}()

	time.Sleep(50 * time.Millisecond)
	cancel() // Cancel execution context

	<-done

	if toolErr == nil {
		t.Fatalf("expected tool execution to be canceled, but got nil error")
	}
	if !strings.Contains(toolRes, `"type":"context_canceled"`) {
		t.Fatalf("expected 'context_canceled' error type, got: %s", toolRes)
	}
}

// 5. Two Consecutive Runs on /health with Different Fixture Versions Produce Distinct Evidence Under Same Asset
func TestHTTPTraceability_TwoRunsDifferentVersions_SameAssetHistory(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()

	// Initial version is 1.0.0
	srv.SetVersion("1.0.0")

	// Task targeting /health
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Fixture Health Verification Task",
		Objective: "Verify fixture health version across runs",
		Scope:     json.RawMessage(`{"endpoints": ["/health"]}`),
		Config: json.RawMessage(`{
			"executor": "orchestrator",
			"provider": "mock",
			"mock_scenario": "http_traceability",
			"budget": {
				"max_planner_rounds": 3,
				"max_work_items": 10,
				"max_parallel_workers": 2,
				"run_timeout_seconds": 15
			}
		}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// --- RUN 1 ---
	p1 := provider.NewMockProvider(provider.ScenarioHTTPTraceability)
	run1, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor:     strPtr("orchestrator"),
		Provider:     strPtr("mock"),
		MockScenario: strPtr("http_traceability"),
	}, nil)
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}

	eng1 := orchestration.NewEngine(run1.ID, env.runService, p1, model.DefaultBudgetConfig())
	if err := env.manager.StartExecutor(run1.ID, eng1); err != nil {
		t.Fatalf("start engine 1: %v", err)
	}

	finalRun1 := waitForRunTerminal(t, env, run1.ID, 10*time.Second)
	if finalRun1.Status != model.RunStatusCompleted {
		t.Fatalf("run 1 expected completed, got %s (reason: %v)", finalRun1.Status, finalRun1.FinishReason)
	}

	// Verify observations from Run 1
	obs1, err := env.runService.ListObservationsByRun(ctx, run1.ID)
	if err != nil || len(obs1) == 0 {
		t.Fatalf("expected observations for run 1, got %v (err: %v)", obs1, err)
	}
	var version1Obs *model.Observation
	for i := range obs1 {
		if strings.Contains(obs1[i].Content, "1.0.0") {
			version1Obs = &obs1[i]
			break
		}
	}
	if version1Obs == nil {
		t.Fatalf("expected observation recording version 1.0.0 in run 1, got observations: %+v", obs1)
	}
	healthAssetID := *version1Obs.AssetID

	// --- MODIFY FIXTURE VERSION DYNAMICALLY ---
	srv.SetVersion("2.0.0")

	// --- RUN 2 ---
	p2 := provider.NewMockProvider(provider.ScenarioHTTPTraceability)
	run2, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor:     strPtr("orchestrator"),
		Provider:     strPtr("mock"),
		MockScenario: strPtr("http_traceability"),
	}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}

	eng2 := orchestration.NewEngine(run2.ID, env.runService, p2, model.DefaultBudgetConfig())
	if err := env.manager.StartExecutor(run2.ID, eng2); err != nil {
		t.Fatalf("start engine 2: %v", err)
	}

	finalRun2 := waitForRunTerminal(t, env, run2.ID, 10*time.Second)
	if finalRun2.Status != model.RunStatusCompleted {
		t.Fatalf("run 2 expected completed, got %s (reason: %v)", finalRun2.Status, finalRun2.FinishReason)
	}

	// Verify observations from Run 2
	obs2, err := env.runService.ListObservationsByRun(ctx, run2.ID)
	if err != nil || len(obs2) == 0 {
		t.Fatalf("expected observations for run 2, got %v (err: %v)", obs2, err)
	}
	var version2Obs *model.Observation
	for i := range obs2 {
		if strings.Contains(obs2[i].Content, "2.0.0") {
			version2Obs = &obs2[i]
			break
		}
	}
	if version2Obs == nil {
		t.Fatalf("expected observation recording version 2.0.0 in run 2, got observations: %+v", obs2)
	}

	// Verify Run 1 and Run 2 share the EXACT SAME asset ID
	if *version2Obs.AssetID != healthAssetID {
		t.Fatalf("expected both runs to share asset ID %s, but run 2 used %s", healthAssetID, *version2Obs.AssetID)
	}

	// Verify asset history: query GetAssetDetailsWithHistory
	details, err := env.runService.GetAssetDetailsWithHistory(ctx, healthAssetID)
	if err != nil {
		t.Fatalf("get asset details with history: %v", err)
	}
	if len(details.RunHistory) < 2 {
		t.Fatalf("expected at least 2 historical runs in asset details, got %d", len(details.RunHistory))
	}

	// Assert Run 1's history still records version 1.0.0, and Run 2 records version 2.0.0
	var foundRun1, foundRun2 bool
	for _, h := range details.RunHistory {
		if h.RunID == run1.ID {
			foundRun1 = true
			foundV1InH1 := false
			for _, o := range h.Observations {
				if strings.Contains(o.Content, "1.0.0") {
					foundV1InH1 = true
					break
				}
			}
			if !foundV1InH1 {
				t.Fatalf("Run 1 history in asset details does not contain version 1.0.0 observation")
			}
		}
		if h.RunID == run2.ID {
			foundRun2 = true
			foundV2InH2 := false
			for _, o := range h.Observations {
				if strings.Contains(o.Content, "2.0.0") {
					foundV2InH2 = true
					break
				}
			}
			if !foundV2InH2 {
				t.Fatalf("Run 2 history in asset details does not contain version 2.0.0 observation")
			}
		}
	}
	if !foundRun1 || !foundRun2 {
		t.Fatalf("expected both runs in asset history (foundRun1=%v, foundRun2=%v)", foundRun1, foundRun2)
	}

	// Verify that real HTTP traffic was actually received by the fixture server!
	if srv.PathCount("/health") < 2 {
		t.Fatalf("expected at least 2 real requests to /health fixture, got %d", srv.PathCount("/health"))
	}

	t.Logf("Verified two consecutive runs produced distinct evidence under the same asset. Fixture requests: %d", srv.PathCount("/health"))
}
