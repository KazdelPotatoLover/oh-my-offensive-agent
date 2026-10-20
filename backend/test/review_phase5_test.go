package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
)

func reviewPrepareHTTPWork(t *testing.T, env *TestEnv, port, path string) (*model.Run, *model.WorkItem, *model.AgentSession, uuid.UUID) {
	t.Helper()
	t.Setenv("FIXTURE_PORT", port)
	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Review HTTP boundary", Objective: "Record actual evidence", Scope: json.RawMessage(`{"endpoints":["http_home"]}`)})
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
	target := "http://127.0.0.1:" + port + path
	metadata, _ := json.Marshal(map[string]any{"method": "GET", "url": target, "path": path, "port": port})
	var asset uuid.UUID
	err = env.pool.QueryRow(ctx, `INSERT INTO assets (namespace,kind,external_key,name,metadata) VALUES ('review-fixture','http_endpoint',$1,'Review endpoint',$2) RETURNING id`, "GET "+target, metadata).Scan(&asset)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := json.Marshal(map[string]any{"kind": "http_endpoint", "external_key": "GET " + target, "metadata": map[string]any{"url": target, "method": "GET"}})
	if _, err = env.pool.Exec(ctx, `INSERT INTO run_assets (run_id,asset_id,snapshot) VALUES ($1,$2,$3)`, run.ID, asset, snapshot); err != nil {
		t.Fatal(err)
	}
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "review_http", Title: "Review HTTP", Objective: "Read endpoint", WorkType: "inspect_http_endpoint", AssetIDs: []uuid.UUID{asset}, InputData: map[string]any{"asset_id": asset.String()}}})
	if err != nil {
		t.Fatal(err)
	}
	work, _, session, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || work == nil || session == nil {
		t.Fatalf("claim: %v", err)
	}
	return run, work, session, asset
}

func TestReview_HTTPIncompleteBodyMustNotSucceed(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("short"))
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	run, work, _, asset := reviewPrepareHTTPWork(t, env, port, "/health")
	out, err := tools.NewGetEndpointTool(run.ID, work.ID, env.runService).Execute(context.Background(), []byte(fmt.Sprintf(`{"asset_id":%q}`, asset.String())))
	if err == nil {
		t.Fatalf("incomplete response body falsely returned success: %s", out)
	}
}

func TestReview_HTTPCancelAfterHeadersMustNotSucceed(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("prefix"))
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(srv.Listener.Addr().String())
	run, work, _, asset := reviewPrepareHTTPWork(t, env, port, "/health")
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	out, err := tools.NewGetEndpointTool(run.ID, work.ID, env.runService).Execute(ctx, []byte(fmt.Sprintf(`{"asset_id":%q}`, asset.String())))
	if err == nil {
		t.Fatalf("cancellation during body read falsely returned success: %s", out)
	}
}

type reviewHTTPProvider struct {
	asset uuid.UUID
	calls int
}

func (p *reviewHTTPProvider) Call(ctx context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	p.calls++
	if p.calls == 1 {
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "review_call", Type: "function", Function: provider.FunctionCall{Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":%q}`, p.asset.String())}}}}, nil
	}
	return &provider.ModelResponse{FinishReason: "stop", Content: "HTTP evidence received"}, nil
}

func TestReview_HTTPRawEvidenceMustSurviveModelOutputBudget(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	srv, cleanup := setupTestFixture(t)
	defer cleanup()
	run, work, session, asset := reviewPrepareHTTPWork(t, env, srv.Port(), "/large")
	tool := tools.NewGetEndpointTool(run.ID, work.ID, env.runService)
	args := []byte(fmt.Sprintf(`{"asset_id":%q}`, asset.String()))
	direct, err := tool.Execute(context.Background(), args)
	if err != nil || !json.Valid([]byte(direct)) {
		t.Fatalf("direct tool setup failed: %v", err)
	}
	reg := tools.NewRegistry()
	reg.Register(tool)
	budget := model.DefaultBudgetConfig()
	runner := agent.NewSessionRunner(session.ID, run.ID, "worker", env.runService, &reviewHTTPProvider{asset: asset}, reg, budget)
	if _, err = runner.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: "Read endpoint and preserve evidence"}}); err != nil {
		t.Fatal(err)
	}
	calls, err := env.runService.ListAgentToolCalls(context.Background(), run.ID)
	if err != nil || len(calls) != 1 {
		t.Fatalf("calls: len=%d err=%v", len(calls), err)
	}
	stored := calls[0].Output
	if !json.Valid([]byte(stored)) {
		t.Fatalf("stored raw evidence became invalid JSON after model budget truncation: direct_bytes=%d stored_bytes=%d budget=%d", len(direct), len(stored), budget.MaxToolOutputBytes)
	}
	var parsed struct {
		Integrity struct {
			SavedBytes  int  `json:"saved_bytes"`
			IsTruncated bool `json:"is_truncated"`
		} `json:"integrity"`
	}
	if err = json.Unmarshal([]byte(stored), &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Integrity.SavedBytes != 65536 || !parsed.Integrity.IsTruncated {
		t.Fatalf("raw evidence was not preserved: %+v", parsed.Integrity)
	}
}
