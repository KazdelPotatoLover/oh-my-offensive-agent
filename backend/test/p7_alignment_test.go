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
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// CustomFuzzerTool simulates a dynamically added domain scanner tool
type CustomFuzzerTool struct{}

func (t *CustomFuzzerTool) Name() string {
	return "custom_fuzzer"
}

func (t *CustomFuzzerTool) Description() string {
	return "Simulates custom fuzzer tool testing specialized attack vectors."
}

func (t *CustomFuzzerTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"target_path": {"type": "string"}
		},
		"required": ["target_path"]
	}`)
}

func (t *CustomFuzzerTool) Validate(args []byte) error {
	var m map[string]any
	return json.Unmarshal(args, &m)
}

func (t *CustomFuzzerTool) Execute(ctx context.Context, args []byte) (string, error) {
	var m struct {
		TargetPath string `json:"target_path"`
	}
	_ = json.Unmarshal(args, &m)
	return fmt.Sprintf(`{"fuzz_status": "anomaly_detected", "path": "%s", "code": 500, "flag": "VULN-FUZZ-OK"}`, m.TargetPath), nil
}

// BloatedTool returns large text to test truncation
type BloatedTool struct{}

func (t *BloatedTool) Name() string        { return "bloated_tool" }
func (t *BloatedTool) Description() string { return "Returns huge output to test truncation." }
func (t *BloatedTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type": "object"}`)
}
func (t *BloatedTool) Validate([]byte) error { return nil }
func (t *BloatedTool) Execute(ctx context.Context, args []byte) (string, error) {
	return strings.Repeat("A_VERY_LONG_EVIDENCE_PAYLOAD_", 200), nil // ~6KB
}

func TestP7_DynamicToolAdapterExtension(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P7 Dynamic Tool Extension Task",
		Objective: "Test dynamic tool registration and execution in worker session",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	require.NoError(t, err)

	// 1. Register custom tool dynamically into a new Registry
	registry := tools.NewRegistry()
	customTool := &CustomFuzzerTool{}
	registry.Register(customTool)

	// Verify definitions includes custom_fuzzer
	defs := registry.Definitions()
	var foundCustom bool
	for _, d := range defs {
		if d.Name == "custom_fuzzer" {
			foundCustom = true
			break
		}
	}
	require.True(t, foundCustom, "custom_fuzzer must be discovered in tool definitions")

	// 2. Setup mock LLM provider
	called := false
	fakeP := &testFakeProvider{
		callFunc: func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
			if !called {
				called = true
				return &provider.ModelResponse{
					Content: "",
					ToolCalls: []provider.ToolCall{
						{
							ID:   "call_fuzz_1",
							Type: "function",
							Function: provider.FunctionCall{
								Name:      "custom_fuzzer",
								Arguments: `{"target_path": "/api/v2/debug"}`,
							},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			}
			return &provider.ModelResponse{
				Content:      "Fuzzing complete: anomaly detected on /api/v2/debug with code 500.",
				FinishReason: "stop",
			}, nil
		},
	}

	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "wi-custom-fuzz",
			Title:     "Run Custom Fuzzing",
			Objective: "Test debug path vulnerability",
			WorkType:  "intent",
		},
	})
	require.NoError(t, err)
	require.Len(t, items, 1)

	claimedWI, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, claimedWI)

	budgetCfg := model.BudgetConfig{
		MaxModelCalls:       10,
		MaxToolCalls:        10,
		WorkerMaxModelCalls: 5,
		WorkerMaxToolCalls:  5,
		ToolTimeoutSeconds:  5,
		MaxToolOutputBytes:  2048,
	}

	runner := agent.NewSessionRunner(
		sess.ID, run.ID, "worker", env.runService, fakeP, registry, budgetCfg,
	)

	initialMsgs := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "You are worker with custom tools."},
		{Role: provider.RoleUser, Content: "Please fuzz target."},
	}

	res, err := runner.Run(ctx, initialMsgs)
	require.NoError(t, err)
	require.Equal(t, "completed", res.FinishReason)

	// Verify tool call was persisted into DB with correct name and completion
	var tcName, tcStatus, tcOutput string
	err = env.pool.QueryRow(ctx, `
		SELECT tool_name, status, output
		FROM agent_tool_calls
		WHERE session_id = $1 AND tool_name = 'custom_fuzzer'`, sess.ID).Scan(&tcName, &tcStatus, &tcOutput)
	require.NoError(t, err)
	require.Equal(t, "custom_fuzzer", tcName)
	require.Equal(t, "success", tcStatus)
	require.Contains(t, tcOutput, "VULN-FUZZ-OK")

	_ = att
}

