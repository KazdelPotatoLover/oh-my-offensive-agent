package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// 1. Initial RunAssets Only Contains Seed Asset (Never Leaks Existing Global Assets)
func TestDiscovery_InitialRunAssetsOnlySeed(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()

	// Pre-populate global assets with /login and /health
	fixtureOrigin := fmt.Sprintf("http://127.0.0.1:%s", srv.Port())
	loginURL := fixtureOrigin + "/login"
	healthURL := fixtureOrigin + "/health"

	_, err := env.pool.Exec(ctx,
		`INSERT INTO assets (id, namespace, kind, external_key, name, description, metadata)
		 VALUES 
		 (gen_random_uuid(), 'local-fixture-v1', 'http_endpoint', 'GET ' || $1, 'Global Login', 'Existing login', '{}'),
		 (gen_random_uuid(), 'local-fixture-v1', 'http_endpoint', 'GET ' || $2, 'Global Health', 'Existing health', '{}')`,
		loginURL, healthURL)
	if err != nil {
		t.Fatalf("failed pre-populating global assets: %v", err)
	}

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"origin": "%s",
			"allowed_methods": ["GET"],
			"allowed_paths": ["/", "/login", "/health"]
		}
	}`, fixtureOrigin)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Discovery Isolation Task",
		Objective: "Test initial seed isolation",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Verify run_assets ONLY contains the seed asset ("/")
	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list run assets: %v", err)
	}

	if len(runAssets) != 1 {
		t.Fatalf("expected exactly 1 run_asset initially (the seed), got %d", len(runAssets))
	}

	expectedKey := fmt.Sprintf("GET %s/", fixtureOrigin)
	if runAssets[0].ExternalKey != expectedKey {
		t.Fatalf("expected seed asset %s, got %s", expectedKey, runAssets[0].ExternalKey)
	}

	// Verify global assets count is 3 (/login, /health, /)
	var globalCount int
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM assets`).Scan(&globalCount)
	if globalCount != 3 {
		t.Fatalf("expected 3 total global assets, got %d", globalCount)
	}
}

// 2. Full Chain Dynamic Discovery: / -> /login -> /health
func TestDiscovery_FullChain_MockAndFixture(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()
	fixtureOrigin := fmt.Sprintf("http://127.0.0.1:%s", srv.Port())

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"origin": "%s",
			"allowed_methods": ["GET"],
			"allowed_paths": ["/", "/login", "/health"]
		}
	}`, fixtureOrigin)

	cfgJSON := `{
		"agent_config": {
			"executor": "orchestrator",
			"provider": "mock",
			"mock_scenario": "discovery"
		},
		"budget": {
			"max_rounds": 10,
			"max_model_calls": 30,
			"max_tool_calls": 30,
			"max_duration_seconds": 30
		}
	}`

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Full Chain Discovery",
		Objective: "Discover endpoints starting from home",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(cfgJSON),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	mockP := provider.NewMockProvider(provider.ScenarioDiscovery)
	budget := model.DefaultBudgetConfig()
	budget.MaxPlannerRounds = 10
	budget.MaxModelCalls = 30
	budget.MaxToolCalls = 30
	budget.RunTimeoutSeconds = 30

	engine := orchestration.NewEngine(run.ID, env.runService, mockP, budget)
	err = env.manager.StartExecutor(run.ID, engine)
	if err != nil {
		t.Fatalf("start executor: %v", err)
	}

	// Wait for run completion
	completedRun := waitForRunTerminal(t, env, run.ID, 15*time.Second)
	if completedRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected run completed, got %s (reason: %v)", completedRun.Status, completedRun.FinishReason)
	}

	// Verify all 3 endpoints are in run_assets
	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list run assets: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("expected 3 run_assets discovered, got %d", len(assets))
	}

	// Verify work items: home, login, health, aggregate_summary
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list work items: %v", err)
	}
	if len(items) != 4 {
		t.Fatalf("expected 4 work items, got %d", len(items))
	}

	// Verify work item keys and statuses
	itemKeyMap := make(map[string]model.WorkItem)
	for _, it := range items {
		itemKeyMap[it.Key] = it
		if it.Status != "completed" {
			t.Fatalf("work item %s expected completed, got %s", it.Key, it.Status)
		}
	}

	if _, ok := itemKeyMap["inspect_http_home"]; !ok {
		t.Fatal("missing inspect_http_home work item")
	}
	if _, ok := itemKeyMap["inspect_http_login"]; !ok {
		t.Fatal("missing inspect_http_login work item")
	}
	if _, ok := itemKeyMap["inspect_http_health"]; !ok {
		t.Fatal("missing inspect_http_health work item")
	}
	if _, ok := itemKeyMap["aggregate_summary"]; !ok {
		t.Fatal("missing aggregate_summary work item")
	}

	// Verify provenance: inspect_http_login has source_observation_ids in work_item_sources
	loginWI := itemKeyMap["inspect_http_login"]
	var srcObsCount int
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM work_item_sources WHERE work_item_id = $1`, loginWI.ID).Scan(&srcObsCount)
	if srcObsCount == 0 {
		t.Fatal("inspect_http_login work item should have source_observation_ids from home discovery")
	}

	// Verify discoveries in database
	discoveries, err := env.runService.ListDiscoveriesByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list discoveries: %v", err)
	}
	if len(discoveries) < 2 {
		t.Fatalf("expected at least 2 discoveries (/ -> /login, /login -> /health), got %d", len(discoveries))
	}

	var foundHomeToLogin, foundLoginToHealth bool
	for _, d := range discoveries {
		if d.SourceAssetKey == fmt.Sprintf("GET %s/", fixtureOrigin) && d.TargetAssetKey == fmt.Sprintf("GET %s/login", fixtureOrigin) {
			foundHomeToLogin = true
		}
		if d.SourceAssetKey == fmt.Sprintf("GET %s/login", fixtureOrigin) && d.TargetAssetKey == fmt.Sprintf("GET %s/health", fixtureOrigin) {
			foundLoginToHealth = true
		}
	}
	if !foundHomeToLogin {
		t.Fatal("missing discovery provenance from / to /login")
	}
	if !foundLoginToHealth {
		t.Fatal("missing discovery provenance from /login to /health")
	}
}

