package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

const toolOutput = `[
 {"type":"reasoning","id":"rs_1","summary":[],"encrypted_content":"opaque-ciphertext"},
 {"type":"message","id":"msg_1","role":"assistant","status":"completed","phase":"commentary","content":[{"type":"output_text","text":"Inspecting samples","annotations":[]}]},
 {"type":"function_call","id":"fc_item_1","call_id":"call_1","name":"list_samples","arguments":"{}","status":"completed"},
 {"type":"function_call","id":"fc_item_2","call_id":"call_2","name":"list_samples","arguments":"{}","status":"completed"}
]`

func TestResponsesRoundTrip(t *testing.T) {
	round := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		if r.URL.Path != "/v1/responses" || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Error("wrong endpoint, method or authorization")
		}
		var req map[string]interface{}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		if req["store"] != false || req["stream"] != false || req["model"] != "test-model" || req["previous_response_id"] != nil {
			t.Error("expected stateless, non-streaming request")
		}
		defs := req["tools"].([]interface{})
		def := defs[0].(map[string]interface{})
		if def["name"] != "list_samples" || def["type"] != "function" || def["strict"] != false || def["function"] != nil {
			t.Error("Responses tools must be flat, non-strict definitions")
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("x-request-id", "http-request-1")
		if round == 1 {
			_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":` + toolOutput + `,"usage":{"input_tokens":10,"output_tokens":7,"total_tokens":17}}`))
			return
		}
		input := req["input"].([]interface{})
		var expected []interface{}
		_ = json.Unmarshal([]byte(toolOutput), &expected)
		if len(input) != 8 {
			t.Errorf("expected system,user,4 native items,2 results; got %d", len(input))
			return
		}
		if !reflect.DeepEqual(input[2:6], expected) {
			t.Error("native items lost fields, order, phase or encrypted reasoning")
		}
		for i, id := range []string{"call_1", "call_2"} {
			result := input[6+i].(map[string]interface{})
			if result["type"] != "function_call_output" || result["call_id"] != id || result["output"] != "tool result" {
				t.Error("tool output must reference call_id, not item id")
			}
		}
		_, _ = w.Write([]byte(`{"id":"resp_2","status":"completed","output":[{"id":"msg_final","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done."}]}]}`))
	}))
	defer server.Close()
	p := NewResponsesProvider(server.URL+"/v1/", "test-key", "test-model", server.Client())
	tools := []ToolDefinition{{Name: "list_samples", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}}
	messages := []ChatMessage{{Role: RoleSystem, Content: "System"}, {Role: RoleUser, Content: "Inspect"}}
	r, err := p.Call(context.Background(), messages, tools, ModelOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.ToolCalls) != 2 || r.ToolCalls[0].ID != "call_1" || r.FinishReason != "tool_calls" || r.Content != "Inspecting samples" {
		t.Fatalf("wrong normalized output: %+v", r)
	}
	if r.ResponseID != "resp_1" || r.RequestID != "http-request-1" || r.Usage == nil || r.Usage.TotalTokens != 17 {
		t.Fatalf("lost metadata: %+v", r)
	}
	// Serialize/reload just as the database does, then replay with a fresh adapter.
	raw, _ := json.Marshal(r.NativePayload)
	var saved NativePayload
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	messages = append(messages, ChatMessage{Role: RoleAssistant, Content: r.Content, ToolCalls: r.ToolCalls, NativePayload: &saved})
	for _, tc := range r.ToolCalls {
		messages = append(messages, ChatMessage{Role: RoleTool, ToolCallID: tc.ID, Content: "tool result"})
	}
	r, err = NewResponsesProvider(server.URL+"/v1", "test-key", "test-model", server.Client()).Call(context.Background(), messages, tools, ModelOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Content != "Done." || r.FinishReason != "stop" || r.Usage != nil {
		t.Fatalf("wrong final response or fabricated usage: %+v", r)
	}
}

