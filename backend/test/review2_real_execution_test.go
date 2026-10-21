package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

type review2ScriptedEngineProvider struct {
	mu           sync.Mutex
	onCall       func(messages []provider.ChatMessage, tools []provider.ToolDefinition) (*provider.ModelResponse, error)
	receivedMsgs [][]provider.ChatMessage
}

func (p *review2ScriptedEngineProvider) Call(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	p.mu.Lock()
	p.receivedMsgs = append(p.receivedMsgs, messages)
	handler := p.onCall
	p.mu.Unlock()

	if handler != nil {
		return handler(messages, tools)
	}
	return &provider.ModelResponse{Content: "default ok", FinishReason: "stop"}, nil
}

func genToolCallID(prefix string) string {
	return fmt.Sprintf("%s_%s", prefix, uuid.New().String()[:8])
}

// 1. 真实调度器中局部耗尽→Planner 决策继续→第二 Attempt 真实执行并产出结果
func TestReview2_Exhaustion_PlannerContinues_SecondAttemptExecutes(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Exhaustion and Continuation",
		Objective: "Test exhaustion and continuation",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	runReq := model.CreateRunRequest{
		Budget: &model.BudgetConfig{
			MaxModelCalls:      25,
			MaxToolCalls:       25,
			MaxParallelWorkers: 1,
			RunTimeoutSeconds:  15,
		},
	}
	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatal(err)
	}

	var workerAttempts atomic.Int32

	prov := &review2ScriptedEngineProvider{}
	prov.onCall = func(messages []provider.ChatMessage, tDefs []provider.ToolDefinition) (*provider.ModelResponse, error) {
		lastMsg := messages[len(messages)-1]
		firstMsg := messages[0].Content
		isPlanner := strings.Contains(firstMsg, "Lead Security Planner") || strings.Contains(firstMsg, "Lead Security Architect")

		if isPlanner {
			if lastMsg.Role == provider.RoleTool {
				return &provider.ModelResponse{
					Content:      "Acknowledged planner tool result.",
					FinishReason: "stop",
				}, nil
			}

			// Planner fresh turn
			if strings.Contains(lastMsg.Content, "Break down the task") || strings.Contains(lastMsg.Content, "submit a plan") {
				// Round 1: submit plan
				planArgs, _ := json.Marshal(map[string]any{
					"items": []map[string]any{
						{
							"key":        "scan_target",
							"title":      "Scan target endpoint",
							"work_type":  "analyze_sample",
							"objective":  "Gather sample evidence",
							"input_data": map[string]any{"sample_id": "sample_home"},
						},
					},
				})
				return &provider.ModelResponse{
					ToolCalls: []provider.ToolCall{
						{ID: genToolCallID("c_plan"), Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)}},
					},
					FinishReason: "tool_calls",
				}, nil
			}

			// Round 2 or later
			if strings.Contains(lastMsg.Content, "scan_target") && strings.Contains(lastMsg.Content, "exhausted") {
				contArgs, _ := json.Marshal(map[string]any{
					"work_item_key": "scan_target",
					"reason":        "granting second attempt",
				})
				return &provider.ModelResponse{
					ToolCalls: []provider.ToolCall{
						{ID: genToolCallID("c_cont"), Type: "function", Function: provider.FunctionCall{Name: "continue_work_item", Arguments: string(contArgs)}},
					},
					FinishReason: "tool_calls",
				}, nil
			}

			// All work items finished: suggest completion
			compArgs, _ := json.Marshal(map[string]any{
				"final_summary": "Scan completed successfully across attempts",
				"conclusion":    "ok",
			})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_comp"), Type: "function", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		// Worker session
		if lastMsg.Role == provider.RoleTool {
			return &provider.ModelResponse{
				Content:      "Second attempt completed successfully with evidence",
				FinishReason: "stop",
			}, nil
		}

		attNum := workerAttempts.Add(1)
		if attNum == 1 {
			// First attempt: simulate local exhaustion
			return &provider.ModelResponse{
				Content:      "Local exhaustion reached in attempt 1",
				FinishReason: "worker_exhausted",
			}, nil
		}

		// Second attempt: call read_sample tool
		readArgs, _ := json.Marshal(map[string]any{"sample_id": "sample_home"})
		return &provider.ModelResponse{
			ToolCalls: []provider.ToolCall{
				{ID: genToolCallID("c_read"), Type: "function", Function: provider.FunctionCall{Name: "read_sample", Arguments: string(readArgs)}},
			},
			FinishReason: "tool_calls",
		}, nil
	}

	engine := orchestration.NewEngine(run.ID, env.runService, prov, *runReq.Budget)
	if err := env.manager.StartExecutor(run.ID, engine); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	// Wait for run to finish
	var finishedRun *model.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		r, err := env.runService.Get(ctx, run.ID)
		if err == nil && r.Status.IsTerminal() {
			finishedRun = r
			break
		}
	}

	if finishedRun == nil {
		t.Fatal("timed out waiting for engine to finish")
	}
	if finishedRun.Status != model.RunStatusCompleted {
		reasonStr := "nil"
		if finishedRun.FinishReason != nil {
			reasonStr = *finishedRun.FinishReason
		}
		t.Fatalf("expected run completed, got %s (reason: %s)", finishedRun.Status, reasonStr)
	}

	// Verify work item attempts in DB
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil || len(items) != 1 {
		t.Fatalf("expected 1 work item, got %d (%v)", len(items), err)
	}
	wi := items[0]
	if wi.Status != model.WorkItemStatusCompleted {
		t.Fatalf("expected work item completed, got %s", wi.Status)
	}

	attempts, err := env.runService.ListAttemptsByWorkItem(ctx, wi.ID)
	if err != nil || len(attempts) != 2 {
		t.Fatalf("expected exactly 2 attempts, got %d (%v)", len(attempts), err)
	}
	if attempts[0].Status != "exhausted" && attempts[0].Status != "stopped" {
		t.Errorf("expected attempt 1 status exhausted or stopped, got %s", attempts[0].Status)
	}
	if attempts[0].FinishReason == nil || *attempts[0].FinishReason != "worker_exhausted" {
		t.Errorf("expected attempt 1 finish_reason worker_exhausted, got %v", attempts[0].FinishReason)
	}
	if attempts[1].Status != "completed" {
		t.Errorf("expected attempt 2 completed, got status=%s", attempts[1].Status)
	}
}

