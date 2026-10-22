package provider

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Incomplete Chat Completions arguments must not be returned as executable tool
// calls. This reproduces the protocol failure class separately from worker races.
func TestMAProvider_ChatIncompleteArgumentsAreNotExecutable(t *testing.T) {
	const secret = "sk-test-should-not-leak"
	const prompt = "private prompt text that must stay out of errors"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req-incomplete")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_samples","arguments":"{"}}]}}]}`))
	}))
	defer server.Close()

	resp, err := NewOpenAIProvider(server.URL, secret, "test-model", server.Client()).Call(
		context.Background(),
		[]ChatMessage{{Role: RoleUser, Content: prompt}},
		[]ToolDefinition{{Name: "list_samples", Parameters: []byte(`{"type":"object"}`)}},
		ModelOpts{},
	)
	if err == nil && resp != nil && len(resp.ToolCalls) > 0 {
		t.Fatal("incomplete function arguments were returned as executable tool calls")
	}
	if err != nil && (strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), prompt) || strings.Contains(err.Error(), "Authorization")) {
		t.Fatalf("error leaked secret material: %v", err)
	}
}