func TestP7_ToolOutputTruncationEnforcement(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P7 Truncation Task",
		Objective: "Test output truncation enforcement",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	require.NoError(t, err)

	registry := tools.NewRegistry()
	registry.Register(&BloatedTool{})

	called := false
	fakeP := &testFakeProvider{
		callFunc: func(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
			if !called {
				called = true
				return &provider.ModelResponse{
					Content: "",
					ToolCalls: []provider.ToolCall{
						{
							ID:   "call_bloat_1",
							Type: "function",
							Function: provider.FunctionCall{
								Name:      "bloated_tool",
								Arguments: `{}`,
							},
						},
					},
					FinishReason: "tool_calls",
				}, nil
			}
			return &provider.ModelResponse{
				Content:      "Bloated tool call completed and handled safely.",
				FinishReason: "stop",
			}, nil
		},
	}

	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "wi-bloat",
			Title:     "Run Bloated Tool",
			Objective: "Test output byte limit",
			WorkType:  "intent",
		},
	})
	require.NoError(t, err)
	require.Len(t, items, 1)

	claimedWI, _, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, claimedWI)

	// Set tiny output budget: 128 bytes
	budgetCfg := model.BudgetConfig{
		MaxModelCalls:       5,
		MaxToolCalls:        5,
		WorkerMaxModelCalls: 3,
		WorkerMaxToolCalls:  3,
		ToolTimeoutSeconds:  5,
		MaxToolOutputBytes:  128,
	}

	runner := agent.NewSessionRunner(
		sess.ID, run.ID, "worker", env.runService, fakeP, registry, budgetCfg,
	)

	initialMsgs := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: "You are worker."},
		{Role: provider.RoleUser, Content: "Execute bloated tool."},
	}

	res, err := runner.Run(ctx, initialMsgs)
	require.NoError(t, err)
	require.Equal(t, "completed", res.FinishReason)

	// Verify tool response message sent back to model was strictly truncated to budget limit
	var msgContent string
	err = env.pool.QueryRow(ctx, `
		SELECT content
		FROM agent_messages
		WHERE session_id = $1 AND role = 'tool'`, sess.ID).Scan(&msgContent)
	require.NoError(t, err)
	require.Contains(t, msgContent, "truncated: output exceeded model context budget limit")
}

