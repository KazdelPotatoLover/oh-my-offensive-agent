package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

const protocolSecret = "sk-test-should-not-leak"
const protocolPrompt = "private prompt text that must stay out of errors"

func assertNoSecret(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	text := err.Error()
	for _, secret := range []string{protocolSecret, protocolPrompt, "Authorization", "Bearer " + protocolSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked secret material: %v", err)
		}
	}
}

func TestMAProvider_ChatIncompleteArgumentsAreClassified(t *testing.T) {
	body, err := os.ReadFile("testdata/chat_incomplete_arguments.json")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-request-id", "req-incomplete")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer server.Close()
	resp, err := NewOpenAIProvider(server.URL, protocolSecret, "test-model", server.Client()).Call(
		context.Background(),
		[]ChatMessage{{Role: RoleUser, Content: protocolPrompt}},
		[]ToolDefinition{{Name: "list_samples", Parameters: []byte(`{"type":"object"}`)}},
		ModelOpts{},
	)
	if err == nil && resp != nil && len(resp.ToolCalls) > 0 {
		t.Fatal("incomplete function arguments were returned as executable tool calls")
	}
	if err == nil || !strings.Contains(err.Error(), "incomplete function arguments or status") || !errors.Is(err, ErrProtocol) {
		t.Fatalf("expected classified incomplete output, got %v", err)
	}
	assertNoSecret(t, err)
	var ce *CallError
	if !errors.As(err, &ce) || ce.Class != ClassIncomplete || ce.RequestID != "req-incomplete" || ce.DiagnosticRef == "" {
		t.Fatalf("missing diagnosis: %v", err)
	}
}

func TestMAProvider_ResponsesIncompleteOutputIsNotAWorkerRace(t *testing.T) {
	for _, name := range []string{"responses_incomplete_arguments.json", "responses_incomplete_status.json"} {
		t.Run(name, func(t *testing.T) {
			body, err := os.ReadFile("testdata/" + name)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				var req map[string]any
				_ = json.NewDecoder(r.Body).Decode(&req)
				if _, ok := req["previous_response_id"]; ok {
					t.Error("provider stored previous_response_id")
				}
				w.Header().Set("x-request-id", "req-"+name)
				_, _ = w.Write(body)
			}))
			defer server.Close()
			resp, err := NewResponsesProvider(server.URL, protocolSecret, "m", server.Client()).Call(
				context.Background(),
				[]ChatMessage{{Role: RoleUser, Content: protocolPrompt}},
				nil,
				ModelOpts{},
			)
			if n != 1 {
				t.Fatalf("provider retried internally: %d", n)
			}
			if err == nil || !strings.Contains(err.Error(), "incomplete function arguments or status") {
				t.Fatalf("incomplete output was not classified: resp=%+v err=%v", resp, err)
			}
			if resp != nil && len(resp.ToolCalls) > 0 {
				t.Fatal("incomplete function call entered the executable set")
			}
			assertNoSecret(t, err)
		})
	}
}

func TestMAProvider_CompatibleArgumentsAndOmittedStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp_ok","status":"completed","output":[{"type":"function_call","id":"item_should_not_be_call_id","call_id":"call_ok","name":"list_samples","arguments":{"sample_id":"home"}}]}`))
	}))
	defer server.Close()
	resp, err := NewResponsesProvider(server.URL, "k", "m", server.Client()).Call(context.Background(), nil, nil, ModelOpts{})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_ok" || resp.ToolCalls[0].Function.Arguments != `{"sample_id":"home"}` {
		t.Fatalf("object arguments were not normalized: %+v", resp)
	}
	if resp.NativePayload == nil || len(resp.NativePayload.Output) != 1 {
		t.Fatal("native payload was dropped")
	}
	_, err = parseResponse([]byte(`{"status":"completed","output":[{"type":"function_call","id":"item-only","name":"list_samples","arguments":"{}"}]}`))
	if err == nil || !errors.Is(err, ErrProtocol) {
		t.Fatal("item id was accepted as call_id")
	}
	_, err = parseResponse([]byte(`{"status":"completed","output":[{"type":"function_call","call_id":"call","name":"list_samples","arguments":""}]}`))
	if err == nil || strings.Contains(err.Error(), `{}`) {
		t.Fatalf("empty arguments were repaired: %v", err)
	}
}

