package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// A URL that was not pre-registered must yield usable evidence on its first
// capture. Changing query values must reuse its endpoint, without a repair GET.
func TestExplorationProgress_NewURLCaptureBindsEvidence(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	var hits atomic.Int32
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("captured endpoint response"))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	policy := fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":%q,"path_mode":"same_origin","allow_private":true}],"max_pages":1,"max_requests":3}}`, u.Port())
	runID, rootAsset, workID := seedHTTPAsset(t, pool, policy, app.URL+"/", "GET "+app.URL+"/")
	var sessionID, attemptID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,status,work_item_id) VALUES($1,'worker','running',$2) RETURNING id`, runID, workID).Scan(&sessionID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO attempts(work_item_id,attempt_number,status,session_id) VALUES($1,1,'running',$2) RETURNING id`, workID, sessionID).Scan(&attemptID))
	_, err := pool.Exec(ctx, `UPDATE work_items SET status='running',current_attempt_id=$2 WHERE id=$1`, workID, attemptID)
	require.NoError(t, err)
	svc := service.NewRunService(pool)
	store := netcap.NewStore(pool)
	transport := netcap.NewTransport(store)
	tool := tools.NewHTTPRequestTool(runID, workID, attemptID, sessionID, svc, transport)
	var endpoint uuid.UUID
	for _, query := range []string{"first", "second"} {
		args := []byte(fmt.Sprintf(`{"url":%q,"method":"GET"}`, app.URL+"/new?q="+query))
		callID := insertToolCall(t, pool, runID, &sessionID, "http_request", args, "", "pending", 1)
		output, err := tool.Execute(ctx, args)
		require.NoError(t, err)
		var result struct {
			Receipt contract.HTTPReceipt `json:"receipt"`
			Bound   bool                 `json:"asset_bound"`
			Warning string               `json:"evidence_warning"`
		}
		require.NoError(t, json.Unmarshal([]byte(output), &result))
		require.True(t, result.Bound, result.Warning)
		require.NotNil(t, result.Receipt.AssetID)
		require.NotEqual(t, rootAsset, *result.Receipt.AssetID)
		if endpoint != uuid.Nil {
			require.Equal(t, endpoint, *result.Receipt.AssetID)
		}
		endpoint = *result.Receipt.AssetID
		_, err = pool.Exec(ctx, `UPDATE agent_tool_calls SET status='success',output=$2 WHERE id=$1`, callID, output)
		require.NoError(t, err)
		_, err = svc.CreateObservationsBatchWithAssets(ctx, runID, attemptID, []service.ObservationInput{{ObsType: "page_observation", Content: "new endpoint captured", SourceToolCallID: callID, AssetID: &endpoint}})
		require.NoError(t, err)
		fact, err := svc.RecordFact(ctx, runID, "首次响应即可登记", "captured endpoint response", "observed", callID.String(), &workID, []uuid.UUID{endpoint})
		require.NoError(t, err)
		require.Equal(t, "verified", fact.State)
		fake := insertToolCall(t, pool, runID, &sessionID, "http_request", args, output, "success", 1)
		_, err = netcap.ReadToolExchange(ctx, pool, runID, fake, 1, true)
		require.ErrorIs(t, err, netcap.ErrNotEmpirical, "copying the receipt cannot mint new evidence")
	}
	require.EqualValues(t, 2, hits.Load(), "no extra request to repair asset binding")
	// A new page is still refused by the gateway, despite automatic registration.
	_, err = tool.Execute(ctx, []byte(fmt.Sprintf(`{"url":%q,"method":"GET"}`, app.URL+"/other")))
	require.ErrorContains(t, err, "page limit")
	require.EqualValues(t, 2, hits.Load())
	// Binding only accepts the current pending worker call, never an arbitrary receipt.
	_, err = pool.Exec(ctx, `UPDATE work_items SET status='completed' WHERE id=$1`, workID)
	require.NoError(t, err)
	require.Error(t, store.BindCapturedEndpoint(ctx, contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RoleWorker, WorkItemID: &workID, SessionID: &sessionID, AttemptID: &attemptID}, &contract.HTTPReceipt{ReceiptID: uuid.New()}))
}