// 3. Toggleable Link: Disabling /login link stops discovery at home; re-enabling restores it
func TestDiscovery_ToggleLinkStopsDiscovery(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()
	fixtureOrigin := fmt.Sprintf("http://127.0.0.1:%s", srv.Port())

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"origin": "%s",
			"allowed_methods": ["GET"],
			"allowed_paths": ["/", "/login", "/health"]
		}
	}`, fixtureOrigin)

	cfgJSON := `{
		"agent_config": {
			"executor": "orchestrator",
			"provider": "mock",
			"mock_scenario": "discovery"
		},
		"budget": {
			"max_rounds": 10,
			"max_model_calls": 30,
			"max_tool_calls": 30,
			"max_duration_seconds": 30
		}
	}`

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Toggle Link Discovery",
		Objective: "Test controlled link disabling",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(cfgJSON),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// 3a. Disable login link on fixture
	srv.SetEnableLoginLink(false)

	run1, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run1: %v", err)
	}

	mockP1 := provider.NewMockProvider(provider.ScenarioDiscovery)
	budget := model.DefaultBudgetConfig()
	budget.MaxPlannerRounds = 10
	budget.MaxModelCalls = 30
	budget.MaxToolCalls = 30
	budget.RunTimeoutSeconds = 30

	engine1 := orchestration.NewEngine(run1.ID, env.runService, mockP1, budget)
	_ = env.manager.StartExecutor(run1.ID, engine1)

	completedRun1 := waitForRunTerminal(t, env, run1.ID, 15*time.Second)
	if completedRun1.Status != model.RunStatusCompleted {
		t.Fatalf("run1 expected completed, got %s", completedRun1.Status)
	}

	// Run1 must ONLY have inspected "/" and then finished!
	assets1, _ := env.runService.ListAssetsByRun(ctx, run1.ID)
	if len(assets1) != 1 {
		t.Fatalf("expected run1 to have only 1 asset (/), got %d", len(assets1))
	}
	discoveries1, _ := env.runService.ListDiscoveriesByRun(ctx, run1.ID)
	if len(discoveries1) != 0 {
		t.Fatalf("expected run1 to have 0 discoveries, got %d", len(discoveries1))
	}

	// 3b. Re-enable login link on fixture
	srv.SetEnableLoginLink(true)

	run2, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run2: %v", err)
	}

	mockP2 := provider.NewMockProvider(provider.ScenarioDiscovery)
	engine2 := orchestration.NewEngine(run2.ID, env.runService, mockP2, budget)
	_ = env.manager.StartExecutor(run2.ID, engine2)

	completedRun2 := waitForRunTerminal(t, env, run2.ID, 15*time.Second)
	if completedRun2.Status != model.RunStatusCompleted {
		t.Fatalf("run2 expected completed, got %s", completedRun2.Status)
	}

	// Run2 must have discovered both /login and /health!
	assets2, _ := env.runService.ListAssetsByRun(ctx, run2.ID)
	if len(assets2) != 3 {
		t.Fatalf("expected run2 to discover 3 assets, got %d", len(assets2))
	}
	discoveries2, _ := env.runService.ListDiscoveriesByRun(ctx, run2.ID)
	if len(discoveries2) < 2 {
		t.Fatalf("expected run2 to have at least 2 discoveries, got %d", len(discoveries2))
	}
}

// 4. Duplicate and Cyclic Links Do Not Create Duplicate Assets or Infinite Loops
func TestDiscovery_CyclicAndDuplicateLinks(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	ctx := context.Background()
	fixtureOrigin := fmt.Sprintf("http://127.0.0.1:%s", srv.Port())

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"origin": "%s",
			"allowed_methods": ["GET"],
			"allowed_paths": ["/", "/login", "/health"]
		}
	}`, fixtureOrigin)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Cyclic Discovery",
		Objective: "Test loop and duplicate resilience",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	mockP := provider.NewMockProvider(provider.ScenarioDiscovery)
	budget := model.DefaultBudgetConfig()
	budget.MaxPlannerRounds = 10
	budget.MaxModelCalls = 30
	budget.MaxToolCalls = 30
	budget.RunTimeoutSeconds = 30

	engine := orchestration.NewEngine(run.ID, env.runService, mockP, budget)
	_ = env.manager.StartExecutor(run.ID, engine)

	completedRun := waitForRunTerminal(t, env, run.ID, 15*time.Second)
	if completedRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected completed, got %s", completedRun.Status)
	}

	// Check that UNIQUE constraint on asset_discoveries prevented duplicate records
	var duplicateCount int
	err = env.pool.QueryRow(ctx,
		`SELECT count(*) FROM (
			SELECT run_id, source_asset_id, target_asset_id, raw_href, count(*)
			FROM asset_discoveries
			WHERE run_id = $1
			GROUP BY run_id, source_asset_id, target_asset_id, raw_href
			HAVING count(*) > 1
		) duplicates`, run.ID).Scan(&duplicateCount)
	if err != nil {
		t.Fatalf("query duplicate discoveries: %v", err)
	}
	if duplicateCount > 0 {
		t.Fatalf("found %d duplicate discovery rows", duplicateCount)
	}

	// Exactly 4 work items (home, login, health, aggregate_summary)
	items, _ := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if len(items) != 4 {
		t.Fatalf("expected 4 work items, got %d", len(items))
	}
}