func TestP7_EndToEndAlignmentLifecycle(t *testing.T) {
	env := setupTestEnv(t, false)
	defer env.teardown()
	ctx := context.Background()

	// 1. Create Task & Run with defined goal
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P7 End-to-End Alignment Lifecycle Task",
		Objective: "Demonstrate complete ARTEX alignment flow",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	// Initialize Goal via SetGoals
	goals, err := env.runService.SetGoals(ctx, run.ID, []model.GoalSpec{
		{Text: "Verify admin bypass vulnerability"},
	})
	require.NoError(t, err)
	require.Len(t, goals, 1)
	goalNode := goals[0]

	// 2. Start Run in Epoch 1
	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=$1 WHERE id=$2`, time.Now(), run.ID)
	require.NoError(t, err)

	// Create test asset
	var assetID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO assets (namespace, kind, external_key, name, description)
		VALUES ('http', 'endpoint', 'target-private-ep', 'Private Area', 'http://target.local/private')
		RETURNING id`).Scan(&assetID)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, '{}')`, run.ID, assetID)
	require.NoError(t, err)

	var tcID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'tc-p7', 'get_endpoint', jsonb_build_object('asset_id',$2::text), '{"status_code":200}', 'success')
		RETURNING id`, run.ID, assetID).Scan(&tcID)
	require.NoError(t, err)

	// 3. Worker reports an empirical finding via tool call
	findingArgs, _ := json.Marshal(map[string]any{
		"asset_id":            assetID.String(),
		"title":               "Auth Bypass Exploit Confirmed",
		"category":            "auth_bypass",
		"severity":            "high",
		"source_kind":         "empirical",
		"source_tool_call_id": tcID.String(),
		"description":         "Header bypass successfully tested with exploit payload",
		"remediation":         "Enforce backend auth tokens",
		"trigger_condition":   "GET /private with Auth-Override: 1",
		"reproduce_steps":     "curl -H 'Auth-Override: 1' http://target.local/private",
		"evidence": map[string]any{
			"status": 200,
			"data":   "WELCOME_ADMIN",
		},
	})

	findingTool := tools.NewReportFindingTool(run.ID, env.runService)
	respStr, err := findingTool.Execute(ctx, findingArgs)
	require.NoError(t, err)
	require.Contains(t, respStr, "Auth Bypass Exploit Confirmed")

	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	empiricalFinding := findings[0]
	require.Equal(t, "empirical", empiricalFinding.SourceKind)
	require.Equal(t, "verified", empiricalFinding.VerificationState)

	// 4. Operator Pauses Run
	req := controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/pause", 1, "", "")
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	rObj, err := env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusPaused, rObj.Status)

	// 5. Operator grants budget and advances epoch
	grantBody, _ := json.Marshal(map[string]any{
		"additional_model_calls": 25,
		"additional_tool_calls":  50,
		"additional_seconds":     120,
		"reason":                 "Operator approved phase 2 investigation",
	})
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/grant_budget", 1, "", string(grantBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 6. Operator Resumes Run
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/resume", 1, "", "")
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	rObj, err = env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusRunning, rObj.Status)

	// Control-only contract test; real executor resume is covered separately.
	_, err = env.runService.StartNewEpoch(ctx, run.ID, "Phase 2 deep audit", "")
	require.NoError(t, err)

	rObj, err = env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusRunning, rObj.Status)
	require.Equal(t, 2, rObj.CurrentEpoch)

	// 7. Independent Retest workflow
	retestReqBody, _ := json.Marshal(map[string]string{
		"notes": "Triggering independent verification after developer patch",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+empiricalFinding.ID.String()+"/retests", bytes.NewReader(retestReqBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var retestCreateRes struct {
		Retest  model.FindingRetest `json:"retest"`
		Session model.AgentSession  `json:"session"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &retestCreateRes)
	require.NoError(t, err)
	require.Equal(t, "running", retestCreateRes.Retest.Status)

	// No current target verification: finish inconclusive.
	finishRetestBody, _ := json.Marshal(map[string]string{
		"verdict":  "inconclusive",
		"evidence": "Confirmed 403 Forbidden with payload; vulnerability patched.",
		"summary":  "Anti-bypass filter successfully verified",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+empiricalFinding.ID.String()+"/retests/"+retestCreateRes.Retest.ID.String()+"/finish", bytes.NewReader(finishRetestBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Finding verification_state should now be 'retested'
	refreshedFindings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, refreshedFindings, 1)
	require.Equal(t, "retested", refreshedFindings[0].VerificationState)
	require.Equal(t, model.FindingStatusPendingReview, refreshedFindings[0].Status) // Human review status untouched!

	// 8. Prove Goal with evidence from exploration node
	var findingNodeID uuid.UUID
	err = env.pool.QueryRow(ctx, `SELECT id FROM exploration_nodes WHERE run_id=$1 AND finding_id=$2`, run.ID, empiricalFinding.ID).Scan(&findingNodeID)
	require.NoError(t, err)

	allMet, err := env.runService.ProveGoal(ctx, run.ID, goalNode.ID, findingNodeID, "Confirmed via exploit evidence")
	require.NoError(t, err)
	require.True(t, allMet)

	// Verify goal state in DB
	var gState string
	err = env.pool.QueryRow(ctx, `SELECT state FROM exploration_nodes WHERE id=$1`, goalNode.ID).Scan(&gState)
	require.NoError(t, err)
	require.Equal(t, "met", gState)

	// 9. Planner suggests completion cleanly now that goals are satisfied
	suggestTool := tools.NewSuggestCompletionTool(run.ID, env.runService, nil)
	compArgs := []byte(`{"final_summary": "All goals proven and retested", "conclusion": "complete"}`)
	compOut, err := suggestTool.Execute(ctx, compArgs)
	require.NoError(t, err)
	require.Contains(t, compOut, "complete")

	// Finish Run cleanly
	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='completed', finished_at=$1 WHERE id=$2`, time.Now(), run.ID)
	require.NoError(t, err)

	finalRun, err := env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusCompleted, finalRun.Status)
	require.Equal(t, 2, finalRun.CurrentEpoch)
}