func TestMAProvider_TruncationAndRefusalStayTerminal(t *testing.T) {
	truncated, err := parseResponse([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","encrypted_content":"opaque"},{"type":"function_call","call_id":"partial","name":"list_samples","arguments":"{"}]}`))
	if err != nil || truncated.FinishReason != "length" || len(truncated.ToolCalls) != 0 || truncated.NativePayload == nil {
		t.Fatalf("truncation changed native history or became executable: %+v %v", truncated, err)
	}
	refused, err := parseResponse([]byte(`{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}]}`))
	if err != nil || refused.FinishReason != "refusal" || len(refused.ToolCalls) != 0 {
		t.Fatalf("refusal became executable: %+v %v", refused, err)
	}
}

func TestMAProvider_HTTPClassesDoNotRetryInsideProvider(t *testing.T) {
	for _, status := range []int{401, 429, 500, 503} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			n := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n++
				w.Header().Set("Retry-After", "2")
				w.Header().Set("x-request-id", "req-http")
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"error":{"message":"` + protocolSecret + ` and ` + protocolPrompt + `"}}`))
			}))
			defer server.Close()
			_, err := NewOpenAIProvider(server.URL, protocolSecret, "m", server.Client()).Call(
				context.Background(), []ChatMessage{{Role: RoleUser, Content: protocolPrompt}}, nil, ModelOpts{})
			if n != 1 || !errors.Is(err, ErrOpenAIUpstream) {
				t.Fatalf("calls=%d err=%v", n, err)
			}
			assertNoSecret(t, err)
			var ce *CallError
			if !errors.As(err, &ce) || ce.RequestID != "req-http" {
				t.Fatalf("missing typed HTTP error: %v", err)
			}
			if status == 429 && (!ce.Retryable || ce.Class != ClassRateLimited || ce.RetryAfter != 2*time.Second) {
				t.Fatalf("429 not classified: %+v", ce)
			}
			if status >= 500 && (!ce.Retryable || ce.Class != ClassUpstream) {
				t.Fatalf("5xx not classified: %+v", ce)
			}
			if status == 401 && ce.Retryable {
				t.Fatal("401 was marked retryable")
			}
		})
	}
}

func TestMAProvider_TimeoutIsClassifiedAndCancellable(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	_, err := NewResponsesProvider(server.URL, protocolSecret, "m", server.Client()).Call(
		context.Background(), []ChatMessage{{Role: RoleUser, Content: protocolPrompt}}, nil, ModelOpts{Timeout: 50 * time.Millisecond})
	<-started
	if !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrOpenAIUpstream) {
		t.Fatalf("timeout lost: %v", err)
	}
	var ce *CallError
	if !errors.As(err, &ce) || ce.Class != ClassTimeout || !ce.Retryable {
		t.Fatalf("timeout class missing: %v", err)
	}
	assertNoSecret(t, err)
}

func TestMAProvider_OutputCapIsExplainable(t *testing.T) {
	var saw int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		saw = req.MaxTokens
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer server.Close()
	if _, err := NewOpenAIProvider(server.URL, "k", "m", server.Client()).Call(context.Background(), nil, nil, ModelOpts{}); err != nil {
		t.Fatal(err)
	}
	if saw != DefaultMaxOutputTokens {
		t.Fatalf("default cap = %d", saw)
	}
	_, err := NewOpenAIProvider(server.URL, "k", "m", server.Client()).Call(context.Background(), nil, nil, ModelOpts{MaxOutputTokens: -1})
	if err == nil || !errors.Is(err, ErrProtocol) {
		t.Fatalf("unlimited tokens accepted: %v", err)
	}
	_, err = BoundOutputTokens(HardMaxOutputTokens + 1)
	if err == nil {
		t.Fatal("over-cap accepted")
	}
}

func TestMAProvider_LimiterIsIndependentOfWorkerCount(t *testing.T) {
	limiter := NewRequestLimiter(2)
	started := make(chan struct{}, 4)
	release := make(chan struct{})
	var maxIn, sawQueued atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := limiter.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer limiter.Release()
			snap := limiter.Snapshot()
			for {
				old := maxIn.Load()
				if int32(snap.InFlight) <= old || maxIn.CompareAndSwap(old, int32(snap.InFlight)) {
					break
				}
			}
			if snap.Queued > 0 {
				sawQueued.Store(1)
			}
			started <- struct{}{}
			<-release
		}()
	}
	deadline := time.After(2 * time.Second)
	for limiter.Snapshot().InFlight+limiter.Snapshot().Queued < 4 {
		select {
		case <-deadline:
			t.Fatal("workers did not queue")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	snap := limiter.Snapshot()
	if snap.Limit != 2 || snap.InFlight > 2 {
		t.Fatalf("model limit mixed with worker count: %+v", snap)
	}
	if snap.Queued == 0 && sawQueued.Load() == 0 {
		t.Fatalf("queued was not observable: %+v", snap)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := limiter.Acquire(ctx); err == nil {
		t.Fatal("cancelled acquire waited")
	}
	close(release)
	wg.Wait()
}

func TestMAProvider_RecoveryDoesNotRetryUnclassifiedOrTerminal(t *testing.T) {
	policy := RecoveryPolicy{MaxAttempts: 3}
	if policy.Attempts() != 3 {
		t.Fatal("attempts")
	}
	if policy.ShouldRetry(ErrMockSimulatedFailure) || policy.ShouldRetry(context.Canceled) {
		t.Fatal("unclassified or cancel was retried")
	}
	if policy.ShouldRetry(&CallError{Class: ClassRefusal, Retryable: false}) {
		t.Fatal("refusal retried")
	}
	if !policy.ShouldRetry(&CallError{Class: ClassIncomplete, Retryable: true}) {
		t.Fatal("incomplete output was not retryable")
	}
	if (RecoveryPolicy{}).ShouldRetry(&CallError{Class: ClassRateLimited, Retryable: true}) {
		t.Fatal("default policy hid a retry")
	}
	if policy.Backoff(1, 0) != 500*time.Millisecond || policy.Backoff(5, 0) != 4*time.Second {
		t.Fatalf("backoff = %s / %s", policy.Backoff(1, 0), policy.Backoff(5, 0))
	}
	if policy.Backoff(1, 9*time.Second) != 9*time.Second {
		t.Fatal("Retry-After was ignored")
	}
}

func TestMAProvider_RuntimeResolverAndCallOnce(t *testing.T) {
	historical := ModelAlias{Alias: "historical", Model: "configured-model", Protocol: FormatChatCompletions, ProviderAlias: "historical"}
	worker := ModelAlias{Alias: "worker-profile", Model: "worker-model", Protocol: FormatResponses, ProviderAlias: "historical"}
	agents := contract.AgentsConfig{
		MainAgent: contract.AgentRoleConfig{RuntimeProfile: "historical", Model: "configured-model"},
		Planner:   contract.AgentRoleConfig{RuntimeProfile: "historical", Model: "configured-model"},
		Worker:    contract.AgentRoleConfig{RuntimeProfile: "worker-profile", Model: "worker-model"},
		Verifier:  contract.AgentRoleConfig{RuntimeProfile: "historical", Model: "configured-model"},
	}
	n := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n++
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "previous_response_id") {
			t.Error("adapter stored previous_response_id")
		}
		w.Header().Set("x-request-id", "req-once")
		_, _ = w.Write([]byte(`{"id":"chat","choices":[{"finish_reason":"tool_calls","message":{"tool_calls":[{"id":"call_1","type":"function","function":{"name":"list_samples","arguments":"{"}}]}}]}`))
	}))
	defer server.Close()
	limiter := NewRequestLimiter(2)
	resolver := &StaticRuntimeResolver{
		Historical: historical,
		Aliases:    map[string]ModelAlias{"historical": historical, "configured-model": historical, "worker-profile": worker, "worker-model": worker},
		Version:    1,
		Epoch:      1,
		Caller:     &ContractCaller{Provider: NewOpenAIProvider(server.URL, protocolSecret, historical.Model, server.Client()), Protocol: FormatChatCompletions},
		Policy:     &SharedCallPolicy{Limiter: limiter},
	}
	fallback, err := resolver.Resolve(context.Background(), contract.ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: contract.RolePlanner}, "")
	if err != nil || fallback.Model != "configured-model" || strings.Contains(string(fallback.NonSensitiveSnapshot), protocolSecret) {
		t.Fatalf("historical fallback: %+v %v", fallback, err)
	}
	for _, role := range []contract.Role{contract.RoleMainAgent, contract.RolePlanner, contract.RoleWorker, contract.RoleVerifier} {
		policy, err := RoleCallPolicyFor(role)
		if err != nil || !policy.PauseCancels || !policy.StopCancels || !policy.DeadlineCancels || policy.BudgetSource == "" {
			t.Fatalf("role policy %s: %+v %v", role, policy, err)
		}
	}
	workerItem, workerAttempt := uuid.New(), uuid.New()
	resolved, err := (&StaticRuntimeResolver{Historical: historical, Aliases: resolver.Aliases, Agents: &agents, Version: 1, Epoch: 1}).Resolve(
		context.Background(), contract.ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: contract.RoleWorker, WorkItemID: &workerItem, AttemptID: &workerAttempt}, "")
	if err != nil || resolved.Model != "worker-model" || resolved.Protocol != FormatResponses {
		t.Fatalf("worker model: %+v %v", resolved, err)
	}
	if _, err := resolver.Resolve(context.Background(), contract.ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: contract.RoleWorker}, "missing-alias"); err == nil {
		t.Fatal("unknown alias fell back")
	}
	workID, attemptID := uuid.New(), uuid.New()
	scope := contract.ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: contract.RoleWorker, WorkItemID: &workID, AttemptID: &attemptID}
	_, err = contract.CallOnce(context.Background(), resolver.Policy, resolver.Caller, scope, contract.ModelCallRequest{
		Scope:    scope,
		Messages: []contract.ModelMessage{{Role: "user", Content: protocolPrompt}},
	})
	if n != 1 || err == nil || !errors.Is(err, &contract.Error{Code: contract.CodeInvalidModelOutput}) {
		t.Fatalf("CallOnce calls=%d err=%v", n, err)
	}
	assertNoSecret(t, err)
	if limiter.Snapshot().InFlight != 0 {
		t.Fatalf("lease leaked: %+v", limiter.Snapshot())
	}
}

func TestMAProvider_RestoreDoesNotInventOrReplay(t *testing.T) {
	messages := []ChatMessage{
		{Role: RoleAssistant, ToolCalls: []ToolCall{
			{ID: "done", Type: "function", Function: FunctionCall{Name: "list_samples", Arguments: `{}`}},
			{ID: "open", Type: "function", Function: FunctionCall{Name: "list_samples", Arguments: `{}`}},
		}, NativePayload: &NativePayload{Format: FormatResponses, Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque"}`)}}},
	}
	restored, open := RestoreSavedToolResults(messages, map[string]SavedToolOutcome{
		"done": {CallID: "done", Status: "success", Content: `{"status":"success","recovered_result":true}`},
	})
	if len(open) != 1 || open[0] != "open" || len(restored) != 2 || restored[1].ToolCallID != "done" {
		t.Fatalf("restore changed history: %+v open=%v", restored, open)
	}
	if restored[0].NativePayload == nil {
		t.Fatal("restore dropped native payload")
	}
}