// 2. 真实调度器中执行中 Pause，验证即使模型较慢，暂停生效后不再执行后续工具；Resume 后继续
func TestReview2_PauseBarrier_BlocksToolsAndTurns(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Pause Barrier",
		Objective: "Test pause barrier across tools and turns",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	runReq := model.CreateRunRequest{
		Budget: &model.BudgetConfig{
			MaxModelCalls:      20,
			MaxToolCalls:       20,
			MaxParallelWorkers: 1,
			RunTimeoutSeconds:  15,
		},
	}
	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatal(err)
	}

	var isPausedState atomic.Bool
	var workerTurn atomic.Int32
	var toolExecutedDuringPause atomic.Bool

	prov := &review2ScriptedEngineProvider{}
	prov.onCall = func(messages []provider.ChatMessage, tDefs []provider.ToolDefinition) (*provider.ModelResponse, error) {
		lastMsg := messages[len(messages)-1]
		firstMsg := messages[0].Content
		isPlanner := strings.Contains(firstMsg, "Lead Security Planner") || strings.Contains(firstMsg, "Lead Security Architect")

		if isPlanner {
			if lastMsg.Role == provider.RoleTool {
				return &provider.ModelResponse{
					Content:      "Planner tool acknowledged.",
					FinishReason: "stop",
				}, nil
			}

			if strings.Contains(lastMsg.Content, "Break down the task") || strings.Contains(lastMsg.Content, "submit a plan") {
				planArgs, _ := json.Marshal(map[string]any{
					"items": []map[string]any{
						{
							"key":        "pause_target",
							"title":      "Worker to test pause",
							"work_type":  "analyze_sample",
							"objective":  "Verify tool blocking",
							"input_data": map[string]any{"sample_id": "sample_home"},
						},
					},
				})
				return &provider.ModelResponse{
					ToolCalls: []provider.ToolCall{
						{ID: genToolCallID("c_plan"), Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)}},
					},
					FinishReason: "tool_calls",
				}, nil
			}

			compArgs, _ := json.Marshal(map[string]any{
				"final_summary": "Pause test completed",
				"conclusion":    "ok",
			})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_comp"), Type: "function", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		if lastMsg.Role == provider.RoleTool {
			if isPausedState.Load() {
				toolExecutedDuringPause.Store(true)
			}
			return &provider.ModelResponse{
				Content:      "Worker finished after resume",
				FinishReason: "stop",
			}, nil
		}

		turn := workerTurn.Add(1)
		if turn == 1 {
			// Trigger pause via API in background right as tool call is returned
			go func() {
				time.Sleep(10 * time.Millisecond)
				pauseReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/pause", run.ID), nil)
				rr := httptest.NewRecorder()
				env.router.ServeHTTP(rr, pauseReq)
				if rr.Code == http.StatusOK {
					isPausedState.Store(true)
				}
			}()

			readArgs, _ := json.Marshal(map[string]any{"sample_id": "sample_home"})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_read"), Type: "function", Function: provider.FunctionCall{Name: "read_sample", Arguments: string(readArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		return &provider.ModelResponse{
			Content:      "Worker concluded",
			FinishReason: "stop",
		}, nil
	}

	engine := orchestration.NewEngine(run.ID, env.runService, prov, *runReq.Budget)
	if err := env.manager.StartExecutor(run.ID, engine); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	// Wait for pause to take effect
	for i := 0; i < 40; i++ {
		if isPausedState.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Verify pause state in DB
	r, err := env.runService.Get(ctx, run.ID)
	if err != nil || r.Status != model.RunStatusPaused {
		t.Fatalf("expected run status paused, got %s (%v)", r.Status, err)
	}

	// Verify while paused, tool execution is blocked
	time.Sleep(200 * time.Millisecond)
	if toolExecutedDuringPause.Load() {
		t.Fatal("tool executed while engine was paused!")
	}

	// Resume run
	isPausedState.Store(false)
	resumeReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/resume", run.ID), nil)
	rrResume := httptest.NewRecorder()
	env.router.ServeHTTP(rrResume, resumeReq)
	if rrResume.Code != http.StatusOK {
		t.Fatalf("resume request failed: %d (%s)", rrResume.Code, rrResume.Body.String())
	}

	// Wait for run completion
	var finished *model.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		r, err := env.runService.Get(ctx, run.ID)
		if err == nil && r.Status.IsTerminal() {
			finished = r
			break
		}
	}
	if finished == nil || finished.Status != model.RunStatusCompleted {
		t.Fatalf("expected run completed after resume, got %v", finished)
	}
}

