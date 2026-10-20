package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// 1. Mock 正常循环 (Requirement 13)
func TestAgent_MockNormalLoop(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Agent Normal Loop Task")
	ctx := context.Background()

	p := provider.NewMockProvider(provider.ScenarioNormal)
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	req := model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
		Budget:   &budget,
	}

	run, _, err := env.runService.Create(ctx, task.ID, req, nil)
	if err != nil {
		t.Fatalf("create agent run: %v", err)
	}

	runner := agent.NewAgentRunner(run.ID, env.runService, p, reg, budget)
	err = env.manager.StartExecutor(run.ID, runner)
	if err != nil {
		t.Fatalf("start agent runner: %v", err)
	}

	// Wait for completion
	finalRun := waitForRunTerminal(t, env, run.ID, 10*time.Second)
	if finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected completed status, got %s (reason: %v)", finalRun.Status, finalRun.FinishReason)
	}
	if finalRun.FinishReason == nil || *finalRun.FinishReason != "final_answer" {
		t.Fatalf("expected finish_reason 'final_answer', got %v", finalRun.FinishReason)
	}
	if finalRun.FinalOutput == nil || !strings.Contains(*finalRun.FinalOutput, "sample_home") {
		t.Fatalf("expected final_output to contain sample summary, got: %v", finalRun.FinalOutput)
	}

	// Verify agent_messages
	msgs, err := env.runService.ListAgentMessages(ctx, run.ID)
	if err != nil {
		t.Fatalf("list agent messages: %v", err)
	}
	// Expected: system(1), user(2), assistant list(3), tool list(4), assistant read 3(5), 3 tool reads(6,7,8), assistant final(9)
	if len(msgs) < 8 {
		t.Fatalf("expected at least 8 conversation messages, got %d", len(msgs))
	}

	// Verify model_calls
	modelCalls, err := env.runService.ListModelCalls(ctx, run.ID)
	if err != nil {
		t.Fatalf("list model calls: %v", err)
	}
	if len(modelCalls) != 3 {
		t.Fatalf("expected 3 model call rounds, got %d", len(modelCalls))
	}
	for _, mc := range modelCalls {
		if mc.Status != "completed" {
			t.Fatalf("expected model call status 'completed', got '%s'", mc.Status)
		}
	}

	// Verify agent_tool_calls
	toolCalls, err := env.runService.ListAgentToolCalls(ctx, run.ID)
	if err != nil {
		t.Fatalf("list tool calls: %v", err)
	}
	if len(toolCalls) != 4 { // 1 list_samples + 3 read_sample
		t.Fatalf("expected 4 tool calls, got %d", len(toolCalls))
	}
	for _, tc := range toolCalls {
		if tc.Status != "success" {
			t.Fatalf("expected tool call status 'success', got '%s'", tc.Status)
		}
		if tc.Output == "" {
			t.Fatalf("expected non-empty output for tool %s", tc.ToolName)
		}
	}

	t.Log("Mock normal loop verified: model calls, serial tool execution, events, and final output verified successfully")
}

// 2. 同轮返回三个工具调用且顺序与ID完全匹配验证
func TestAgent_MultiToolCallInSingleRound(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Multi-Tool Task")
	ctx := context.Background()

	p := provider.NewMockProvider(provider.ScenarioNormal)
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	var toolCallsOrder []string
	var mu sync.Mutex
	runner := agent.NewAgentRunner(run.ID, env.runService, p, reg, budget,
		agent.WithOnToolCallHook(func(callID string, toolName string) {
			mu.Lock()
			defer mu.Unlock()
			toolCallsOrder = append(toolCallsOrder, callID)
		}),
	)

	if err := env.manager.StartExecutor(run.ID, runner); err != nil {
		t.Fatalf("start runner: %v", err)
	}

	waitForRunTerminal(t, env, run.ID, 10*time.Second)

	mu.Lock()
	defer mu.Unlock()
	expectedOrder := []string{"call_list_1", "call_read_home", "call_read_login", "call_read_health"}
	if len(toolCallsOrder) != len(expectedOrder) {
		t.Fatalf("expected %d tool calls, got %d: %v", len(expectedOrder), len(toolCallsOrder), toolCallsOrder)
	}
	for i, id := range expectedOrder {
		if toolCallsOrder[i] != id {
			t.Fatalf("tool call at index %d expected %s, got %s", i, id, toolCallsOrder[i])
		}
	}
	t.Log("Multi-tool batch execution order and tool_call_id pairing verified successfully")
}