// 5. Out-of-Scope Links Rejected with Zero Network Traffic to Secondary Local Server
func TestDiscovery_OutOfScopeLinksRejected_ZeroCrossTraffic(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	srv, cleanupFixture := setupTestFixture(t)
	defer cleanupFixture()

	// Secondary local server to prove no out-of-scope requests ever leak
	var secondaryCounter int32
	secondaryServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondaryCounter, 1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("secondary server response"))
	}))
	defer secondaryServer.Close()

	ctx := context.Background()
	fixtureOrigin := fmt.Sprintf("http://127.0.0.1:%s", srv.Port())

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"origin": "%s",
			"allowed_methods": ["GET"],
			"allowed_paths": ["/", "/login", "/health"]
		}
	}`, fixtureOrigin)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Scope Rejection Test",
		Objective: "Verify strict rejection of invalid links",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatalf("transition run: %v", err)
	}

	// Seed asset ID
	assets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := assets[0]

	// Submit plan for home and claim next pending work item
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_http_home",
			Title:     "Inspect Home",
			Objective: "Fetch home",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	if err != nil {
		t.Fatalf("submit plan: %v", err)
	}

	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim work item: %v", err)
	}
	_ = att

	// Simulate get_endpoint tool call with HTML containing invalid/filtered links
	htmlContent := fmt.Sprintf(`
		<html>
		<body>
			<a href="/login">Valid Login</a>
			<a href="/outside-scope">Outside Scope Path</a>
			<a href="%s/">Other Local Server</a>
			<a href="http://evil.com/exploit">External Domain</a>
			<a href="mailto:admin@example.com">Mailto Link</a>
			<a href="javascript:alert(1)">Javascript URI</a>
			<a href="/search?q=query">Query Parameter Link</a>
			<a href="#section">Pure Fragment Link</a>
		</body>
		</html>
	`, secondaryServer.URL)

	getToolCall, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_get_home", "get_endpoint", json.RawMessage(fmt.Sprintf(`{"asset_id":"%s"}`, homeAsset.ID)))
	if err != nil {
		t.Fatalf("create tool call: %v", err)
	}

	respData := map[string]interface{}{
		"request": map[string]interface{}{
			"url":      fixtureOrigin + "/",
			"asset_id": homeAsset.ID.String(),
		},
		"response": map[string]interface{}{
			"status_code": 200,
			"headers": map[string]string{
				"Content-Type": "text/html; charset=utf-8",
			},
			"body": htmlContent,
		},
	}
	respBytes, _ := json.Marshal(respData)
	now := time.Now()
	err = env.runService.UpdateAgentToolCall(ctx, getToolCall.ID, "success", string(respBytes), nil, &now, &now)
	if err != nil {
		t.Fatalf("update tool call: %v", err)
	}

	// Call extract_endpoint_links tool
	extractTool := tools.NewExtractEndpointLinksTool(run.ID, wi.ID, sess.ID, env.runService)
	outputStr, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, getToolCall.ID)))
	if err != nil {
		t.Fatalf("extractTool failed: %v", err)
	}

	var result struct {
		Discovered []struct {
			Path string `json:"path"`
		} `json:"discovered"`
		Existing []struct {
			Path string `json:"path"`
		} `json:"existing"`
		Rejected []struct {
			RawHref string `json:"raw_href"`
			Reason  string `json:"reason"`
		} `json:"rejected"`
	}
	if err := json.Unmarshal([]byte(outputStr), &result); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}

	// Verify secondary server received ZERO requests
	if count := atomic.LoadInt32(&secondaryCounter); count != 0 {
		t.Fatalf("expected secondary server counter 0, got %d", count)
	}

	// Verify only /login was discovered
	if len(result.Discovered) != 1 || result.Discovered[0].Path != "/login" {
		t.Fatalf("expected only /login discovered, got %+v", result.Discovered)
	}

	// Verify rejected list contains reasons for outside-scope, other port/host, mailto, javascript, query
	rejectedMap := make(map[string]string)
	for _, r := range result.Rejected {
		rejectedMap[r.RawHref] = r.Reason
	}

	if _, ok := rejectedMap["/outside-scope"]; !ok {
		t.Error("expected /outside-scope in rejected items")
	}
	if _, ok := rejectedMap[secondaryServer.URL+"/"]; !ok {
		t.Error("expected secondary server URL in rejected items")
	}
	if _, ok := rejectedMap["http://evil.com/exploit"]; !ok {
		t.Error("expected external URL in rejected items")
	}
	if _, ok := rejectedMap["mailto:admin@example.com"]; !ok {
		t.Error("expected mailto in rejected items")
	}
	if _, ok := rejectedMap["javascript:alert(1)"]; !ok {
		t.Error("expected javascript in rejected items")
	}
	if _, ok := rejectedMap["/search?q=query"]; !ok {
		t.Error("expected query param link in rejected items")
	}
}

// 6. Source ToolCall Provenance and Strict Validation
func TestDiscovery_ProvenanceAndValidation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Provenance Task",
		Objective: "Test provenance validation",
		Scope:     json.RawMessage(`{"mode":"discovery","seeds":["/"],"discovery_policy":{"origin":"http://127.0.0.1:29090","allowed_methods":["GET"],"allowed_paths":["/","/login"]}}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	sess, _ := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil)

	extractTool := tools.NewExtractEndpointLinksTool(run.ID, uuid.Nil, sess.ID, env.runService)

	// a) Non-existent source_tool_call_id rejected
	_, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, uuid.New())))
	if err == nil {
		t.Fatal("expected error for non-existent tool call")
	}

	// b) Tool call from different session rejected
	otherSess, _ := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil)
	otherTC, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &otherSess.ID, nil, "call_other", "get_endpoint", json.RawMessage(`{}`))
	_, err = extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, otherTC.ID)))
	if err == nil {
		t.Fatal("expected error for tool call belonging to different session")
	}

	// c) Tool call that failed (error != nil) rejected
	failedTC, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_fail", "get_endpoint", json.RawMessage(`{}`))
	errStr := "connection refused"
	nowTC := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, failedTC.ID, "failed", "", &errStr, &nowTC, &nowTC)
	_, err = extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, failedTC.ID)))
	if err == nil {
		t.Fatal("expected error for failed tool call")
	}
}

