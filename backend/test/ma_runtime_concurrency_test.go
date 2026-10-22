package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
)

// TestMARuntime_DiscoveryN4_RequestSnapshotEffectivePeakMatch is the M1 failing
// case for the discovery serial override. It creates a discovery run through the
// HTTP handler with max_parallel_workers=4 and four distinct authorized pages.
// A barrier records real request overlap. Counting completed work items is not
// enough: a serial pool can still finish all four.
func TestMARuntime_DiscoveryN4_RequestSnapshotEffectivePeakMatch(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	const requested = 4
	var current atomic.Int32
	var peak atomic.Int32
	var releaseOnce sync.Once
	release := make(chan struct{})
	note := func() {
		cur := current.Add(1)
		for {
			old := peak.Load()
			if cur <= old || peak.CompareAndSwap(old, cur) {
				break
			}
		}
		if cur >= requested {
			releaseOnce.Do(func() { close(release) })
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<html><body>
<a href="/dir-a">a</a><a href="/dir-b">b</a><a href="/dir-c">c</a><a href="/dir-d">d</a>
</body></html>`)
			return
		}
		note()
		defer current.Add(-1)
		select {
		case <-release:
		case <-time.After(1200 * time.Millisecond):
		case <-r.Context().Done():
		}
		fmt.Fprintf(w, "<html><body>%s</body></html>", r.URL.Path)
	}))
	defer srv.Close()

	ctx := context.Background()
	scope, _ := json.Marshal(map[string]any{
		"mode":  "discovery",
		"seeds": []string{srv.URL + "/"},
		"discovery_policy": map[string]any{
			"mode":                "same_origin_html",
			"origin":              srv.URL,
			"max_pages":           8,
			"max_depth":           1,
			"allowed_methods":     []string{"GET"},
			"request_interval_ms": 1,
		},
	})
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "M1 discovery N=4",
		Objective: "Inspect four distinct authorized directions concurrently",
		Scope:     scope,
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	body := fmt.Sprintf(`{"executor":"orchestrator","provider":"mock","mock_scenario":"discovery","budget":{"max_model_calls":80,"max_tool_calls":80,"run_timeout_seconds":40,"max_parallel_workers":%d,"worker_max_model_calls":8,"worker_max_tool_calls":12,"worker_timeout_seconds":20}}`, requested)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID.String()+"/runs", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	env.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK {
		t.Fatalf("create run status %d body %s", rec.Code, rec.Body.String())
	}
	var created model.Run
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode run: %v body %s", err, rec.Body.String())
	}

	var input struct {
		Budget struct {
			MaxParallelWorkers int `json:"max_parallel_workers"`
		} `json:"budget"`
	}
	if err := json.Unmarshal(created.ExecutionInput, &input); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	snapshot := input.Budget.MaxParallelWorkers

	deadline := time.Now().Add(25 * time.Second)
	var final *model.Run
	for time.Now().Before(deadline) {
		run, err := env.runService.Get(ctx, created.ID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		if run.Status.IsTerminal() {
			final = run
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if final == nil {
		t.Fatalf("discovery run did not finish; peak overlap=%d snapshot=%d", peak.Load(), snapshot)
	}

	peakSeen := int(peak.Load())
	t.Logf("discovery N chain requested=%d snapshot=%d peak=%d status=%s reason=%v", requested, snapshot, peakSeen, final.Status, final.FinishReason)
	if snapshot != requested || peakSeen != requested {
		t.Fatalf("concurrency chain mismatch: requested=%d snapshot=%d peak=%d (handler discovery override forces a serial pool; peak is observed overlap, not completed count)", requested, snapshot, peakSeen)
	}
}

// TestMARuntime_PlannerModelCallMustNotBlockDispatch fails while the planner
// session and the dispatcher share one goroutine. Item B is persisted by
// submit_plan during a planner turn whose following model call stays blocked.
// B must start before that blocked call returns.
func TestMARuntime_PlannerModelCallMustNotBlockDispatch(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "M1 planner must not block dispatcher",
		Objective: "Dispatch B while the planner model call is still in flight",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}
	budget := model.BudgetConfig{
		MaxModelCalls:                  40,
		MaxToolCalls:                   40,
		RunTimeoutSeconds:              30,
		MaxParallelWorkers:             2,
		WorkerMaxModelCalls:            6,
		WorkerMaxToolCalls:             8,
		WorkerTimeoutSeconds:           20,
		WorkerSettlementTimeoutSeconds: 5,
	}
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	plannerHeld := make(chan struct{})
	releasePlanner := make(chan struct{})
	bStarted := make(chan struct{})
	var plannerRound atomic.Int32
	var bStartedBeforeRelease atomic.Bool

	fake := &testFakeProvider{callFunc: func(callCtx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
		has := func(name string) bool {
			for _, tool := range tools {
				if tool.Name == name {
					return true
				}
			}
			return false
		}
		if has("submit_plan") || has("suggest_completion") {
			if len(messages) > 0 && messages[len(messages)-1].Role == provider.RoleTool {
				round := plannerRound.Load()
				if round == 2 {
					select {
					case plannerHeld <- struct{}{}:
					default:
					}
					select {
					case <-releasePlanner:
					case <-callCtx.Done():
						return nil, callCtx.Err()
					}
				}
				return &provider.ModelResponse{Content: "planner turn finished", FinishReason: "stop"}, nil
			}
			round := plannerRound.Add(1)
			if round == 1 {
				args, _ := json.Marshal(map[string]any{
					"summary": "plan A",
					"items": []map[string]any{{
						"key": "intent_a", "title": "A", "objective": "record a fact and wait", "work_type": "intent",
					}},
				})
				return &provider.ModelResponse{
					FinishReason: "tool_calls",
					ToolCalls:    []provider.ToolCall{{ID: "call_plan_a", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(args)}}},
				}, nil
			}
			if round == 2 {
				args, _ := json.Marshal(map[string]any{
					"summary": "plan B from fact",
					"items": []map[string]any{{
						"key": "intent_b", "title": "B", "objective": "start before planner returns", "work_type": "intent",
					}},
				})
				return &provider.ModelResponse{
					FinishReason: "tool_calls",
					ToolCalls:    []provider.ToolCall{{ID: "call_plan_b", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(args)}}},
				}, nil
			}
			args, _ := json.Marshal(map[string]any{"conclusion": "complete", "final_summary": "both directions ran"})
			return &provider.ModelResponse{
				FinishReason: "tool_calls",
				ToolCalls:    []provider.ToolCall{{ID: "call_done", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(args)}}},
			}, nil
		}

		isA, isB := false, false
		for _, msg := range messages {
			if bytes.Contains([]byte(msg.Content), []byte("intent_a")) {
				isA = true
			}
			if bytes.Contains([]byte(msg.Content), []byte("intent_b")) {
				isB = true
			}
		}
		if isB {
			select {
			case <-releasePlanner:
			default:
				bStartedBeforeRelease.Store(true)
			}
			select {
			case bStarted <- struct{}{}:
			default:
			}
			return &provider.ModelResponse{Content: "B finished", FinishReason: "stop"}, nil
		}
		if isA {
			if len(messages) > 0 && messages[len(messages)-1].Role == provider.RoleTool {
				select {
				case <-bStarted:
				case <-callCtx.Done():
					return nil, callCtx.Err()
				}
				return &provider.ModelResponse{Content: "A finished after B started", FinishReason: "stop"}, nil
			}
			args, _ := json.Marshal(map[string]any{
				"summary": "fact from A", "detail": "new direction", "confidence": "observed", "evidence": "status=200",
			})
			return &provider.ModelResponse{
				FinishReason: "tool_calls",
				ToolCalls:    []provider.ToolCall{{ID: "call_fact_a", Function: provider.FunctionCall{Name: "record_fact", Arguments: string(args)}}},
			}, nil
		}
		return &provider.ModelResponse{Content: "done", FinishReason: "stop"}, nil
	}}

	eng := orchestration.NewEngine(run.ID, env.runService, fake, budget, orchestration.WithDebounce(10*time.Millisecond))
	done := make(chan struct{})
	go func() {
		eng.Run(ctx)
		close(done)
	}()

	select {
	case <-plannerHeld:
	case <-time.After(8 * time.Second):
		t.Fatal("planner follow-up model call was not held")
	}
	select {
	case <-bStarted:
		if !bStartedBeforeRelease.Load() {
			t.Fatal("B started only after the held planner call was released")
		}
	case <-time.After(2 * time.Second):
		close(releasePlanner)
		t.Fatal("B did not start while the planner model call was still blocked; dispatcher is stuck behind the planner session")
	}
	close(releasePlanner)
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("engine did not finish after releasing the planner call")
	}
}