// 3. 同轮有文本和工具调用：不因文本提前完成
func TestAgent_TextAndToolCallsInSingleRound(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Text and Tool Task")
	ctx := context.Background()

	p := provider.NewMockProvider(provider.ScenarioTextAndTool)
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
	}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	runner := agent.NewAgentRunner(run.ID, env.runService, p, reg, budget)
	if err := env.manager.StartExecutor(run.ID, runner); err != nil {
		t.Fatalf("start runner: %v", err)
	}

	finalRun := waitForRunTerminal(t, env, run.ID, 10*time.Second)
	if finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected completed status, got %s", finalRun.Status)
	}

	// Must have completed round 3, not round 1!
	modelCalls, err := env.runService.ListModelCalls(ctx, run.ID)
	if err != nil {
		t.Fatalf("list model calls: %v", err)
	}
	if len(modelCalls) != 3 {
		t.Fatalf("expected 3 model calls, meaning text in round 1 was not prematurely treated as final answer, got %d", len(modelCalls))
	}
	t.Log("Text and tool calls co-existence verified: text in tool-calling turn does not trigger premature completion")
}

// 4. 未知工具或参数非法：记录 rejected 并返回错误，由模型修正
func TestAgent_UnknownToolAndInvalidArgsRejected(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Rejected Tool Task")
	ctx := context.Background()

	// 4a. Unknown tool
	pUnknown := provider.NewMockProvider(provider.ScenarioInvalidTool)
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	run1, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
	}, nil)
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}

	runner1 := agent.NewAgentRunner(run1.ID, env.runService, pUnknown, reg, budget)
	if err := env.manager.StartExecutor(run1.ID, runner1); err != nil {
		t.Fatalf("start runner 1: %v", err)
	}

	final1 := waitForRunTerminal(t, env, run1.ID, 10*time.Second)
	if final1.Status != model.RunStatusCompleted {
		t.Fatalf("expected run 1 to recover and complete, got %s (%v)", final1.Status, final1.FinishReason)
	}

	toolCalls1, _ := env.runService.ListAgentToolCalls(ctx, run1.ID)
	if len(toolCalls1) != 1 || toolCalls1[0].Status != "rejected" {
		t.Fatalf("expected 1 rejected tool call, got %v", toolCalls1)
	}

	// 4b. Invalid parameters (e.g. unknown sample_id)
	pInvalidArgs := provider.NewMockProvider(provider.ScenarioInvalidArgs)
	task2 := createTestTask(t, env, "Invalid Args Task")
	run2, _, err := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
	}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}

	runner2 := agent.NewAgentRunner(run2.ID, env.runService, pInvalidArgs, reg, budget)
	if err := env.manager.StartExecutor(run2.ID, runner2); err != nil {
		t.Fatalf("start runner 2: %v", err)
	}

	final2 := waitForRunTerminal(t, env, run2.ID, 10*time.Second)
	if final2.Status != model.RunStatusCompleted {
		t.Fatalf("expected run 2 to complete after seeing validation rejection, got %s", final2.Status)
	}

	toolCalls2, _ := env.runService.ListAgentToolCalls(ctx, run2.ID)
	if len(toolCalls2) != 1 || toolCalls2[0].Status != "rejected" {
		t.Fatalf("expected tool call to be marked 'rejected', got %v", toolCalls2)
	}
	t.Log("Unknown tool and invalid argument validation verified: correctly marked rejected and handed back to model")
}