func TestExplorationProgress_OverviewRetainsConclusionsAndBlockers(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	policy := `{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"8080","path_mode":"same_origin","allow_private":true}],"max_pages":1,"max_requests":1}}`
	runID, _, workID := seedHTTPAsset(t, pool, policy, "http://127.0.0.1:8080/", uuid.NewString())
	_, err := pool.Exec(ctx, `UPDATE work_items SET status='completed',output_data='{"summary":"已确认结果，额度阻断复测"}' WHERE id=$1`, workID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO exploration_nodes(run_id,kind,state,work_item_id,payload) VALUES($1,'intent','open',$2,'{"summary":"文件读取验证"}')`, runID, workID)
	require.NoError(t, err)
	// Hide an older direction and ensure it cannot induce another dispatch.
	_, err = pool.Exec(ctx, `INSERT INTO exploration_nodes(run_id,kind,state,payload,delete_reason) VALUES($1,'intent','done','{"summary":"已删除方向"}','removed')`, runID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO http_request_ledger(run_id,epoch_number,origin,state) VALUES($1,1,'http://127.0.0.1:8080','completed'),($1,2,'http://127.0.0.1:8080','completed')`, runID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO http_pages(run_id,epoch_number,origin,page_key) VALUES($1,1,'http://127.0.0.1:8080','GET /')`, runID)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		callID := insertToolCall(t, pool, runID, nil, "http_request", []byte(`{"method":"GET"}`), "", "failed", 1)
		_, err = pool.Exec(ctx, `UPDATE agent_tool_calls SET error_message='scope_denied: origin request limit reached' WHERE id=$1`, callID)
		require.NoError(t, err)
	}
	svc := service.NewRunService(pool)
	graph, err := svc.GraphOverview(ctx, runID)
	require.NoError(t, err)
	done := graph["recent_done_intents"].([]map[string]any)
	require.Len(t, done, 1)
	require.Equal(t, "completed", done[0]["state"], "use WorkItem status, not stale intent state")
	require.Contains(t, done[0]["last_output"], "额度阻断")
	exec := graph["execution_context"].(map[string]any)
	origins := exec["network_limits"].(map[string]any)["origins"].([]map[string]any)
	require.Len(t, origins, 1)
	require.Equal(t, 1, origins[0]["requests_used"], "old/new epochs do not share usage")
	require.Equal(t, true, origins[0]["requests_blocked"])
	require.Equal(t, true, origins[0]["new_pages_blocked"])
	failures := exec["recent_tool_failures"].([]map[string]any)
	require.Len(t, failures, 1)
	require.Equal(t, 3, failures[0]["count"])
	require.True(t, strings.Contains(failures[0]["error"].(string), "request limit"))
}

// Exercise actual dispatch, capture and fresh Planner sessions. The model
// double consumes the supplied conclusion instead of scheduling another fetch.
func TestExplorationProgress_PlannerSeesWorkerConclusion(t *testing.T) {
	requireMA5(t)
	env := setupTestEnv(t)
	defer env.teardown()
	previousTransport := netcap.Shared()
	netcap.Install(netcap.NewTransport(netcap.NewStore(env.pool)))
	t.Cleanup(func() { netcap.Install(previousTransport) })
	var hits, workerTurns atomic.Int32
	var sawWorkerContext, sawConclusion atomic.Bool
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_, _ = w.Write([]byte("a real captured response"))
	}))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	scope, _ := json.Marshal(map[string]any{
		"mode": "discovery", "seeds": []string{app.URL + "/"},
		"network_policy": map[string]any{"template": "readonly", "allowed_methods": []string{"GET"}, "origins": []map[string]any{{"scheme": "http", "host": "127.0.0.1", "port": u.Port(), "path_mode": "same_origin", "allow_private": true}}, "max_pages": 2, "max_requests": 2},
	})
	task, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{Name: "进度上下文回归", Objective: "读取端点并保留已有结果", Scope: scope})
	require.NoError(t, err)
	budget := model.DefaultBudgetConfig()
	budget.MaxModelCalls, budget.MaxToolCalls, budget.RunTimeoutSeconds = 50, 50, 30
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	require.NoError(t, err)
	p := &testFakeProvider{callFunc: func(ctx context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
		has := func(name string) bool {
			for _, d := range defs {
				if d.Name == name {
					return true
				}
			}
			return false
		}
		require.Greater(t, len(msgs), 1)
		require.Contains(t, msgs[1].Content, `"execution_context"`)
		require.Contains(t, msgs[1].Content, `"recent_done_intents"`)
		if has("submit_plan") {
			if msgs[len(msgs)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{FinishReason: "stop", Content: "本轮结束"}, nil
			}
			if !has("suggest_completion") {
				return toolResponse("plan_once", "submit_plan", map[string]any{"items": []map[string]any{{"key": "capture_once", "title": "首次取证", "objective": "读取新路径", "work_type": "intent"}}}), nil
			}
			if strings.Contains(msgs[1].Content, "已完成新端点取证") {
				sawConclusion.Store(true)
				return toolResponse("finish_partial", "suggest_completion", map[string]any{"conclusion": "partial", "final_summary": "已有结果已保留，无需再次采集；未证明漏洞。"}), nil
			}
			return &provider.ModelResponse{FinishReason: "stop", Content: "等待当前方向完成"}, nil
		}
		sawWorkerContext.Store(true)
		if workerTurns.Add(1) == 1 {
			return toolResponse("capture_new", "http_request", map[string]any{"method": "GET", "url": app.URL + "/new"}), nil
		}
		out := lastToolJSON(t, msgs)
		require.Equal(t, true, out["asset_bound"])
		return &provider.ModelResponse{FinishReason: "stop", Content: "已完成新端点取证"}, nil
	}}
	engine := orchestration.NewEngine(run.ID, env.runService, p, budget)
	require.NoError(t, env.manager.StartExecutor(run.ID, engine))
	final := waitForRunTerminal(t, env, run.ID, 10*time.Second)
	require.NotEqual(t, model.RunStatusFailed, final.Status)
	require.True(t, sawWorkerContext.Load())
	require.True(t, sawConclusion.Load(), "new Planner round must see the actual Worker output")
	require.EqualValues(t, 1, hits.Load())
	items, err := env.runService.ListWorkItemsByRun(context.Background(), run.ID)
	require.NoError(t, err)
	require.Len(t, items, 1)
	require.Equal(t, model.WorkItemStatusCompleted, items[0].Status)
}
