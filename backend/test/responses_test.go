package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/tools"
)

func TestResponses_PersistedReplayAndConcurrentSessionIsolation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "Responses replay")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	requests := make(map[string]interface{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []map[string]interface{} `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		if len(req.Input) < 2 {
			t.Error("missing session prompts")
			w.WriteHeader(400)
			return
		}
		label, _ := req.Input[1]["content"].(string)
		if label != "session_A" && label != "session_B" {
			t.Error("unexpected session label")
			w.WriteHeader(400)
			return
		}
		w.Header().Set("x-request-id", "http_"+label)
		w.Header().Set("Content-Type", "application/json")
		if len(req.Input) == 2 {
			_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "resp_" + label, "status": "completed", "output": []interface{}{
				map[string]interface{}{"type": "reasoning", "id": "rs_" + label, "summary": []interface{}{}, "encrypted_content": "opaque_" + label},
				map[string]interface{}{"type": "message", "id": "msg_" + label, "role": "assistant", "phase": "commentary", "status": "completed", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "Reading samples"}}},
				map[string]interface{}{"type": "function_call", "id": "item_" + label, "call_id": "shared_call_id", "name": "list_samples", "arguments": "{}", "status": "completed"},
			}, "usage": map[string]int{"input_tokens": 11, "output_tokens": 6, "total_tokens": 17}})
			return
		}
		if len(req.Input) != 6 || req.Input[2]["encrypted_content"] != "opaque_"+label || req.Input[3]["phase"] != "commentary" || req.Input[4]["id"] != "item_"+label || req.Input[5]["call_id"] != "shared_call_id" || req.Input[5]["type"] != "function_call_output" {
			t.Error("lost native state or crossed session boundary")
			w.WriteHeader(400)
			return
		}
		result, _ := req.Input[5]["output"].(string)
		if !strings.Contains(result, "sample_home") {
			t.Error("actual tool result missing")
		}
		mu.Lock()
		if saved, ok := requests[label]; ok && !reflect.DeepEqual(saved, req.Input) {
			t.Error("database replay changed the wire input")
		}
		requests[label] = req.Input
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"id": "resp_final_" + label, "status": "completed", "output": []interface{}{map[string]interface{}{"type": "message", "role": "assistant", "phase": "final_answer", "content": []interface{}{map[string]interface{}{"type": "output_text", "text": "Done " + label}}}}})
	}))
	defer server.Close()
	p := provider.NewResponsesProvider(server.URL, "test-key", "test-model", server.Client())
	reg := tools.NewRegistry()
	reg.Register(tools.NewListSamplesTool())
	sessions := make([]model.AgentSession, 2)
	for i := range sessions {
		sess, err := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		sessions[i] = *sess
	}
	var wg sync.WaitGroup
	for i, sess := range sessions {
		wg.Add(1)
		go func(i int, sess model.AgentSession) {
			defer wg.Done()
			label := fmt.Sprintf("session_%c", 'A'+i)
			sr := agent.NewSessionRunner(sess.ID, run.ID, "worker", env.runService, p, reg, model.DefaultBudgetConfig())
			res, err := sr.Run(ctx, []provider.ChatMessage{{Role: provider.RoleSystem, Content: "Test"}, {Role: provider.RoleUser, Content: label}})
			if err != nil || res == nil {
				t.Errorf("session failed: %v", err)
				return
			}
			if res.FinishReason != "completed" || res.ModelCallsCount != 2 || res.ToolCallsCount != 1 || res.TotalTokens != 17 || res.FinalContent != "Done "+label {
				t.Errorf("unexpected result: %+v", res)
			}
		}(i, sess)
	}
	wg.Wait()
	for _, sess := range sessions {
		records, err := env.runService.ListAgentMessagesBySession(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 5 || len(records[2].ProviderPayload) == 0 {
			t.Fatalf("native state was not persisted: %d messages", len(records))
		}
		public, err := json.Marshal(records)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(public, []byte("opaque_")) || bytes.Contains(public, []byte("provider_payload")) || bytes.Contains(public, []byte("encrypted_content")) {
			t.Fatal("opaque state leaked through public message JSON")
		}
		reloaded, err := env.runService.LoadSessionConversation(ctx, sess.ID)
		if err != nil {
			t.Fatal(err)
		}
		// Start with a new provider instance to prove replay needs no in-memory cache.
		_, err = provider.NewResponsesProvider(server.URL, "test-key", "test-model", server.Client()).Call(ctx, reloaded[:len(reloaded)-1], reg.Definitions(), provider.ModelOpts{})
		if err != nil {
			t.Fatal(err)
		}
	}
	calls, err := env.runService.ListModelCalls(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 4 {
		t.Fatalf("expected four model calls, got %d", len(calls))
	}
	for _, c := range calls {
		if c.ResponseID == nil || c.RequestID == nil || *c.ResponseID == *c.RequestID || !strings.HasPrefix(*c.RequestID, "http_") {
			t.Fatalf("IDs not recorded separately: %+v", c)
		}
		if c.UsageKnown != (c.Round == 1) || (c.Round == 1 && c.TotalTokens != 17) {
			t.Fatalf("unknown usage was lost: %+v", c)
		}
	}
}

func TestResponses_HTTPRunUsesRecordedFormat(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("wrong route: %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		var req map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req["model"] != "responses-test" {
			t.Error("model snapshot lost")
		}
		_, _ = w.Write([]byte(`{"id":"resp_http","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"HTTP configured Responses works"}]}]}`))
	}))
	defer server.Close()
	t.Setenv("LLM_API_FORMAT", "responses")
	t.Setenv("LLM_BASE_URL", server.URL+"/v1")
	t.Setenv("LLM_MODEL", "responses-test")
	t.Setenv("LLM_API_KEY", "local-secret")
	task := createTestTask(t, env, "Responses HTTP")
	req := httptest.NewRequest("POST", "/api/tasks/"+task.ID.String()+"/runs", strings.NewReader(`{"executor":"agent","provider":"openai"}`))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("create failed: %d %s", w.Code, w.Body.String())
	}
	var run model.Run
	if err := json.Unmarshal(w.Body.Bytes(), &run); err != nil {
		t.Fatal(err)
	}
	var snapshot struct {
		Config map[string]interface{} `json:"agent_config"`
	}
	_ = json.Unmarshal(run.ExecutionInput, &snapshot)
	if snapshot.Config["api_format"] != "responses" || snapshot.Config["model"] != "responses-test" || snapshot.Config["endpoint"] != server.URL+"/v1" || bytes.Contains(run.ExecutionInput, []byte("local-secret")) {
		t.Fatalf("invalid safe snapshot: %s", run.ExecutionInput)
	}
	final := waitForRunTerminal(t, env, run.ID, 5*time.Second)
	if final.Status != model.RunStatusCompleted || final.FinalOutput == nil || *final.FinalOutput != "HTTP configured Responses works" {
		t.Fatalf("wrong final run: %+v", final)
	}
}