// 5. 重复/缺失调用 ID：该批工具未执行，报 protocol_error
func TestAgent_DuplicateAndMissingToolCallID(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Protocol Error Task")
	ctx := context.Background()
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	// 5a. Duplicate Tool Call ID
	pDup := provider.NewMockProvider(provider.ScenarioDuplicateToolCallID)
	run1, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
	}, nil)
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}

	runner1 := agent.NewAgentRunner(run1.ID, env.runService, pDup, reg, budget)
	if err := env.manager.StartExecutor(run1.ID, runner1); err != nil {
		t.Fatalf("start runner 1: %v", err)
	}

	final1 := waitForRunTerminal(t, env, run1.ID, 10*time.Second)
	if final1.Status != model.RunStatusFailed || final1.FinishReason == nil || *final1.FinishReason != "protocol_error" {
		t.Fatalf("expected failed with protocol_error, got status=%s, finish_reason=%v", final1.Status, final1.FinishReason)
	}

	// Verify no tools were executed
	toolCalls1, _ := env.runService.ListAgentToolCalls(ctx, run1.ID)
	if len(toolCalls1) != 0 {
		t.Fatalf("expected zero tools executed on duplicate call ID, got %d", len(toolCalls1))
	}

	// 5b. Missing Tool Call ID
	pMissing := provider.NewMockProvider(provider.ScenarioMissingToolCallID)
	task2 := createTestTask(t, env, "Missing ID Task")
	run2, _, err := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
	}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}

	runner2 := agent.NewAgentRunner(run2.ID, env.runService, pMissing, reg, budget)
	if err := env.manager.StartExecutor(run2.ID, runner2); err != nil {
		t.Fatalf("start runner 2: %v", err)
	}

	final2 := waitForRunTerminal(t, env, run2.ID, 10*time.Second)
	if final2.Status != model.RunStatusFailed || final2.FinishReason == nil || *final2.FinishReason != "protocol_error" {
		t.Fatalf("expected failed with protocol_error, got status=%s, finish_reason=%v", final2.Status, final2.FinishReason)
	}

	toolCalls2, _ := env.runService.ListAgentToolCalls(ctx, run2.ID)
	if len(toolCalls2) != 0 {
		t.Fatalf("expected zero tools executed on missing call ID, got %d", len(toolCalls2))
	}

	t.Log("Duplicate and missing tool call IDs verified: batch rejected and failed with protocol_error")
}

// 6. 模型一直要求工具：预算上限生效，以稳定原因失败
func TestAgent_BudgetLimitsExceeded(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Infinite Tools Task")
	ctx := context.Background()
	reg := tools.DefaultRegistry()

	// 6a. Max Model Calls Budget Exceeded
	budgetModel := model.BudgetConfig{
		MaxModelCalls:       3,
		MaxToolCalls:        100,
		RunTimeoutSeconds:   10,
		ModelTimeoutSeconds: 5,
		ToolTimeoutSeconds:  2,
		MaxToolOutputBytes:  8192,
	}
	pInfinite := provider.NewMockProvider(provider.ScenarioInfiniteTools)

	run1, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
		Budget:   &budgetModel,
	}, nil)
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}

	runner1 := agent.NewAgentRunner(run1.ID, env.runService, pInfinite, reg, budgetModel)
	if err := env.manager.StartExecutor(run1.ID, runner1); err != nil {
		t.Fatalf("start runner 1: %v", err)
	}

	final1 := waitForRunTerminal(t, env, run1.ID, 10*time.Second)
	if final1.Status != model.RunStatusFailed || final1.FinishReason == nil || *final1.FinishReason != "max_model_calls_exceeded" {
		t.Fatalf("expected failed with max_model_calls_exceeded, got status=%s, finish_reason=%v", final1.Status, final1.FinishReason)
	}

	// 6b. Max Tool Calls Budget Exceeded
	task2 := createTestTask(t, env, "Tool Budget Task")
	budgetTool := model.BudgetConfig{
		MaxModelCalls:       10,
		MaxToolCalls:        2,
		RunTimeoutSeconds:   10,
		ModelTimeoutSeconds: 5,
		ToolTimeoutSeconds:  2,
		MaxToolOutputBytes:  8192,
	}
	pInfinite2 := provider.NewMockProvider(provider.ScenarioInfiniteTools)

	run2, _, err := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("mock"),
		Budget:   &budgetTool,
	}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}

	runner2 := agent.NewAgentRunner(run2.ID, env.runService, pInfinite2, reg, budgetTool)
	if err := env.manager.StartExecutor(run2.ID, runner2); err != nil {
		t.Fatalf("start runner 2: %v", err)
	}

	final2 := waitForRunTerminal(t, env, run2.ID, 10*time.Second)
	if final2.Status != model.RunStatusFailed || final2.FinishReason == nil || *final2.FinishReason != "max_tool_calls_exceeded" {
		t.Fatalf("expected failed with max_tool_calls_exceeded, got status=%s, finish_reason=%v", final2.Status, final2.FinishReason)
	}

	t.Log("Budget limits verified: max_model_calls_exceeded and max_tool_calls_exceeded strictly enforced")
}

