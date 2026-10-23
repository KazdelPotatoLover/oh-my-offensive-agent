package test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExtractAPIEndpoints_FromHTMLAndScripts(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()
	origin := "http://127.0.0.1:29090"

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"mode": "same_origin_html",
			"origin": "%s",
			"allowed_methods": ["GET", "POST", "DELETE"]
		}
	}`, origin)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "API Recon HTML Task",
		Objective: "Test API endpoint discovery from HTML and script tags",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)

	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, runAssets, 1)
	homeAsset := runAssets[0]

	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "item_inspect_api",
			Title:     "Inspect API Entry",
			Objective: "Inspect home and extract API endpoints",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	require.NoError(t, err)

	wi, _, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)

	htmlContent := `
		<!DOCTYPE html>
		<html>
		<head>
			<script src="/assets/bundle.js"></script>
		</head>
		<body>
			<h1>Application Dashboard</h1>
			<script>
				function login(u, p) {
					return axios.post('/api/v1/login', { username: u, password: p });
				}
				function getUsers() {
					return fetch('/api/v1/users');
				}
				function deleteItem(id) {
					return axios.delete('/api/v1/items/' + id);
				}
			</script>
		</body>
		</html>
	`

	getToolCall, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_api_entry", "get_endpoint", json.RawMessage(fmt.Sprintf(`{"asset_id":"%s"}`, homeAsset.ID)))
	require.NoError(t, err)

	respData := map[string]interface{}{
		"request": map[string]interface{}{
			"url":      origin + "/",
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
	require.NoError(t, err)

	extractTool := tools.NewExtractAPIEndpointsTool(run.ID, wi.ID, sess.ID, env.runService)
	outputStr, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, getToolCall.ID)))
	require.NoError(t, err)

	var result struct {
		Discovered []struct {
			AssetID uuid.UUID `json:"asset_id"`
			URL     string    `json:"url"`
			Path    string    `json:"path"`
		} `json:"discovered"`
		Existing []struct {
			Path string `json:"path"`
		} `json:"existing"`
		Rejected []struct {
			RawHref string `json:"raw_href"`
			Reason  string `json:"reason"`
		} `json:"rejected"`
	}
	err = json.Unmarshal([]byte(outputStr), &result)
	require.NoError(t, err)

	discoveredPaths := make(map[string]bool)
	for _, d := range result.Discovered {
		discoveredPaths[d.Path] = true
	}

	require.True(t, discoveredPaths["/assets/bundle.js"], "expected /assets/bundle.js to be discovered")
	require.True(t, discoveredPaths["/api/v1/login"], "expected /api/v1/login to be discovered")
	require.True(t, discoveredPaths["/api/v1/users"], "expected /api/v1/users to be discovered")

	// Verify assets has correct method in metadata
	var loginMethod string
	err = env.pool.QueryRow(ctx, `SELECT a.metadata->>'method' FROM run_assets ra JOIN assets a ON ra.asset_id=a.id WHERE ra.run_id=$1 AND a.external_key=$2`, run.ID, "POST "+origin+"/api/v1/login").Scan(&loginMethod)
	require.NoError(t, err)
	require.Equal(t, "POST", loginMethod)

	// Second execution on same source should find them as existing
	outputStr2, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, getToolCall.ID)))
	require.NoError(t, err)
	var result2 struct {
		Discovered []interface{} `json:"discovered"`
		Existing   []struct {
			Path string `json:"path"`
		} `json:"existing"`
	}
	err = json.Unmarshal([]byte(outputStr2), &result2)
	require.NoError(t, err)
	require.Len(t, result2.Discovered, 0)
	require.GreaterOrEqual(t, len(result2.Existing), 3)
}

func TestExtractAPIEndpoints_FromJavaScriptAndJSON(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()
	origin := "http://127.0.0.1:29090"

	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"mode": "same_origin_html",
			"origin": "%s",
			"allowed_methods": ["GET", "POST", "DELETE", "PUT"]
		}
	}`, origin)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "API Recon JS Task",
		Objective: "Test API endpoint discovery from JS and JSON responses",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)

	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	require.NoError(t, err)
	homeAsset := runAssets[0]

	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "item_inspect_js",
			Title:     "Inspect JS Bundle",
			Objective: "Extract endpoints from JS and JSON",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	require.NoError(t, err)

	wi, _, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)

	// 1. Test pure JavaScript response
	jsContent := `
		export const api = {
			getProfile: () => axios.get('/api/v2/profile'),
			updateProfile: (data) => axios.put('/api/v2/profile', data),
			deleteAccount: () => axios.delete('/api/v2/account'),
			fetchStats: () => fetch('/api/v2/stats')
		};
	`

	jsToolCall, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_js_bundle", "get_endpoint", json.RawMessage(fmt.Sprintf(`{"asset_id":"%s"}`, homeAsset.ID)))
	require.NoError(t, err)

	respData := map[string]interface{}{
		"request": map[string]interface{}{
			"url":      origin + "/assets/bundle.js",
			"asset_id": homeAsset.ID.String(),
		},
		"response": map[string]interface{}{
			"status_code": 200,
			"headers": map[string]string{
				"Content-Type": "application/javascript; charset=utf-8",
			},
			"body": jsContent,
		},
	}
	respBytes, _ := json.Marshal(respData)
	now := time.Now()
	err = env.runService.UpdateAgentToolCall(ctx, jsToolCall.ID, "success", string(respBytes), nil, &now, &now)
	require.NoError(t, err)

	extractTool := tools.NewExtractAPIEndpointsTool(run.ID, wi.ID, sess.ID, env.runService)
	outputStr, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, jsToolCall.ID)))
	require.NoError(t, err)

	var result struct {
		Discovered []struct {
			Path string `json:"path"`
		} `json:"discovered"`
	}
	err = json.Unmarshal([]byte(outputStr), &result)
	require.NoError(t, err)

	pathsFound := make(map[string]bool)
	for _, d := range result.Discovered {
		pathsFound[d.Path] = true
	}
	require.True(t, pathsFound["/api/v2/profile"])
	require.True(t, pathsFound["/api/v2/account"])
	require.True(t, pathsFound["/api/v2/stats"])

	// 2. Test OpenAPI JSON response
	jsonContent := `{
		"openapi": "3.0.0",
		"paths": {
			"/api/v3/orders": {
				"get": {},
				"post": {}
			},
			"/api/v3/inventory": {
				"get": {}
			}
		}
	}`

	jsonToolCall, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_openapi_json", "get_endpoint", json.RawMessage(fmt.Sprintf(`{"asset_id":"%s"}`, homeAsset.ID)))
	require.NoError(t, err)

	respDataJSON := map[string]interface{}{
		"request": map[string]interface{}{
			"url":      origin + "/openapi.json",
			"asset_id": homeAsset.ID.String(),
		},
		"response": map[string]interface{}{
			"status_code": 200,
			"headers": map[string]string{
				"Content-Type": "application/json",
			},
			"body": jsonContent,
		},
	}
	respBytesJSON, _ := json.Marshal(respDataJSON)
	err = env.runService.UpdateAgentToolCall(ctx, jsonToolCall.ID, "success", string(respBytesJSON), nil, &now, &now)
	require.NoError(t, err)

	outputJSONStr, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, jsonToolCall.ID)))
	require.NoError(t, err)

	var resultJSON struct {
		Discovered []struct {
			Path string `json:"path"`
		} `json:"discovered"`
	}
	err = json.Unmarshal([]byte(outputJSONStr), &resultJSON)
	require.NoError(t, err)

	jsonPathsFound := make(map[string]bool)
	for _, d := range resultJSON.Discovered {
		jsonPathsFound[d.Path] = true
	}
	require.True(t, jsonPathsFound["/api/v3/orders"])
	require.True(t, jsonPathsFound["/api/v3/inventory"])
}