func TestResponses_InvalidOrTruncatedOutputNeverExecutesTools(t *testing.T) {
	for _, tc := range []struct{ name, body, reason string }{
		{"truncated", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"call","name":"list_samples","arguments":"{"}]}`, "model_output_truncated"},
		{"refused", `{"status":"completed","output":[{"type":"function_call","call_id":"call","name":"list_samples","arguments":"{}"},{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}]}`, "model_refused"},
		{"duplicate", `{"status":"completed","output":[{"type":"function_call","call_id":"same","name":"list_samples","arguments":"{}"},{"type":"function_call","call_id":"same","name":"list_samples","arguments":"{}"}]}`, "protocol_error"},
		{"tool_budget", `{"status":"completed","output":[{"type":"function_call","call_id":"one","name":"list_samples","arguments":"{}"},{"type":"function_call","call_id":"two","name":"list_samples","arguments":"{}"}]}`, "max_tool_calls_exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(tc.body)) }))
			defer server.Close()
			task := createTestTask(t, env, tc.name)
			run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			sess, err := env.runService.CreateSession(context.Background(), run.ID, "worker", nil, nil, nil, nil)
			if err != nil {
				t.Fatal(err)
			}
			budget := model.DefaultBudgetConfig()
			budget.MaxToolCalls = 1
			runner := agent.NewSessionRunner(sess.ID, run.ID, "worker", env.runService, provider.NewResponsesProvider(server.URL, "k", "m", server.Client()), nil, budget)
			res, _ := runner.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: "Test"}})
			if res == nil || res.FinishReason != tc.reason {
				t.Fatalf("unexpected result: %+v", res)
			}
			calls, err := env.runService.ListAgentToolCalls(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 0 {
				t.Fatal("tools executed on invalid or truncated model output")
			}
		})
	}
}