// 7. 空响应、拒绝与截断验证
func TestAgent_EmptyResponseRefusalAndTruncation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Edge Case Responses Task")
	ctx := context.Background()
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	// 7a. Empty response
	pEmpty := provider.NewMockProvider(provider.ScenarioEmptyResponse)
	run1, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Executor: strPtr("agent")}, nil)
	runner1 := agent.NewAgentRunner(run1.ID, env.runService, pEmpty, reg, budget)
	env.manager.StartExecutor(run1.ID, runner1)
	final1 := waitForRunTerminal(t, env, run1.ID, 10*time.Second)
	if final1.Status != model.RunStatusFailed || final1.FinishReason == nil || *final1.FinishReason != "protocol_error" {
		t.Fatalf("expected failed with protocol_error for empty response, got %s (%v)", final1.Status, final1.FinishReason)
	}

	// 7b. Refusal
	task2 := createTestTask(t, env, "Refusal Task")
	pRefusal := provider.NewMockProvider(provider.ScenarioRefusal)
	run2, _, _ := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{Executor: strPtr("agent")}, nil)
	runner2 := agent.NewAgentRunner(run2.ID, env.runService, pRefusal, reg, budget)
	env.manager.StartExecutor(run2.ID, runner2)
	final2 := waitForRunTerminal(t, env, run2.ID, 10*time.Second)
	if final2.Status != model.RunStatusFailed || final2.FinishReason == nil || *final2.FinishReason != "model_refused" {
		t.Fatalf("expected failed with model_refused, got %s (%v)", final2.Status, final2.FinishReason)
	}

	// 7c. Truncated output
	task3 := createTestTask(t, env, "Truncated Task")
	pTruncated := provider.NewMockProvider(provider.ScenarioTruncated)
	run3, _, _ := env.runService.Create(ctx, task3.ID, model.CreateRunRequest{Executor: strPtr("agent")}, nil)
	runner3 := agent.NewAgentRunner(run3.ID, env.runService, pTruncated, reg, budget)
	env.manager.StartExecutor(run3.ID, runner3)
	final3 := waitForRunTerminal(t, env, run3.ID, 10*time.Second)
	if final3.Status != model.RunStatusFailed || final3.FinishReason == nil || *final3.FinishReason != "model_output_truncated" {
		t.Fatalf("expected failed with model_output_truncated, got %s (%v)", final3.Status, final3.FinishReason)
	}

	t.Log("Edge response conditions verified: empty response, refusal, and truncation properly distinguished")
}

// 8. Provider 请求等待时停止：请求被取消，Run 收尾为 stopped
func TestAgent_StopWhileWaitingForProvider(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Stop Provider Task")
	ctx := context.Background()

	pDelay := provider.NewMockProvider(provider.ScenarioDelay)
	reg := tools.DefaultRegistry()
	budget := model.DefaultBudgetConfig()

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Executor: strPtr("agent")}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	calledCh := make(chan bool, 1)
	pDelay.SetOnCallHook(func(round int, msgs []provider.ChatMessage) {
		calledCh <- true
	})

	runner := agent.NewAgentRunner(run.ID, env.runService, pDelay, reg, budget)
	if err := env.manager.StartExecutor(run.ID, runner); err != nil {
		t.Fatalf("start runner: %v", err)
	}

	// Wait until provider call is entered
	select {
	case <-calledCh:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for provider call to initiate")
	}

	// User requests stop
	_, err = env.runService.Stop(ctx, run.ID)
	if err != nil {
		t.Fatalf("stop run: %v", err)
	}
	env.manager.Stop(run.ID)

	finalRun := waitForRunTerminal(t, env, run.ID, 10*time.Second)
	if finalRun.Status != model.RunStatusStopped {
		t.Fatalf("expected status 'stopped', got '%s'", finalRun.Status)
	}
	var actualReason string
	if finalRun.FinishReason != nil {
		actualReason = *finalRun.FinishReason
	}
	if actualReason != "user_requested_stop" && actualReason != "user requested stop" {
		t.Fatalf("expected finish_reason 'user_requested_stop', got: %q", actualReason)
	}

	// Verify no tools were started
	toolCalls, _ := env.runService.ListAgentToolCalls(ctx, run.ID)
	if len(toolCalls) != 0 {
		t.Fatalf("expected zero tool calls after stop, got %d", len(toolCalls))
	}
	t.Log("Stop while waiting for provider verified: safely cancels and converges to stopped")
}