// 7. Stopping Run Aborts Discovery Registration
func TestDiscovery_StoppingRunAbortsRegistration(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Stopping Discovery Task",
		Objective: "Test stopping aborts registration",
		Scope:     json.RawMessage(`{"mode":"discovery","seeds":["/"],"discovery_policy":{"origin":"http://127.0.0.1:29090","allowed_methods":["GET"],"allowed_paths":["/","/login"]}}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	sess, _ := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil)

	// Transition run to stopping
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusStopping, nil)

	tc, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_valid", "get_endpoint", json.RawMessage(`{}`))
	respData := map[string]interface{}{
		"request":  map[string]interface{}{"url": "http://127.0.0.1:29090/"},
		"response": map[string]interface{}{"status_code": 200, "headers": map[string]string{"Content-Type": "text/html"}, "body": `<a href="/login">Login</a>`},
	}
	respBytes, _ := json.Marshal(respData)
	nowTC := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", string(respBytes), nil, &nowTC, &nowTC)

	extractTool := tools.NewExtractEndpointLinksTool(run.ID, uuid.Nil, sess.ID, env.runService)
	_, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, tc.ID)))
	if err == nil {
		t.Fatal("expected error registering discovery when run is stopping")
	}
}

// 8. Real Target Range: Live Connectivity, Dynamic Extraction and Provenance
func TestDiscovery_RealTargetRange_LiveConnectivityAndExtraction(t *testing.T) {
	// First check if target range is reachable
	targetURL := "http://192.168.77.88:7780/"
	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(targetURL)
	if err != nil {
		t.Skipf("skipping live target range test: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Skipf("skipping live target range test: status code %d", resp.StatusCode)
	}

	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Live Target Range Discovery",
		Objective: "Discover pages on Qiyue manufacturing target portal",
		Scope: json.RawMessage(`{
			"mode": "discovery",
			"seeds": ["http://192.168.77.88:7780/"],
			"discovery_policy": {
				"origin": "http://192.168.77.88:7780",
				"mode": "same_origin_html",
				"allowed_methods": ["GET"],
				"max_pages": 20,
				"max_depth": 2,
				"request_interval_ms": 100
			}
		}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// 1. Initial RunAssets must only contain 1 seed asset
	var initialCount int
	err = env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_assets WHERE run_id = $1`, run.ID).Scan(&initialCount)
	if err != nil || initialCount != 1 {
		t.Fatalf("expected initial run_assets to contain exactly 1 seed, got %d (err: %v)", initialCount, err)
	}

	budget := model.BudgetConfig{
		MaxModelCalls:       60,
		MaxToolCalls:        60,
		RunTimeoutSeconds:   120,
		ModelTimeoutSeconds: 30,
		ToolTimeoutSeconds:  5,
		MaxToolOutputBytes:  65536, // 64 KiB — real target pages are ~7 KiB; headers must be visible
		MaxParallelWorkers:  1,
	}

	mockP := provider.NewMockProvider(provider.ScenarioDiscovery)
	engine := orchestration.NewEngine(run.ID, env.runService, mockP, budget)

	engine.Run(ctx)

	// Check final run status
	finalRun, err := env.runService.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get final run: %v", err)
	}
	if finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected run status 'completed', got '%s' (reason: %v)", finalRun.Status, finalRun.FinishReason)
	}

	rows, _ := env.pool.Query(ctx, `SELECT tool_name, status, output FROM agent_tool_calls WHERE run_id = $1 ORDER BY started_at`, run.ID)
	for rows.Next() {
		var tName, tStatus, tOut string
		_ = rows.Scan(&tName, &tStatus, &tOut)
		if len(tOut) > 200 {
			tOut = tOut[:200] + "..."
		}
		t.Logf("Tool Call: %s (%s) output: %s", tName, tStatus, tOut)
	}
	rows.Close()

	// Verify discoveries in database
	discoveries, err := env.runService.ListDiscoveriesByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("list discoveries: %v", err)
	}
	if len(discoveries) == 0 {
		t.Fatal("expected at least 1 discovery from homepage, got 0")
	}

	// Verify that endpoints from homepage (e.g. /news, /products, /downloads, /about, /contact) were discovered
	discoveredHrefs := make(map[string]bool)
	for _, d := range discoveries {
		discoveredHrefs[d.RawHref] = true
		if d.SourceAssetID == uuid.Nil || d.TargetAssetID == uuid.Nil || d.SourceToolCallID == uuid.Nil {
			t.Fatalf("discovery has invalid foreign keys: %+v", d)
		}
	}

	expectedFound := []string{"/news", "/products", "/downloads", "/about", "/contact"}
	for _, ep := range expectedFound {
		if !discoveredHrefs[ep] {
			t.Errorf("expected discovery of %s, but not found in %v", ep, discoveredHrefs)
		}
	}

	t.Logf("Live target range discovery verified! Total discovered: %d endpoints across %s", len(discoveries), targetURL)
}
