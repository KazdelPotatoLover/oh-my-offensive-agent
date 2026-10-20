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
)

// All HTTP responses are persisted fixtures: this test makes no network requests.
func TestDiscoveryGraph_CyclesMultipleSeedsAndShorterPaths(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	const origin = "http://127.0.0.1:29999"
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "graph contract", Objective: "Test graph distances", Scope: json.RawMessage(`{"mode":"discovery","seeds":["http://127.0.0.1:29999/","http://127.0.0.1:29999/other"],"discovery_policy":{"mode":"same_origin_html","origin":"http://127.0.0.1:29999","max_depth":3,"max_pages":10,"allowed_methods":["GET"]}}`)})
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
	if _, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "fixture", Title: "Fixture", Objective: "Store graph evidence", WorkType: "aggregate_summary"}}); err != nil {
		t.Fatal(err)
	}
	_, _, session, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || session == nil {
		t.Fatal(err)
	}
	seq := 0
	extract := func(path, html string) {
		t.Helper()
		seq++
		var asset uuid.UUID
		if err := env.pool.QueryRow(ctx, `SELECT a.id FROM assets a JOIN run_assets ra ON ra.asset_id=a.id WHERE ra.run_id=$1 AND a.external_key=$2`, run.ID, "GET "+origin+path).Scan(&asset); err != nil {
			t.Fatal(err)
		}
		call, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &session.ID, nil, fmt.Sprintf("fixture_%d", seq), "get_endpoint", json.RawMessage(`{}`))
		if err != nil {
			t.Fatal(err)
		}
		evidence, _ := json.Marshal(map[string]any{"request": map[string]any{"url": origin + path, "asset_id": asset}, "response": map[string]any{"status_code": 200, "headers": map[string]string{"Content-Type": "text/html"}, "body": html}})
		if err := env.runService.UpdateAgentToolCall(ctx, call.ID, "success", string(evidence), nil, nil, nil); err != nil {
			t.Fatal(err)
		}
		bounded, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		_, err = tools.NewExtractEndpointLinksTool(run.ID, uuid.Nil, session.ID, env.runService).Execute(bounded, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, call.ID)))
		if err != nil {
			t.Fatalf("extract %s: %v", path, err)
		}
	}
	depth := func(path string, want int) {
		t.Helper()
		var actual int
		if err := env.pool.QueryRow(ctx, `SELECT ra.depth FROM run_assets ra JOIN assets a ON a.id=ra.asset_id WHERE ra.run_id=$1 AND a.external_key=$2`, run.ID, "GET "+origin+path).Scan(&actual); err != nil {
			t.Fatal(err)
		}
		if actual != want {
			t.Fatalf("%s depth=%d want=%d", path, actual, want)
		}
	}
	extract("/", `<a href="/a">A</a><a href="/a">Duplicate</a><a href="/">Self</a>`)
	extract("/a", `<a href="/">Back</a><a href="/b">B</a>`)
	extract("/b", `<a href="/a">Cycle</a><a href="/c">C</a>`)
	depth("/", 0)
	depth("/a", 1)
	depth("/b", 2)
	depth("/c", 3)
	extract("/other", `<a href="/b">Shorter path to B</a>`)
	depth("/other", 0)
	depth("/b", 1)
	depth("/c", 2)
	extract("/c", `<a href="/d">Now within depth limit</a>`)
	depth("/d", 3)
	extract("/d", `<a href="/too-deep">Beyond limit</a>`)
	var count int
	if err = env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_assets WHERE run_id=$1`, run.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 6 {
		t.Fatalf("assets=%d want=6", count)
	}
	if _, err = env.pool.Exec(ctx, `DELETE FROM run_assets WHERE run_id=$1 AND is_seed`, run.ID); err != nil {
		t.Fatal(err)
	}
	// An unreachable source must not be silently treated as another depth-zero seed.
	var callID string
	if err = env.pool.QueryRow(ctx, `SELECT id::text FROM agent_tool_calls WHERE run_id=$1 AND call_id='fixture_3'`, run.ID).Scan(&callID); err != nil {
		t.Fatal(err)
	}
	if _, err = tools.NewExtractEndpointLinksTool(run.ID, uuid.Nil, session.ID, env.runService).Execute(ctx, []byte(fmt.Sprintf(`{"source_tool_call_id":"%s"}`, callID))); err == nil {
		t.Fatal("unreachable source accepted")
	}
}