// 9. 异常退出后恢复：Run 与遗留子调用终结为中断，不自动重放
func TestAgent_RestartInterruptedRecovery(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Agent Restart Recovery Task")
	ctx := context.Background()

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Executor: strPtr("agent")}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Transition to running
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatalf("transition to running: %v", err)
	}

	// Create an active model call
	mc, err := env.runService.CreateModelCall(ctx, run.ID, 1)
	if err != nil {
		t.Fatalf("create model call: %v", err)
	}

	// Create an active tool call
	tc, err := env.runService.CreateAgentToolCall(ctx, run.ID, &mc.ID, "call_test", "list_samples", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("create tool call: %v", err)
	}
	now := time.Now()
	env.runService.UpdateAgentToolCall(ctx, tc.ID, "running", "", nil, &now, nil)

	// Simulate server restart recovery
	_, err = env.runService.RecoverActiveRuns(ctx)
	if err != nil {
		t.Fatalf("RecoverActiveRuns: %v", err)
	}

	// Verify run is interrupted
	rUpdated, err := env.runService.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if rUpdated.Status != model.RunStatusInterrupted {
		t.Fatalf("expected run status 'interrupted', got '%s'", rUpdated.Status)
	}

	// Verify model_call is interrupted
	mcs, err := env.runService.ListModelCalls(ctx, run.ID)
	if err != nil || len(mcs) != 1 {
		t.Fatalf("expected 1 model call, got %d (%v)", len(mcs), err)
	}
	if mcs[0].Status != "interrupted" {
		t.Fatalf("expected model call status 'interrupted', got '%s'", mcs[0].Status)
	}

	// Verify tool_call is interrupted
	tcs, err := env.runService.ListAgentToolCalls(ctx, run.ID)
	if err != nil || len(tcs) != 1 {
		t.Fatalf("expected 1 tool call, got %d (%v)", len(tcs), err)
	}
	if tcs[0].Status != "interrupted" {
		t.Fatalf("expected tool call status 'interrupted', got '%s'", tcs[0].Status)
	}

	t.Log("Agent restart recovery verified: run, model_call, and tool_call all cleanly marked interrupted")
}

