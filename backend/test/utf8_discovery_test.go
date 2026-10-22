package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestDiscovery_ChineseResponsePersistsAndExtractsLinks(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	body := "<html><body>" + strings.Repeat("a", 498) + strings.Repeat("中文新闻", 40) +
		`<a href="/news/details">详情</a></body></html>`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	scope, err := json.Marshal(map[string]any{
		"mode": "discovery", "seeds": []string{server.URL + "/news"},
		"discovery_policy": map[string]any{
			"origin": server.URL, "mode": "same_origin_html", "allowed_methods": []string{"GET"},
			"max_pages": 20, "max_depth": 2,
		},
	})
	require.NoError(t, err)
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Chinese page discovery", Objective: "Read Chinese HTML and extract its links", Scope: scope})
	require.NoError(t, err)
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)
	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{
		Key: "read_chinese", Title: "Read Chinese page", WorkType: "inspect_http_endpoint",
		AssetIDs: []uuid.UUID{assets[0].ID}, InputData: map[string]any{"asset_id": assets[0].ID.String()},
	}})
	require.NoError(t, err)
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, wi)
	require.NotNil(t, att)
	require.NotNil(t, sess)
	registry := tools.NewRegistry()
	registry.Register(tools.NewGetEndpointTool(run.ID, wi.ID, env.runService))
	registry.Register(tools.NewExtractEndpointLinksTool(run.ID, wi.ID, sess.ID, env.runService))
	turn := 0
	fake := &testFakeProvider{callFunc: func(context.Context, []provider.ChatMessage, []provider.ToolDefinition, provider.ModelOpts) (*provider.ModelResponse, error) {
		turn++
		switch turn {
		case 1:
			return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "read_chinese", Function: provider.FunctionCall{Name: "get_endpoint", Arguments: `{"asset_id":"` + assets[0].ID.String() + `"}`}}}}, nil
		case 2:
			return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "extract_chinese", Function: provider.FunctionCall{Name: "extract_endpoint_links", Arguments: `{"source_tool_call_id":"read_chinese"}`}}}}, nil
		default:
			return &provider.ModelResponse{FinishReason: "stop", Content: "Read and extracted links"}, nil
		}
	}}
	runner := agent.NewSessionRunner(sess.ID, run.ID, "worker", env.runService, fake, registry, model.DefaultBudgetConfig())
	_, err = runner.Run(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: "Read the seed and extract its links"}})
	require.NoError(t, err)
	var successfulCalls int
	err = env.pool.QueryRow(ctx, `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND status='success'`, run.ID).Scan(&successfulCalls)
	require.NoError(t, err)
	require.Equal(t, 2, successfulCalls)
	discoveries, err := env.runService.ListDiscoveriesByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, discoveries, 1)
	require.Equal(t, "/news/details", discoveries[0].RawHref)
	require.NotEqual(t, uuid.Nil, discoveries[0].SourceToolCallID)
	require.NoError(t, env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, true, nil, "completed"))
}