func TestResponsesTerminalStates(t *testing.T) {
	for _, tc := range []struct {
		name, body, finish string
		wantErr            bool
	}{
		{"truncated", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"function_call","call_id":"partial","name":"list_samples","arguments":"{"}]}`, "length", false},
		{"refusal", `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"Cannot comply"}]}]}`, "refusal", false},
		{"empty", `{"status":"completed","output":[]}`, "", true},
		{"reasoning_only", `{"status":"completed","output":[{"type":"reasoning","summary":[]}]}`, "", true},
		{"commentary_only", `{"status":"completed","output":[{"type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Working..."}]}]}`, "", true},
		{"duplicate_call", `{"status":"completed","output":[{"type":"function_call","call_id":"same","name":"x","arguments":"{}"},{"type":"function_call","call_id":"same","name":"x","arguments":"{}"}]}`, "", true},
		{"missing_call_id", `{"status":"completed","output":[{"type":"function_call","id":"item-id","name":"x","arguments":"{}"}]}`, "", true},
		{"malformed_arguments", `{"status":"completed","output":[{"type":"function_call","call_id":"call","name":"x","arguments":"{"}]}`, "", true},
		{"partial_call", `{"status":"completed","output":[{"type":"function_call","call_id":"call","name":"x","arguments":"{}","status":"in_progress"}]}`, "", true},
		{"hosted_tool", `{"status":"completed","output":[{"type":"web_search_call","id":"ws"}]}`, "", true},
		{"failed", `{"id":"resp_failed","status":"failed","error":{"message":"secret upstream text"},"output":[]}`, "", true},
		{"cancelled", `{"status":"cancelled","output":[]}`, "", true},
		{"in_progress", `{"status":"in_progress","output":[]}`, "", true},
		{"content_filter", `{"status":"incomplete","incomplete_details":{"reason":"content_filter"},"output":[]}`, "", true},
		{"bad_json", `{oops`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, err := parseResponse([]byte(tc.body))
			if (err != nil) != tc.wantErr {
				t.Fatalf("response=%+v error=%v", r, err)
			}
			if !tc.wantErr && r.FinishReason != tc.finish {
				t.Fatalf("finish=%s", r.FinishReason)
			}
			if err != nil && strings.Contains(err.Error(), "secret upstream text") {
				t.Fatal("upstream error body leaked")
			}
		})
	}
}

func TestResponsesErrorsAndCancellation(t *testing.T) {
	for _, status := range []int{401, 404, 429, 500} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			n := 0
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				if r.URL.Path != "/responses" {
					t.Error("protocol fallback attempted")
				}
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"test-key"}}`))
			}))
			defer s.Close()
			_, err := NewResponsesProvider(s.URL, "test-key", "m", s.Client()).Call(context.Background(), nil, nil, ModelOpts{})
			if !errors.Is(err, ErrOpenAIUpstream) || n != 1 || strings.Contains(err.Error(), "test-key") {
				t.Fatalf("wrong error handling: %v, calls=%d", err, n)
			}
		})
	}
	for _, mode := range []string{"cancel", "timeout"} {
		t.Run(mode, func(t *testing.T) {
			started, release := make(chan struct{}), make(chan struct{})
			s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				close(started)
				select {
				case <-r.Context().Done():
				case <-release:
				}
			}))
			defer s.Close()
			defer close(release)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			opts := ModelOpts{}
			expected := context.Canceled
			if mode == "timeout" {
				opts.Timeout = 100 * time.Millisecond
				expected = context.DeadlineExceeded
			}
			done := make(chan error, 1)
			go func() {
				_, err := NewResponsesProvider(s.URL, "k", "m", s.Client()).Call(ctx, nil, nil, opts)
				done <- err
			}()
			<-started
			if mode == "cancel" {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, expected) {
					t.Fatalf("lost cancellation/timeout: %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("request ignored cancellation/timeout")
			}
		})
	}
}

func TestConfiguredProvider(t *testing.T) {
	base := Config{APIKey: "local-test", BaseURL: "https://example.test/v1/", Model: "test"}
	p, err := NewConfiguredProvider(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*OpenAIProvider); !ok {
		t.Fatal("default must remain chat_completions")
	}
	base.APIFormat = FormatResponses
	p, err = NewConfiguredProvider(base, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := p.(*ResponsesProvider); !ok {
		t.Fatal("responses not selected")
	}
	for _, endpoint := range []string{"https://example.test/v1?key=secret", "https://user:secret@example.test/v1", "https://example.test/v1/responses", "https://example.test/v1/chat/completions", "ftp://example.test", "not a url"} {
		c := base
		c.BaseURL = endpoint
		if _, err := NewConfiguredProvider(c, nil); !errors.Is(err, ErrModelConfigInvalid) || strings.Contains(err.Error(), "secret") {
			t.Fatalf("unsafe base URL validation: %v", err)
		}
	}
	base.APIFormat = "response"
	if _, err := NewConfiguredProvider(base, nil); !errors.Is(err, ErrModelConfigInvalid) {
		t.Fatal("unknown format silently accepted")
	}
	base.APIFormat = FormatResponses
	base.APIKey = ""
	if _, err := NewConfiguredProvider(base, nil); !errors.Is(err, ErrModelConfigMissing) {
		t.Fatal("missing configuration silently accepted")
	}
	t.Setenv("LLM_API_KEY", "local-test")
	t.Setenv("LLM_API_FORMAT", FormatResponses)
	t.Setenv("LLM_MODEL", "new-model")
	t.Setenv("LLM_BASE_URL", "https://new.example.test/v1")
	p, err = FromSnapshot(map[string]interface{}{"provider": "openai", "api_format": "chat_completions", "model": "frozen-model", "endpoint": "https://frozen.example.test/v1"}, "normal")
	if err != nil {
		t.Fatal(err)
	}
	old, ok := p.(*OpenAIProvider)
	if !ok || old.defaultModel != "frozen-model" || old.baseURL != "https://frozen.example.test/v1" {
		t.Fatal("snapshot routing changed with environment")
	}
}