func TestExtractAPIEndpoints_PolicyAndProfiles(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()
	origin := "http://127.0.0.1:29090"

	// 1. Verify capability profiles contain extract_api_endpoints
	profiles := model.DefaultProfiles()
	profileMap := make(map[string]model.CapabilityProfile)
	for _, p := range profiles {
		profileMap[p.Name] = p
	}

	exploreProfile, ok := profileMap["explore"]
	require.True(t, ok, "explore profile must exist")
	var hasExtractInExplore bool
	for _, tool := range exploreProfile.Tools {
		if tool.Name == "extract_api_endpoints" {
			hasExtractInExplore = true
			break
		}
	}
	require.True(t, hasExtractInExplore, "explore profile must contain extract_api_endpoints")

	apiReconProfile, ok := profileMap["api_recon"]
	require.True(t, ok, "api_recon profile must exist")
	var hasExtractInApiRecon bool
	for _, tool := range apiReconProfile.Tools {
		if tool.Name == "extract_api_endpoints" {
			hasExtractInApiRecon = true
			break
		}
	}
	require.True(t, hasExtractInApiRecon, "api_recon profile must contain extract_api_endpoints")

	// 2. Test exact_paths policy boundary enforcement
	scopeJSON := fmt.Sprintf(`{
		"mode": "discovery",
		"seeds": ["/"],
		"discovery_policy": {
			"mode": "exact_paths",
			"origin": "%s",
			"allowed_methods": ["GET", "POST"],
			"allowed_paths": ["/", "/api/allowed"]
		}
	}`, origin)

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "API Policy Enforcement Task",
		Objective: "Test policy rejection for out-of-scope API routes",
		Scope:     json.RawMessage(scopeJSON),
		Config:    json.RawMessage(`{}`),
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)

	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	require.NoError(t, err)
	homeAsset := runAssets[0]

	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "item_policy_test",
			Title:     "Test Policy Enforcement",
			Objective: "Inspect policy boundaries",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	require.NoError(t, err)

	wi, _, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)

	// HTML contains an allowed endpoint, an out-of-scope endpoint, and an external domain endpoint
	htmlContent := `
		<html><body>
			<script>
				axios.get('/api/allowed');
				axios.post('/api/forbidden');
				axios.get('http://attacker.com/api/steal');
			</script>
		</body></html>
	`

	getToolCall, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, "call_boundary", "get_endpoint", json.RawMessage(fmt.Sprintf(`{"asset_id":"%s"}`, homeAsset.ID)))
	require.NoError(t, err)

	respData := map[string]interface{}{
		"request": map[string]interface{}{
			"url":      origin + "/",
			"asset_id": homeAsset.ID.String(),
		},
		"response": map[string]interface{}{
			"status_code": 200,
			"headers":     map[string]string{"Content-Type": "text/html"},
			"body":        htmlContent,
		},
	}
	respBytes, _ := json.Marshal(respData)
	now := time.Now()
	err = env.runService.UpdateAgentToolCall(ctx, getToolCall.ID, "success", string(respBytes), nil, &now, &now)
	require.NoError(t, err)

	extractTool := tools.NewExtractAPIEndpointsTool(run.ID, wi.ID, sess.ID, env.runService)
	outputStr, err := extractTool.Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, getToolCall.ID)))
	require.NoError(t, err)

	var result struct {
		Discovered []struct {
			Path string `json:"path"`
		} `json:"discovered"`
		Rejected []struct {
			RawHref string `json:"raw_href"`
			Reason  string `json:"reason"`
		} `json:"rejected"`
	}
	err = json.Unmarshal([]byte(outputStr), &result)
	require.NoError(t, err)

	require.Len(t, result.Discovered, 1)
	require.Equal(t, "/api/allowed", result.Discovered[0].Path)

	rejectedReasons := make(map[string]string)
	for _, r := range result.Rejected {
		rejectedReasons[r.RawHref] = r.Reason
	}
	require.Contains(t, rejectedReasons["/api/forbidden"], "outside discovery policy allowed_paths")
	require.Contains(t, rejectedReasons["http://attacker.com/api/steal"], "does not match policy origin")

	// 3. RegisterProfileTools successfully registers extract_api_endpoints into a Registry
	reg := tools.NewRegistry()
	err = tools.RegisterProfileTools(reg, exploreProfile, tools.AdapterContext{
		RunID:      run.ID,
		WorkItemID: wi.ID,
		SessionID:  sess.ID,
		Service:    env.runService,
	})
	require.NoError(t, err)
	registeredTool, exists := reg.Get("extract_api_endpoints")
	require.True(t, exists)
	require.Equal(t, "extract_api_endpoints", registeredTool.Name())
}