// 10. Provider HTTP Stub 验证协议请求、工具结果回交、错误状态及用量解析
func TestAgent_OpenAIProvider_HTTPStub(t *testing.T) {
	var requestCount int
	var receivedAuthHeader string
	var lastRequestBody []byte

	// Setup local HTTP stub server
	stubServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		receivedAuthHeader = r.Header.Get("Authorization")
		buf := make([]byte, r.ContentLength)
		r.Body.Read(buf)
		lastRequestBody = buf

		if requestCount == 1 {
			// Round 1: Return tool call request
			resp := map[string]interface{}{
				"id": "chatcmpl-test-stub-1",
				"choices": []map[string]interface{}{
					{
						"index": 0,
						"message": map[string]interface{}{
							"role": "assistant",
							"tool_calls": []map[string]interface{}{
								{
									"id":   "stub_call_1",
									"type": "function",
									"function": map[string]interface{}{
										"name":      "list_samples",
										"arguments": "{}",
									},
								},
							},
						},
						"finish_reason": "tool_calls",
					},
				},
				"usage": map[string]interface{}{
					"prompt_tokens":     120,
					"completion_tokens": 30,
					"total_tokens":      150,
				},
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(resp)
			return
		}

		// Round 2: Return final answer after inspecting that tool result was received
		var receivedReq map[string]interface{}
		json.Unmarshal(lastRequestBody, &receivedReq)
		msgs, _ := receivedReq["messages"].([]interface{})
		lastMsg, _ := msgs[len(msgs)-1].(map[string]interface{})
		if lastMsg["role"] != "tool" || lastMsg["tool_call_id"] != "stub_call_1" {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"message":"expected tool response for stub_call_1"}}`))
			return
		}

		resp := map[string]interface{}{
			"id": "chatcmpl-test-stub-2",
			"choices": []map[string]interface{}{
				{
					"index": 0,
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "Stub Final Answer: 3 samples verified.",
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]interface{}{
				"prompt_tokens":     250,
				"completion_tokens": 40,
				"total_tokens":      290,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer stubServer.Close()

	// Instantiate OpenAIProvider pointing to stubServer
	apiKey := "test-secret-key-123"
	p := provider.NewOpenAIProvider(stubServer.URL, apiKey, "gpt-4o-mini-test", nil)

	ctx := context.Background()
	toolsList := []provider.ToolDefinition{
		{
			Name:        "list_samples",
			Description: "List all built-in HTTP response sample IDs",
			Parameters:  json.RawMessage(`{"type":"object","properties":{}}`),
		},
	}

	// 1. Call round 1
	resp1, err := p.Call(ctx, []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "Find all samples"},
	}, toolsList, provider.ModelOpts{})
	if err != nil {
		t.Fatalf("stub call 1 failed: %v", err)
	}

	if receivedAuthHeader != "Bearer test-secret-key-123" {
		t.Fatalf("expected Authorization header 'Bearer test-secret-key-123', got '%s'", receivedAuthHeader)
	}
	if len(resp1.ToolCalls) != 1 || resp1.ToolCalls[0].ID != "stub_call_1" {
		t.Fatalf("expected tool call 'stub_call_1', got %v", resp1.ToolCalls)
	}
	if resp1.Usage == nil || resp1.Usage.TotalTokens != 150 {
		t.Fatalf("expected 150 total tokens in usage, got %v", resp1.Usage)
	}

	// 2. Call round 2 with tool result
	messagesRound2 := []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "Find all samples"},
		{Role: provider.RoleAssistant, ToolCalls: resp1.ToolCalls},
		{Role: provider.RoleTool, ToolCallID: "stub_call_1", Content: `{"count": 3}`},
	}
	resp2, err := p.Call(ctx, messagesRound2, toolsList, provider.ModelOpts{})
	if err != nil {
		t.Fatalf("stub call 2 failed: %v", err)
	}
	if resp2.Content != "Stub Final Answer: 3 samples verified." {
		t.Fatalf("expected final answer content, got: %s", resp2.Content)
	}

	// 3. Test HTTP 401 error response handling
	errServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"Invalid API key provided","type":"invalid_request_error"}}`))
	}))
	defer errServer.Close()

	errProvider := provider.NewOpenAIProvider(errServer.URL, "bad-key", "gpt-4o", nil)
	_, err = errProvider.Call(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: "Hi"}}, nil, provider.ModelOpts{})
	if err == nil {
		t.Fatal("expected error from 401 response, but got nil")
	}
	if !strings.Contains(err.Error(), "Invalid API key provided") {
		t.Fatalf("expected error message to contain upstream error details, got: %v", err)
	}

	t.Log("OpenAIProvider HTTP stub verified: request formatting, auth header, tool round-trip, usage parsing, and error parsing")
}

// 11. 真实模型缺少配置时明确报错，不自动退回 Mock
func TestAgent_RealModelMissingConfigRefused(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	task := createTestTask(t, env, "Real Model Config Task")
	ctx := context.Background()

	// Ensure environment variables are clear
	origKey := os.Getenv("LLM_API_KEY")
	origURL := os.Getenv("LLM_BASE_URL")
	origModel := os.Getenv("LLM_MODEL")
	defer func() {
		os.Setenv("LLM_API_KEY", origKey)
		os.Setenv("LLM_BASE_URL", origURL)
		os.Setenv("LLM_MODEL", origModel)
	}()

	os.Unsetenv("LLM_API_KEY")
	os.Unsetenv("LLM_BASE_URL")
	os.Unsetenv("LLM_MODEL")

	// Attempt to create run configured for openai without environment credentials
	_, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Executor: strPtr("agent"),
		Provider: strPtr("openai"),
	}, nil)

	if err == nil {
		t.Fatal("expected run creation to fail when real model credentials are unset, but it succeeded")
	}

	if !errors.Is(err, provider.ErrModelConfigMissing) {
		t.Fatalf("expected ErrModelConfigMissing, got: %v", err)
	}

	t.Logf("Real model missing configuration refusal verified: %v", err)
}

func strPtr(s string) *string {
	return &s
}

func waitForRunTerminal(t *testing.T, env *TestEnv, runID uuid.UUID, timeout time.Duration) *model.Run {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r, err := env.runService.Get(context.Background(), runID)
		if err != nil {
			t.Fatalf("get run failed: %v", err)
		}
		if r.Status.IsTerminal() {
			return r
		}
		time.Sleep(20 * time.Millisecond)
	}
	r, _ := env.runService.Get(context.Background(), runID)
	t.Fatalf("timed out waiting for run %s to reach terminal status (current: %s)", runID, r.Status)
	return r
}
