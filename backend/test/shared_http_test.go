package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func lastToolJSON(t *testing.T, messages []provider.ChatMessage) map[string]any {
	t.Helper()
	var v map[string]any
	require.NoError(t, json.Unmarshal([]byte(messages[len(messages)-1].Content), &v))
	return v
}

// Both workers run real SessionRunner loops and persist model/tool calls. B has
// no HTTP client in its registry: its content must come from A's gateway capture.
func TestSharedHTTP_TwoWorkersReuseFullCapture(t *testing.T) {
	requireMA5(t)
	env := setupTestEnv(t)
	defer env.teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var requests atomic.Int32
	body := "<html>" + strings.Repeat("中文页面", 800) + `<a href="/beyond-preview">共享末尾证据</a></html>`
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer app.Close()
	scope, _ := json.Marshal(map[string]any{"mode": "discovery", "seeds": []string{app.URL + "/"}, "discovery_policy": map[string]any{"origin": app.URL, "mode": "same_origin_html", "allowed_methods": []string{"GET"}, "max_pages": 20, "max_depth": 2}})
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "shared HTTP", Objective: "Reuse captured material", Scope: scope})
	require.NoError(t, err)
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)
	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	require.NoError(t, err)
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "fetch", Title: "Capture once", WorkType: "intent", Objective: "读取中文页面", AssetIDs: []uuid.UUID{assets[0].ID}},
		{Key: "reuse", Title: "Reuse capture", WorkType: "intent", Objective: "复用已有流量"},
	})
	require.NoError(t, err)
	graph, err := env.runService.GraphOverview(ctx, run.ID)
	require.NoError(t, err)
	graphJSON, _ := json.Marshal(graph)
	require.Contains(t, string(graphJSON), "读取中文页面")
	require.Contains(t, string(graphJSON), "复用已有流量")
	a, aa, as, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	b, ba, bs, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, b)
	regA, regB := tools.NewRegistry(), tools.NewRegistry()
	regA.Register(tools.NewHTTPRequestTool(run.ID, a.ID, aa.ID, as.ID, env.runService, netcap.NewTransport(netcap.NewStore(env.pool))))
	regA.Register(tools.NewExtractEndpointLinksTool(run.ID, a.ID, as.ID, env.runService))
	regA.Register(&isolatedPanicTool{phase: "execute"})
	tools.RegisterArtifactReaders(regB, tools.AdapterContext{RunID: run.ID, WorkItemID: b.ID, AttemptID: ba.ID, SessionID: bs.ID, Service: env.runService})
	ready, reused, panicked := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var receiptID, artifactID, sourceID string
	var aTurn, bTurn int
	var assembled strings.Builder
	aModel := &testFakeProvider{callFunc: func(ctx context.Context, messages []provider.ChatMessage, _ []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
		aTurn++
		switch aTurn {
		case 1:
			return toolResponse("fetch_once", "http_request", map[string]any{"method": "GET", "asset_id": assets[0].ID}), nil
		case 2:
			out := lastToolJSON(t, messages)
			r := out["receipt"].(map[string]any)
			receiptID, artifactID = r["receipt_id"].(string), r["response_artifact_id"].(string)
			require.NotContains(t, r["response_preview"], "共享末尾证据")
			close(ready)
			return toolResponse("links", "extract_endpoint_links", map[string]any{"source_tool_call_id": "fetch_once"}), nil
		case 3:
			return toolResponse("isolated_failure", "panic_execute", map[string]any{}), nil
		default:
			close(panicked)
			select {
			case <-reused:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return &provider.ModelResponse{FinishReason: "stop", Content: "captured once"}, nil
		}
	}}
	bModel := &testFakeProvider{callFunc: func(ctx context.Context, messages []provider.ChatMessage, _ []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
		bTurn++
		if bTurn == 1 {
			select {
			case <-ready:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			select {
			case <-panicked:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			return toolResponse("search", "search_http_traffic", map[string]any{"origin": app.URL, "method": "GET", "status": 200}), nil
		}
		out := lastToolJSON(t, messages)
		if bTurn == 2 {
			entries := out["entries"].([]any)
			require.Len(t, entries, 1)
			e := entries[0].(map[string]any)
			require.Equal(t, receiptID, e["receipt_id"])
			sourceID = e["source_tool_call_id"].(string)
			return toolResponse("get", "get_http_exchange", map[string]any{"receipt_id": e["receipt_id"]}), nil
		}
		var page map[string]any
		if bTurn == 3 {
			require.Equal(t, false, out["new_verification"])
			require.Equal(t, true, out["complete"])
			page = out["parts"].(map[string]any)["response"].(map[string]any)
		} else {
			page = out
		}
		assembled.WriteString(page["content"].(string))
		if page["has_more"] == true {
			return toolResponse(fmt.Sprintf("page_%d", bTurn), "read_artifact", map[string]any{"artifact_id": artifactID, "offset": page["next_offset"], "limit": 4096}), nil
		}
		require.Equal(t, body, assembled.String())
		close(reused)
		return &provider.ModelResponse{FinishReason: "stop", Content: "reused " + receiptID + " " + artifactID + " " + sourceID}, nil
	}}
	results := make(chan error, 2)
	go func() {
		_, err := agent.NewSessionRunner(as.ID, run.ID, "worker", env.runService, aModel, regA, model.DefaultBudgetConfig()).Run(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: "Capture once"}})
		results <- err
	}()
	go func() {
		_, err := agent.NewSessionRunner(bs.ID, run.ID, "worker", env.runService, bModel, regB, model.DefaultBudgetConfig()).Run(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: "Reuse saved traffic"}})
		results <- err
	}()
	require.NoError(t, <-results)
	require.NoError(t, <-results)
	require.EqualValues(t, 1, requests.Load())
	require.NoError(t, env.runService.FinishWorkItem(ctx, a.ID, aa.ID, as.ID, true, nil, "completed"))
	require.NoError(t, env.runService.FinishWorkItem(ctx, b.ID, ba.ID, bs.ID, true, nil, "completed"))
	discoveries, err := env.runService.ListDiscoveriesByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, discoveries, 1)
	require.Equal(t, "/beyond-preview", discoveries[0].RawHref)
	coverage, err := env.runService.ListAssetCoverage(ctx, run.ID)
	require.NoError(t, err)
	var visits int
	for _, c := range coverage {
		if c.Visited {
			visits++
		}
	}
	require.Equal(t, 1, visits)
	var historyCall uuid.UUID
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT id FROM agent_tool_calls WHERE session_id=$1 AND tool_name='get_http_exchange' AND status='success'`, bs.ID).Scan(&historyCall))
	derived, err := env.runService.RecordFact(ctx, run.ID, "history interpretation", "shared page", "observed", historyCall.String(), &b.ID, nil)
	require.NoError(t, err)
	require.NotEqual(t, "verified", derived.State, "reading traffic is not fresh empirical verification")
	t.Logf("run=%s target=GET %s/ worker_A=%s worker_B=%s receipt=%s artifact=%s original_tool_call=%s worker_B_read_call=%s target_requests=%d reused_bytes=%d tail=%q", run.ID, app.URL, as.ID, bs.ID, receiptID, artifactID, sourceID, historyCall, requests.Load(), assembled.Len(), "共享末尾证据")
}

func TestSharedHTTP_TrustFiltersHistoryAndRedaction(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	var requests atomic.Int32
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Set-Cookie", "session=private-cookie")
		_, _ = w.Write([]byte(`{"password":"private-password","ok":"中文"}`))
	}))
	defer app.Close()
	policy := fmt.Sprintf(`{"discovery_policy":{"origin":%q,"mode":"same_origin_html","request_interval_ms":1}}`, app.URL)
	runID, assetID, workID := seedHTTPAsset(t, pool, policy, app.URL+"/", "GET "+app.URL+"/")
	var sessionID, attemptID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,status,work_item_id) VALUES($1,'worker','running',$2) RETURNING id`, runID, workID).Scan(&sessionID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO attempts(work_item_id,attempt_number,status,session_id) VALUES($1,1,'running',$2) RETURNING id`, workID, sessionID).Scan(&attemptID))
	svc := service.NewRunService(pool)
	tool := tools.NewHTTPRequestTool(runID, workID, attemptID, sessionID, svc, netcap.NewTransport(netcap.NewStore(pool)))
	args := []byte(fmt.Sprintf(`{"method":"GET","url":%q,"query":{"token":"private-token"}}`, app.URL+"/"))
	var receipt contract.HTTPReceipt
	var firstCall uuid.UUID
	for i := 0; i < 3; i++ {
		call := insertToolCall(t, pool, runID, &sessionID, "http_request", args, "", "pending", 1)
		out, err := tool.Execute(ctx, args)
		require.NoError(t, err)
		_, err = pool.Exec(ctx, `UPDATE agent_tool_calls SET status='success',output=$2 WHERE id=$1`, call, out)
		require.NoError(t, err)
		if i == 0 {
			var v struct {
				Receipt contract.HTTPReceipt `json:"receipt"`
			}
			require.NoError(t, json.Unmarshal([]byte(out), &v))
			receipt, firstCall = v.Receipt, call
		}
	}
	coverage, err := svc.ListAssetCoverage(ctx, runID)
	require.NoError(t, err)
	require.Len(t, coverage, 1)
	require.True(t, coverage[0].Visited, "three real visits deduplicate to one asset")
	page, err := svc.SearchHTTPTraffic(ctx, runID, service.TrafficFilter{Limit: 1, Host: "127.0.0.1", Method: "GET", Status: 200})
	require.NoError(t, err)
	entries := page["entries"].([]service.TrafficEntry)
	require.Len(t, entries, 1)
	require.True(t, page["has_more"].(bool))
	page2, err := svc.SearchHTTPTraffic(ctx, runID, service.TrafficFilter{Limit: 1, After: page["next_cursor"].(string)})
	require.NoError(t, err)
	require.NotEqual(t, entries[0].ReceiptID, page2["entries"].([]service.TrafficEntry)[0].ReceiptID)
	read, err := svc.ReadHTTPExchange(ctx, runID, receipt.ReceiptID)
	require.NoError(t, err)
	raw, _ := json.Marshal(read)
	for _, secret := range []string{"private-token", "private-password", "private-cookie"} {
		require.NotContains(t, string(raw), secret)
	}
	require.Contains(t, string(raw), "redacted")
	require.EqualValues(t, 3, requests.Load())
	_, err = svc.SearchHTTPTraffic(ctx, runID, service.TrafficFilter{After: uuid.NewString()})
	require.Error(t, err)
	_, err = svc.SearchHTTPTraffic(ctx, runID, service.TrafficFilter{Limit: 21})
	require.Error(t, err)
	_, err = svc.ReadHTTPExchange(ctx, uuid.New(), receipt.ReceiptID)
	require.Error(t, err)
	require.Error(t, svc.AuthorizeTrafficReader(ctx, uuid.New(), sessionID, workID))
	require.Error(t, svc.AuthorizeTrafficReader(ctx, runID, sessionID, uuid.New()))
	copyOutput, _ := json.Marshal(map[string]any{"receipt": receipt})
	copied := insertToolCall(t, pool, runID, &sessionID, "http_request", args, string(copyOutput), "success", 1)
	_, err = svc.HTTPDiscoverySource(ctx, runID, sessionID, workID, copied.String())
	require.Error(t, err)
	_, err = netcap.ReadToolExchange(ctx, pool, runID, copied, 1, false)
	require.ErrorIs(t, err, netcap.ErrNotEmpirical)
	_, err = svc.HTTPDiscoverySource(ctx, runID, uuid.New(), workID, firstCall.String())
	require.Error(t, err)
	_, err = svc.HTTPDiscoverySource(ctx, uuid.New(), sessionID, workID, firstCall.String())
	require.Error(t, err)
	otherRun, _, _ := seedHTTPAsset(t, pool, policy, app.URL+"/other", uuid.NewString())
	insertToolCall(t, pool, otherRun, nil, "http_request", args, string(copyOutput), "success", 1)
	otherCoverage, err := svc.ListAssetCoverage(ctx, otherRun)
	require.NoError(t, err)
	require.False(t, otherCoverage[0].Visited, "copied receipt/arguments cannot prove a visit")
	_, err = pool.Exec(ctx, `DELETE FROM work_item_assets WHERE work_item_id=$1 AND asset_id=$2`, workID, assetID)
	require.NoError(t, err)
	_, err = svc.HTTPDiscoverySource(ctx, runID, sessionID, workID, firstCall.String())
	require.Error(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO work_item_assets(work_item_id,asset_id) VALUES($1,$2)`, workID, assetID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE http_capture_artifacts SET truncated=true WHERE id=$1`, receipt.ResponseArtifactID)
	require.NoError(t, err)
	_, err = svc.HTTPDiscoverySource(ctx, runID, sessionID, workID, firstCall.String())
	require.Error(t, err)
	_, err = netcap.ReadToolExchange(ctx, pool, runID, firstCall, 1, false)
	require.NoError(t, err, "partial body still proves an actual visit")
	read, err = svc.ReadHTTPExchange(ctx, runID, receipt.ReceiptID)
	require.NoError(t, err)
	require.Equal(t, false, read["complete"])
	_, err = pool.Exec(ctx, `UPDATE runs SET current_epoch=2 WHERE id=$1`, runID)
	require.NoError(t, err)
	_, err = svc.ReadHTTPExchange(ctx, runID, receipt.ReceiptID)
	require.NoError(t, err, "authorized old epochs remain readable")
	require.ErrorIs(t, svc.AuthorizeTrafficReader(ctx, runID, sessionID, workID), service.ErrStaleEpoch)
	for _, role := range []string{"planner", "mainagent"} {
		var reader uuid.UUID
		require.NoError(t, pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,status,epoch_number) VALUES($1,$2,'running',2) RETURNING id`, runID, role).Scan(&reader))
		reg := tools.NewRegistry()
		tools.RegisterArtifactReaders(reg, tools.AdapterContext{RunID: runID, SessionID: reader, Service: svc})
		search, err := reg.Validate("search_http_traffic", []byte(`{"epoch":1}`))
		require.NoError(t, err)
		_, err = search.Execute(ctx, []byte(`{"epoch":1}`))
		require.NoError(t, err)
	}
	_, err = svc.HTTPDiscoverySource(ctx, runID, sessionID, workID, firstCall.String())
	require.ErrorIs(t, err, service.ErrStaleEpoch)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = svc.SearchHTTPTraffic(cancelled, runID, service.TrafficFilter{})
	require.True(t, errors.Is(err, context.Canceled), "%v", err)
	_, err = svc.ReadHTTPExchange(cancelled, runID, receipt.ReceiptID)
	require.True(t, errors.Is(err, context.Canceled), "%v", err)
	_, err = pool.Exec(ctx, `UPDATE http_capture_artifacts SET content='tampered'::bytea WHERE id=$1`, receipt.ResponseArtifactID)
	require.NoError(t, err)
	_, err = svc.ReadArtifact(ctx, runID, *receipt.ResponseArtifactID, 0, 4096)
	require.Error(t, err)
	require.EqualValues(t, 3, requests.Load(), "all traffic reads remain offline")
}