// 3. 真实并发 Worker（至少 2 个）下 Kill 目标 Worker，验证仅目标退出，其他 Worker 正常完成
func TestReview2_ConcurrentWorkers_KillTargetWorkerOnly(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Concurrent Kill",
		Objective: "Test killing targeted worker among concurrent workers",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home", "sample_login"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	runReq := model.CreateRunRequest{
		Budget: &model.BudgetConfig{
			MaxModelCalls:      30,
			MaxToolCalls:       30,
			MaxParallelWorkers: 2,
			RunTimeoutSeconds:  15,
		},
	}
	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatal(err)
	}

	var workerAStarted, workerBStarted atomic.Bool

	prov := &review2ScriptedEngineProvider{}
	prov.onCall = func(messages []provider.ChatMessage, tDefs []provider.ToolDefinition) (*provider.ModelResponse, error) {
		lastMsg := messages[len(messages)-1]
		firstMsg := messages[0].Content
		isPlanner := strings.Contains(firstMsg, "Lead Security Planner") || strings.Contains(firstMsg, "Lead Security Architect")

		if isPlanner {
			if lastMsg.Role == provider.RoleTool {
				return &provider.ModelResponse{
					Content:      "Planner tool acknowledged.",
					FinishReason: "stop",
				}, nil
			}

			if strings.Contains(lastMsg.Content, "Break down the task") || strings.Contains(lastMsg.Content, "submit a plan") {
				// Plan 2 concurrent work items
				planArgs, _ := json.Marshal(map[string]any{
					"items": []map[string]any{
						{
							"key":        "worker_a",
							"title":      "Worker A to kill",
							"work_type":  "analyze_sample",
							"objective":  "Stay active for kill test",
							"input_data": map[string]any{"sample_id": "sample_home"},
						},
						{
							"key":        "worker_b",
							"title":      "Worker B to survive",
							"work_type":  "analyze_sample",
							"objective":  "Complete normally",
							"input_data": map[string]any{"sample_id": "sample_login"},
						},
					},
				})
				return &provider.ModelResponse{
					ToolCalls: []provider.ToolCall{
						{ID: genToolCallID("c_plan2"), Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)}},
					},
					FinishReason: "tool_calls",
				}, nil
			}

			compArgs, _ := json.Marshal(map[string]any{
				"final_summary": "Concurrent test finished",
				"conclusion":    "ok",
			})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_comp2"), Type: "function", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		fullPrompt := ""
		for _, m := range messages {
			fullPrompt += m.Content + " "
		}

		if strings.Contains(fullPrompt, "worker_a") {
			workerAStarted.Store(true)
			// Worker A stays in a slow loop so test can kill it
			time.Sleep(3 * time.Second)
			return &provider.ModelResponse{Content: "worker A finished", FinishReason: "stop"}, nil
		}

		if strings.Contains(fullPrompt, "worker_b") {
			workerBStarted.Store(true)
			return &provider.ModelResponse{Content: "worker B completed", FinishReason: "stop"}, nil
		}

		return &provider.ModelResponse{Content: "default", FinishReason: "stop"}, nil
	}

	engine := orchestration.NewEngine(run.ID, env.runService, prov, *runReq.Budget)
	if err := env.manager.StartExecutor(run.ID, engine); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	// Wait until Worker A is started
	var itemA *model.WorkItem
	for i := 0; i < 40; i++ {
		if workerAStarted.Load() {
			items, _ := env.runService.ListWorkItemsByRun(ctx, run.ID)
			for _, it := range items {
				if it.Key == "worker_a" {
					found := it
					itemA = &found
					break
				}
			}
			if itemA != nil {
				break
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	if itemA == nil {
		t.Fatal("worker A did not start in time")
	}

	// Kill Worker A via API
	killBody, _ := json.Marshal(map[string]string{"reason": "operator terminated worker a"})
	killReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/work_items/%s/kill", run.ID, itemA.ID), bytes.NewReader(killBody))
	rrKill := httptest.NewRecorder()
	env.router.ServeHTTP(rrKill, killReq)
	if rrKill.Code != http.StatusOK {
		t.Fatalf("kill request failed: %d (%s)", rrKill.Code, rrKill.Body.String())
	}

	// Wait for run wrapup
	var finished *model.Run
	for i := 0; i < 60; i++ {
		time.Sleep(100 * time.Millisecond)
		r, err := env.runService.Get(ctx, run.ID)
		if err == nil && r.Status.IsTerminal() {
			finished = r
			break
		}
	}
	if finished == nil {
		t.Fatal("timed out waiting for run to finish")
	}

	// Verify Worker A was cancelled/killed, but Worker B completed successfully!
	items, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	var statusA, statusB model.WorkItemStatus
	for _, it := range items {
		if it.Key == "worker_a" {
			statusA = it.Status
		}
		if it.Key == "worker_b" {
			statusB = it.Status
		}
	}

	if statusA != model.WorkItemStatusCancelled {
		t.Errorf("expected worker A status cancelled, got %s", statusA)
	}
	if statusB != model.WorkItemStatusCompleted {
		t.Errorf("expected worker B status completed, got %s", statusB)
	}
}

// 4. 真实 Worker 执行中注入 Steer，验证在下一回合 messages 观察到引导内容
func TestReview2_SteerGuidanceInjectedInNextTurn(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Steer Guidance",
		Objective: "Test steer guidance injection across turns",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	runReq := model.CreateRunRequest{
		Budget: &model.BudgetConfig{
			MaxModelCalls:      20,
			MaxToolCalls:       20,
			MaxParallelWorkers: 1,
			RunTimeoutSeconds:  15,
		},
	}
	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatal(err)
	}

	var workerTurn atomic.Int32
	var sawSteerInTurn2 atomic.Bool
	const steerMessage = "Switch focus: analyze header Authorization format strictly"

	prov := &review2ScriptedEngineProvider{}
	prov.onCall = func(messages []provider.ChatMessage, tDefs []provider.ToolDefinition) (*provider.ModelResponse, error) {
		lastMsg := messages[len(messages)-1]
		firstMsg := messages[0].Content
		isPlanner := strings.Contains(firstMsg, "Lead Security Planner") || strings.Contains(firstMsg, "Lead Security Architect")

		if isPlanner {
			if lastMsg.Role == provider.RoleTool {
				return &provider.ModelResponse{
					Content:      "Planner tool acknowledged.",
					FinishReason: "stop",
				}, nil
			}

			if strings.Contains(lastMsg.Content, "Break down the task") || strings.Contains(lastMsg.Content, "submit a plan") {
				planArgs, _ := json.Marshal(map[string]any{
					"items": []map[string]any{
						{
							"key":        "steer_worker",
							"title":      "Worker to steer",
							"work_type":  "analyze_sample",
							"objective":  "Multi turn analysis",
							"input_data": map[string]any{"sample_id": "sample_home"},
						},
					},
				})
				return &provider.ModelResponse{
					ToolCalls: []provider.ToolCall{
						{ID: genToolCallID("c_plan_s"), Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)}},
					},
					FinishReason: "tool_calls",
				}, nil
			}

			compArgs, _ := json.Marshal(map[string]any{
				"final_summary": "Steer test completed",
				"conclusion":    "ok",
			})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_comp_s"), Type: "function", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		// Check if steer guidance has been received in any of the messages
		for _, m := range messages {
			if strings.Contains(m.Content, "[Operator Steer Guidance]") && strings.Contains(m.Content, steerMessage) {
				sawSteerInTurn2.Store(true)
			}
		}

		turn := workerTurn.Add(1)
		if turn == 1 {
			// Trigger tool call and pause in background so steer can be dispatched before turn 2
			go func() {
				time.Sleep(10 * time.Millisecond)
				pauseReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/pause", run.ID), nil)
				rr := httptest.NewRecorder()
				env.router.ServeHTTP(rr, pauseReq)
			}()

			readArgs, _ := json.Marshal(map[string]any{"sample_id": "sample_home"})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_read1"), Type: "function", Function: provider.FunctionCall{Name: "read_sample", Arguments: string(readArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		return &provider.ModelResponse{
			Content:      "Worker finished",
			FinishReason: "stop",
		}, nil
	}

	engine := orchestration.NewEngine(run.ID, env.runService, prov, *runReq.Budget)
	if err := env.manager.StartExecutor(run.ID, engine); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	// Wait for pause to take effect and item to be located
	var wi *model.WorkItem
	for i := 0; i < 40; i++ {
		r, _ := env.runService.Get(ctx, run.ID)
		if r != nil && r.Status == model.RunStatusPaused {
			items, _ := env.runService.ListWorkItemsByRun(ctx, run.ID)
			if len(items) > 0 {
				found := items[0]
				wi = &found
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if wi == nil {
		t.Fatal("worker did not pause in time")
	}

	// Dispatch Steer via API while paused
	steerBody, _ := json.Marshal(map[string]string{"instructions": steerMessage})
	steerReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/work_items/%s/steer", run.ID, wi.ID), bytes.NewReader(steerBody))
	rrSteer := httptest.NewRecorder()
	env.router.ServeHTTP(rrSteer, steerReq)
	if rrSteer.Code != http.StatusOK {
		t.Fatalf("steer API call failed: %d (%s)", rrSteer.Code, rrSteer.Body.String())
	}

	// Resume run so tool executes and turn 2 starts with steer injected
	resumeReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/resume", run.ID), nil)
	rrResume := httptest.NewRecorder()
	env.router.ServeHTTP(rrResume, resumeReq)
	if rrResume.Code != http.StatusOK {
		t.Fatalf("resume failed: %d", rrResume.Code)
	}

	// Wait for run completion
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		r, err := env.runService.Get(ctx, run.ID)
		if err == nil && r.Status.IsTerminal() {
			break
		}
	}

	if !sawSteerInTurn2.Load() {
		t.Fatal("steer guidance was not injected into worker turn 2 messages")
	}
}

// 5. 旧代次 Engine 在 StartNewEpoch 后尝试 Finalize/写入，验证被拒绝且不破坏新代次
func TestReview2_StaleEpochEngineRejectedOnFinalize(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Stale Epoch",
		Objective: "Verify stale epoch isolation",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Verify initial epoch is 1
	rInit, _ := env.runService.Get(ctx, run.ID)
	if rInit.CurrentEpoch != 1 {
		t.Fatalf("expected epoch 1, got %d", rInit.CurrentEpoch)
	}

	// Advance directly to isolate this service-level stale-finalize assertion.
	if _, err := env.runService.StartNewEpoch(ctx, run.ID, "operator rescoping", "starting epoch 2"); err != nil {
		t.Fatal(err)
	}

	// Verify run is now in Epoch 2 and running
	rEpoch2, _ := env.runService.Get(ctx, run.ID)
	if rEpoch2.CurrentEpoch != 2 || rEpoch2.Status != model.RunStatusRunning {
		t.Fatalf("expected epoch 2 running, got epoch=%d status=%s", rEpoch2.CurrentEpoch, rEpoch2.Status)
	}

	// Simulate stale Engine from Epoch 1 trying to finalize run as failed/stopped
	summary := "stale epoch 1 summary"
	finalizeErr := env.runService.FinalizeRunWrapupWithEpoch(ctx, run.ID, model.CauseInternalError, &summary, 1)
	if finalizeErr == nil {
		t.Fatal("expected ErrStaleEpoch when finalizing from epoch 1, but call succeeded!")
	}
	if !strings.Contains(finalizeErr.Error(), "stale execution epoch") {
		t.Fatalf("expected stale epoch error, got %v", finalizeErr)
	}

	// Verify Run is still running in Epoch 2 without corruption
	rAfter, _ := env.runService.Get(ctx, run.ID)
	if rAfter.Status != model.RunStatusRunning || rAfter.CurrentEpoch != 2 {
		t.Fatalf("epoch 2 run was corrupted by stale epoch 1 call: status=%s epoch=%d", rAfter.Status, rAfter.CurrentEpoch)
	}
}

// 6. 活动执行中调用 GrantBudget，验证新配额生效且超时延长，支撑后续工作完成
func TestReview2_GrantBudgetRefreshesActiveEngineAndExtendsDeadline(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Dynamic Budget Grant",
		Objective: "Test dynamic budget grant and deadline extension",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatal(err)
	}

	// Initial budget is intentionally small: 4 model calls, 2 seconds timeout
	initialBudget := model.BudgetConfig{
		MaxModelCalls:      4,
		MaxToolCalls:       5,
		MaxParallelWorkers: 1,
		RunTimeoutSeconds:  2,
	}
	runReq := model.CreateRunRequest{
		Budget: &initialBudget,
	}
	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatal(err)
	}

	prov := &review2ScriptedEngineProvider{}
	prov.onCall = func(messages []provider.ChatMessage, tDefs []provider.ToolDefinition) (*provider.ModelResponse, error) {
		lastMsg := messages[len(messages)-1]
		firstMsg := messages[0].Content
		isPlanner := strings.Contains(firstMsg, "Lead Security Planner") || strings.Contains(firstMsg, "Lead Security Architect")

		if isPlanner {
			if lastMsg.Role == provider.RoleTool {
				return &provider.ModelResponse{
					Content:      "Planner tool acknowledged.",
					FinishReason: "stop",
				}, nil
			}

			if strings.Contains(lastMsg.Content, "Break down the task") || strings.Contains(lastMsg.Content, "submit a plan") {
				planArgs, _ := json.Marshal(map[string]any{
					"items": []map[string]any{
						{
							"key":        "long_worker",
							"title":      "Worker requiring extra budget",
							"work_type":  "analyze_sample",
							"objective":  "multi step",
							"input_data": map[string]any{"sample_id": "sample_home"},
						},
					},
				})
				return &provider.ModelResponse{
					ToolCalls: []provider.ToolCall{
						{ID: genToolCallID("c_plan_b"), Type: "function", Function: provider.FunctionCall{Name: "submit_plan", Arguments: string(planArgs)}},
					},
					FinishReason: "tool_calls",
				}, nil
			}

			compArgs, _ := json.Marshal(map[string]any{
				"final_summary": "Extra budget leveraged",
				"conclusion":    "ok",
			})
			return &provider.ModelResponse{
				ToolCalls: []provider.ToolCall{
					{ID: genToolCallID("c_comp_b"), Type: "function", Function: provider.FunctionCall{Name: "suggest_completion", Arguments: string(compArgs)}},
				},
				FinishReason: "tool_calls",
			}, nil
		}

		// Worker sleeps 1.2s (accumulating past original 2s deadline) and completes
		time.Sleep(1200 * time.Millisecond)
		return &provider.ModelResponse{Content: "worker done", FinishReason: "stop"}, nil
	}

	engine := orchestration.NewEngine(run.ID, env.runService, prov, initialBudget)
	if err := env.manager.StartExecutor(run.ID, engine); err != nil {
		t.Fatalf("start engine: %v", err)
	}

	// After round 1, grant 10 extra model calls and 15 extra seconds via API
	time.Sleep(200 * time.Millisecond)
	grantBody, _ := json.Marshal(map[string]any{
		"additional_model_calls": 10,
		"additional_tool_calls":  10,
		"additional_seconds":     15,
		"reason":                 "operator extended budget for deep analysis",
	})
	grantReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/grant_budget", run.ID), bytes.NewReader(grantBody))
	rrGrant := httptest.NewRecorder()
	env.router.ServeHTTP(rrGrant, grantReq)
	if rrGrant.Code != http.StatusCreated {
		t.Fatalf("grant budget failed: %d (%s)", rrGrant.Code, rrGrant.Body.String())
	}

	// Verify effective budget in API reflects the new limits
	effReq, _ := http.NewRequest(http.MethodGet, fmt.Sprintf("/api/runs/%s/budget/effective", run.ID), nil)
	rrEff := httptest.NewRecorder()
	env.router.ServeHTTP(rrEff, effReq)
	if rrEff.Code != http.StatusOK {
		t.Fatalf("get effective budget failed: %d", rrEff.Code)
	}
	var effBudget model.BudgetConfig
	_ = json.Unmarshal(rrEff.Body.Bytes(), &effBudget)
	if effBudget.MaxModelCalls != 14 || effBudget.RunTimeoutSeconds != 17 {
		t.Fatalf("expected effective budget 14 calls / 17s, got %+v", effBudget)
	}

	// Wait for run completion
	var finished *model.Run
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		r, err := env.runService.Get(ctx, run.ID)
		if err == nil && r.Status.IsTerminal() {
			finished = r
			break
		}
	}
	if finished == nil || finished.Status != model.RunStatusCompleted {
		t.Fatalf("expected run completed with extended budget, got %v", finished)
	}
}

// 7. 通过 API 触发复测，在本地 fixture 观察到真实 HTTP 工具请求，并根据真实工具结果判定 fixed/still_reproducible
func TestReview2_RetestProductionAPIWithRealHTTPFixture(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	// Local HTTP test fixture simulating a patched endpoint
	var fixtureRequests atomic.Int32
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixtureRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status": "ok", "patch": "applied", "vulnerability": "fixed"}`))
	}))
	defer fixture.Close()

	scopeJSON, _ := json.Marshal(map[string]interface{}{
		"mode":  "discovery",
		"seeds": []string{fixture.URL + "/"},
	})
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Review2 Retest Fixture",
		Objective: "Test independent retest execution against real fixture",
		Scope:     scopeJSON,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Create run: seed asset is created automatically in discovery mode
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatal(err)
	}

	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil || len(assets) == 0 {
		t.Fatalf("expected seed asset created: %v", err)
	}
	asset := assets[0]

	// Report an empirical Finding for this asset with a real tool call record
	var tcID uuid.UUID
	tcArgs, _ := json.Marshal(map[string]any{"asset_id": asset.ID.String()})
	err = env.pool.QueryRow(ctx,
		`INSERT INTO agent_tool_calls (run_id, call_id, tool_name, arguments, output, status, epoch_number)
		 VALUES ($1, 'tc_initial_discovery', 'get_endpoint', $2, '{"status_code":200,"body":"private_email_leaked"}', 'success', 1)
		 RETURNING id`, run.ID, tcArgs).Scan(&tcID)
	if err != nil {
		t.Fatal(err)
	}

	finding, err := env.runService.ReportFinding(ctx, run.ID, service.ReportFindingInput{
		AssetID:          asset.ID,
		Title:            "Unauthenticated Information Disclosure",
		Verification:     &service.FindingVerification{Vulnerable: service.HTTPCondition{StatusCode: 200, BodyContains: "private_email_leaked"}, Fixed: service.HTTPCondition{StatusCode: 200, BodyContains: `"vulnerability": "fixed"`}},
		Category:         "auth_bypass",
		Severity:         "high",
		SourceKind:       "empirical",
		SourceToolCallID: &tcID,
	})
	if err != nil {
		t.Fatalf("report finding: %v", err)
	}

	// Record a human review for the finding: review status should be preserved across retest!
	reviewReqBody, _ := json.Marshal(map[string]any{
		"action":           "confirm",
		"notes":            "Human operator confirmed the vulnerability report",
		"status":           "confirmed",
		"reviewer":         "security-operator",
		"comment":          "Confirmed finding on asset",
		"request_id":       uuid.New().String(),
		"expected_version": 0,
	})
	reviewReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(reviewReqBody))
	rrRev := httptest.NewRecorder()
	env.router.ServeHTTP(rrRev, reviewReq)
	if rrRev.Code != http.StatusCreated {
		t.Fatalf("create review failed: %d (%s)", rrRev.Code, rrRev.Body.String())
	}

	// Verification is independent and must still execute after the original scan ended.
	requireRetestParentStopErr := env.runService.FinalizeRunWrapup(ctx, run.ID, model.CauseCompleted, nil)
	if requireRetestParentStopErr != nil {
		t.Fatal(requireRetestParentStopErr)
	}
	// Call POST /api/runs/{runID}/findings/{findingID}/retests via API
	retestReqBody, _ := json.Marshal(map[string]string{
		"notes": "Verify whether latest patch resolved information disclosure",
	})
	retestReq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("/api/runs/%s/findings/%s/retests", run.ID, finding.ID), bytes.NewReader(retestReqBody))
	rrRetest := httptest.NewRecorder()
	env.router.ServeHTTP(rrRetest, retestReq)
	if rrRetest.Code != http.StatusCreated {
		t.Fatalf("create retest API failed: %d (%s)", rrRetest.Code, rrRetest.Body.String())
	}

	var respMap map[string]any
	_ = json.Unmarshal(rrRetest.Body.Bytes(), &respMap)
	retestData, _ := respMap["retest"].(map[string]any)
	retestIDStr, _ := retestData["id"].(string)
	retestID, err := uuid.Parse(retestIDStr)
	if err != nil {
		t.Fatalf("parse retest id: %v", err)
	}

	// Wait for RetestExecutor to complete via Manager
	var finishedRetest *model.FindingRetest
	for i := 0; i < 50; i++ {
		time.Sleep(100 * time.Millisecond)
		retests, err := env.runService.ListFindingRetests(ctx, run.ID, finding.ID)
		if err == nil && len(retests) > 0 {
			for _, rt := range retests {
				if rt.ID == retestID && rt.Status == "completed" {
					finishedRetest = &rt
					break
				}
			}
		}
		if finishedRetest != nil {
			break
		}
	}

	if finishedRetest == nil {
		t.Fatal("timed out waiting for retest executor to finish")
	}

	// 1. Verify real HTTP request hit the fixture
	if fixtureRequests.Load() == 0 {
		t.Error("expected at least 1 real HTTP request to hit the test fixture during retest")
	}

	// 2. Verify agent_tool_calls has a recorded get_endpoint tool call for this session with status success
	var toolCallCount int
	_ = env.pool.QueryRow(ctx,
		`SELECT count(*) FROM agent_tool_calls WHERE session_id = $1 AND tool_name = 'get_endpoint' AND status = 'success'`,
		finishedRetest.SessionID).Scan(&toolCallCount)
	if toolCallCount == 0 {
		t.Error("expected successful get_endpoint tool call recorded in DB for retest session")
	}

	// 3. Verify verdict is fixed (from fixture response)
	if finishedRetest.Verdict == nil || *finishedRetest.Verdict != "fixed" {
		t.Errorf("expected retest verdict fixed, got %v", finishedRetest.Verdict)
	}

	// 4. Verify Finding verification_state is retested
	var fState, fVerdict string
	_ = env.pool.QueryRow(ctx,
		`SELECT verification_state, retest_verdict FROM findings WHERE id = $1`,
		finding.ID).Scan(&fState, &fVerdict)
	if fState != "retested" || fVerdict != "fixed" {
		t.Errorf("expected finding verification_state=retested retest_verdict=fixed, got state=%s verdict=%s", fState, fVerdict)
	}

	parentAfterRetest, parentErr := env.runService.Get(ctx, run.ID)
	if parentErr != nil || parentAfterRetest.Status != model.RunStatusCompleted {
		t.Fatalf("retest must not reopen original scan: %v", parentErr)
	}
	modelsUsed, toolsUsed, usageErr := env.runService.RunExecutionUsage(ctx, run.ID)
	if usageErr != nil || modelsUsed != 0 || toolsUsed != 1 {
		t.Fatalf("independent verifier must not consume scan epoch counters: models=%d tools=%d err=%v", modelsUsed, toolsUsed, usageErr)
	}
	// 5. Verify human review history was preserved!
	reviews, err := env.runService.ListFindingReviews(ctx, run.ID, finding.ID)
	if err != nil || len(reviews) != 1 {
		t.Fatalf("expected 1 preserved finding review, got %d (%v)", len(reviews), err)
	}
	if reviews[0].ToStatus != model.FindingStatusConfirmed {
		t.Errorf("expected review to_status confirmed, got %s", reviews[0].ToStatus)
	}
}
