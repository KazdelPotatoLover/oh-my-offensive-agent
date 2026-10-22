package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

const maSecret = "sk-ma2-should-not-leak"
const maPrompt = "private prompt text that must stay out of errors"

func assertSessionErrorRedacted(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		return
	}
	text := err.Error()
	for _, secret := range []string{maSecret, maPrompt, "Authorization", "Bearer " + maSecret} {
		if strings.Contains(text, secret) {
			t.Fatalf("error leaked secret material: %v", err)
		}
	}
}

type countingTool struct {
	hits atomic.Int32
}

func (t *countingTool) Name() string { return "probe_external" }
func (t *countingTool) Description() string {
	return "counts external executions"
}
func (t *countingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *countingTool) Validate([]byte) error { return nil }
func (t *countingTool) Execute(context.Context, []byte) (string, error) {
	n := t.hits.Add(1)
	return fmt.Sprintf(`{"status":"success","executed":%d}`, n), nil
}

func maBudget() model.BudgetConfig {
	budget := model.DefaultBudgetConfig()
	budget.ModelTimeoutSeconds = 5
	budget.ToolTimeoutSeconds = 2
	return budget
}

func maRunSession(t *testing.T, env *TestEnv, role string) (uuid.UUID, uuid.UUID) {
	t.Helper()
	task := createTestTask(t, env, "ma-provider "+role)
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	sess, err := env.runService.CreateSession(context.Background(), run.ID, role, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return run.ID, sess.ID
}

type capturedServer struct {
	calls  atomic.Int32
	mu     sync.Mutex
	bodies []string
	server *httptest.Server
}

func (c *capturedServer) count() int { return int(c.calls.Load()) }

func (c *capturedServer) snapshot() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, len(c.bodies))
	copy(out, c.bodies)
	return out
}

type stubStep struct {
	status     int
	retryAfter string
	body       string
}

func newStubServer(steps []stubStep) *capturedServer {
	captured := &capturedServer{}
	captured.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		captured.mu.Lock()
		captured.bodies = append(captured.bodies, string(raw))
		index := int(captured.calls.Add(1)) - 1
		captured.mu.Unlock()
		if index >= len(steps) {
			index = len(steps) - 1
		}
		step := steps[index]
		w.Header().Set("x-request-id", fmt.Sprintf("req-%d", index+1))
		if step.retryAfter != "" {
			w.Header().Set("Retry-After", step.retryAfter)
		}
		if step.status != 0 {
			w.WriteHeader(step.status)
		}
		_, _ = w.Write([]byte(step.body))
	}))
	return captured
}

func chatFinal(text string) string {
	return fmt.Sprintf(`{"id":"chat_final","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":%q}}]}`, text)
}

func TestMAProviderSession_ConcurrentSessionsStayIsolated(t *testing.T) {
	for _, protocol := range []string{provider.FormatChatCompletions, provider.FormatResponses} {
		t.Run(protocol, func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			task := createTestTask(t, env, "isolation "+protocol)
			run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
			if err != nil {
				t.Fatal(err)
			}
			labels := []string{"sessA", "sessB", "sessC", "sessD"}
			sessions := make([]uuid.UUID, len(labels))
			for i := range labels {
				sess, err := env.runService.CreateSession(context.Background(), run.ID, "worker", nil, nil, nil, nil)
				if err != nil {
					t.Fatal(err)
				}
				sessions[i] = sess.ID
			}
			var firstCount atomic.Int32
			allFirst := make(chan struct{})
			var once sync.Once
			var crossed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				body := string(raw)
				label := ""
				for _, candidate := range labels {
					if strings.Contains(body, candidate) {
						if label != "" {
							crossed.Store(true)
							t.Errorf("request mixed %s and %s", label, candidate)
						}
						label = candidate
					}
				}
				if label == "" {
					t.Error("request lost its session label")
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				first := !strings.Contains(body, "function_call_output") && !strings.Contains(body, `"role":"tool"`)
				if first {
					if firstCount.Add(1) == int32(len(labels)) {
						once.Do(func() { close(allFirst) })
					}
					select {
					case <-allFirst:
					case <-time.After(3 * time.Second):
						t.Error("sessions did not interleave")
						w.WriteHeader(http.StatusGatewayTimeout)
						return
					}
					w.Header().Set("x-request-id", "http_"+label)
					if protocol == provider.FormatResponses {
						_, _ = fmt.Fprintf(w, `{"id":"resp_%s","status":"completed","output":[{"type":"reasoning","encrypted_content":"opaque_%s"},{"type":"function_call","call_id":"call_shared","name":"list_samples","arguments":"{}","status":"completed"}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`, label, label)
						return
					}
					_, _ = fmt.Fprintf(w, `{"id":"chat_%s","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_shared","type":"function","function":{"name":"list_samples","arguments":"{}"}}]}}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`, label)
					return
				}
				for _, other := range labels {
					if other == label {
						continue
					}
					if strings.Contains(body, other) || strings.Contains(body, "opaque_"+other) {
						crossed.Store(true)
						t.Errorf("%s history contained %s", label, other)
					}
				}
				if !strings.Contains(body, "call_shared") || !strings.Contains(body, "sample_home") {
					t.Errorf("%s lost tool history", label)
				}
				if protocol == provider.FormatResponses && !strings.Contains(body, "opaque_"+label) {
					t.Errorf("%s dropped native reasoning", label)
				}
				w.Header().Set("x-request-id", "http_final_"+label)
				if protocol == provider.FormatResponses {
					_, _ = fmt.Fprintf(w, `{"id":"resp_final_%s","status":"completed","output":[{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"Done %s"}]}]}`, label, label)
					return
				}
				_, _ = w.Write([]byte(chatFinal("Done " + label)))
			}))
			defer server.Close()
			var p provider.Provider
			if protocol == provider.FormatResponses {
				p = provider.NewResponsesProvider(server.URL, maSecret, "m", server.Client())
			} else {
				p = provider.NewOpenAIProvider(server.URL, maSecret, "m", server.Client())
			}
			reg := tools.NewRegistry()
			reg.Register(tools.NewListSamplesTool())
			var wg sync.WaitGroup
			for i, sessID := range sessions {
				wg.Add(1)
				go func(i int, sessID uuid.UUID) {
					defer wg.Done()
					sr := agent.NewSessionRunner(sessID, run.ID, "worker", env.runService, p, reg, maBudget())
					res, err := sr.Run(context.Background(), []provider.ChatMessage{
						{Role: provider.RoleSystem, Content: "Test"},
						{Role: provider.RoleUser, Content: labels[i]},
					})
					if err != nil || res == nil || res.FinishReason != "completed" || res.ToolCallsCount != 1 {
						t.Errorf("%s result=%+v err=%v", labels[i], res, err)
					}
				}(i, sessID)
			}
			wg.Wait()
			if crossed.Load() {
				t.Fatal("same call_id crossed sessions")
			}
			calls, err := env.runService.ListModelCalls(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(calls) != 8 {
				t.Fatalf("expected 8 model calls, got %d", len(calls))
			}
			perSession := map[uuid.UUID]int{}
			for _, call := range calls {
				if call.SessionID == nil || call.Status != "completed" {
					t.Fatalf("model call missing session or not completed: %+v", call)
				}
				perSession[*call.SessionID]++
			}
			toolCalls, err := env.runService.ListAgentToolCalls(context.Background(), run.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(toolCalls) != 4 {
				t.Fatalf("expected 4 isolated tool calls, got %d", len(toolCalls))
			}
			seenSession := map[uuid.UUID]bool{}
			for _, tc := range toolCalls {
				if tc.CallID != "call_shared" || tc.SessionID == nil || tc.Status != "success" {
					t.Fatalf("tool call was not isolated: %+v", tc)
				}
				if seenSession[*tc.SessionID] {
					t.Fatal("one session recorded the shared call_id twice")
				}
				seenSession[*tc.SessionID] = true
			}
			for _, sessID := range sessions {
				if perSession[sessID] != 2 {
					t.Fatalf("session %s calls=%d", sessID, perSession[sessID])
				}
				records, err := env.runService.ListAgentMessagesBySession(context.Background(), sessID)
				if err != nil {
					t.Fatal(err)
				}
				if protocol == provider.FormatResponses {
					foundNative := false
					for _, record := range records {
						if len(record.ProviderPayload) > 0 && bytesContains(record.ProviderPayload, "opaque_") {
							foundNative = true
						}
					}
					if !foundNative {
						t.Fatal("native payload was not persisted")
					}
				}
			}
		})
	}
}

func bytesContains(raw []byte, part string) bool {
	return strings.Contains(string(raw), part)
}

func TestMAProviderSession_ModelLimitIsSeparateFromWorkerCount(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	task := createTestTask(t, env, "model limit")
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	limiter := provider.NewRequestLimiter(2)
	release := make(chan struct{})
	var entered atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered.Add(1)
		<-release
		_, _ = w.Write([]byte(chatFinal("ok")))
	}))
	defer server.Close()
	p := provider.NewOpenAIProvider(server.URL, maSecret, "m", server.Client())
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		sess, err := env.runService.CreateSession(context.Background(), run.ID, "worker", nil, nil, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(sessID uuid.UUID) {
			defer wg.Done()
			sr := agent.NewSessionRunner(sessID, run.ID, "worker", env.runService, p, tools.NewRegistry(), maBudget(), agent.WithRequestLimiter(limiter))
			res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: "go"}})
			if err != nil || res == nil || res.FinishReason != "completed" || res.ModelCallsCount != 1 {
				t.Errorf("limited session result=%+v err=%v", res, err)
			}
		}(sess.ID)
	}
	deadline := time.Now().Add(3 * time.Second)
	var snap providerSnapshot
	for time.Now().Before(deadline) {
		current := limiter.Snapshot()
		snap = providerSnapshot{current.Limit, current.InFlight, current.Queued}
		if current.InFlight+current.Queued >= 4 && current.Queued > 0 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if snap.limit != 2 || snap.inflight > 2 || snap.queued == 0 {
		t.Fatalf("N=4 did not observe model limit 2: %+v entered=%d", snap, entered.Load())
	}
	close(release)
	wg.Wait()
	if limiter.Snapshot().InFlight != 0 {
		t.Fatalf("limiter leaked: %+v", limiter.Snapshot())
	}
}

type providerSnapshot struct {
	limit, inflight, queued int
}

func TestMAProviderSession_CancelWhileQueued(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	runID, sessA := maRunSession(t, env, "worker")
	sessB, err := env.runService.CreateSession(context.Background(), runID, "planner", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	limiter := provider.NewRequestLimiter(1)
	release := make(chan struct{})
	entered := make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		close(entered)
		<-release
		_, _ = w.Write([]byte(chatFinal("ok")))
	}))
	defer server.Close()
	p := provider.NewOpenAIProvider(server.URL, maSecret, "m", server.Client())
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		sr := agent.NewSessionRunner(sessA, runID, "worker", env.runService, p, tools.NewRegistry(), maBudget(), agent.WithRequestLimiter(limiter))
		_, _ = sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: "hold"}})
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("first session did not acquire")
	}
	ctxB, cancelB := context.WithCancel(context.Background())
	defer cancelB()
	resultB := make(chan *agent.SessionResult, 1)
	errB := make(chan error, 1)
	go func() {
		sr := agent.NewSessionRunner(sessB.ID, runID, "planner", env.runService, p, tools.NewRegistry(), maBudget(), agent.WithRequestLimiter(limiter))
		res, err := sr.Run(ctxB, []provider.ChatMessage{{Role: provider.RoleUser, Content: maPrompt}})
		resultB <- res
		errB <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for limiter.Snapshot().Queued == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if limiter.Snapshot().Queued == 0 {
		t.Fatal("second session was not queued")
	}
	cancelB()
	select {
	case res := <-resultB:
		err := <-errB
		if res == nil || res.FinishReason != "user_requested_stop" || !errors.Is(err, context.Canceled) {
			t.Fatalf("queued cancel result=%+v err=%v", res, err)
		}
		assertSessionErrorRedacted(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("queued cancel did not return")
	}
	if calls.Load() != 1 {
		t.Fatalf("queued session sent a model request: %d", calls.Load())
	}
	close(release)
	<-doneA
}

func TestMAProviderSession_CancelDuringBackoff(t *testing.T) {
	for _, mode := range []string{"user_stop", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			env := setupTestEnv(t)
			defer env.teardown()
			runID, sessID := maRunSession(t, env, "planner")
			started := make(chan struct{})
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				close(started)
				w.Header().Set("Retry-After", "30")
				w.Header().Set("x-request-id", "req-429")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"error":{"message":"` + maSecret + ` and ` + maPrompt + `"}}`))
			}))
			defer server.Close()
			ctx, cancel := context.WithCancelCause(context.Background())
			defer cancel(model.CauseShutdown)
			go func() {
				<-started
				if mode == "deadline" {
					cancel(model.CauseTimeout)
					return
				}
				cancel(context.Canceled)
			}()
			p := provider.NewOpenAIProvider(server.URL, maSecret, "m", server.Client())
			sr := agent.NewSessionRunner(sessID, runID, "planner", env.runService, p, tools.NewRegistry(), maBudget(),
				agent.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 3}))
			res, err := sr.Run(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: maPrompt}})
			want := "user_requested_stop"
			if mode == "deadline" {
				want = "timeout"
			}
			if res == nil || res.FinishReason != want || calls.Load() != 1 {
				t.Fatalf("backoff cancel result=%+v calls=%d err=%v", res, calls.Load(), err)
			}
			assertSessionErrorRedacted(t, err)
			modelCalls, err := env.runService.ListModelCalls(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if len(modelCalls) != 1 || modelCalls[0].ErrorMessage == nil || strings.Contains(*modelCalls[0].ErrorMessage, maSecret) {
				t.Fatalf("model call diagnosis leaked or was missing: %+v", modelCalls)
			}
		})
	}
}

func TestMAProviderSession_RetryStopsAtBudgetAndKeepsClassifiedError(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	runID, sessID := maRunSession(t, env, "worker")
	stub := newStubServer([]stubStep{{
		status: http.StatusTooManyRequests,
		body:   `{"error":{"message":"` + maSecret + `"}}`,
	}})
	defer stub.server.Close()
	budget := maBudget()
	budget.MaxModelCalls = 2
	p := provider.NewOpenAIProvider(stub.server.URL, maSecret, "m", stub.server.Client())
	sr := agent.NewSessionRunner(sessID, runID, "worker", env.runService, p, tools.NewRegistry(), budget,
		agent.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 5, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond}))
	res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: maPrompt}})
	if stub.count() != 2 || res == nil || res.FinishReason != "provider_error" || res.FinishReason == "model_calls_exhausted" {
		t.Fatalf("budget rewrote the classified error: calls=%d result=%+v err=%v", stub.count(), res, err)
	}
	if !strings.Contains(err.Error(), "HTTP 429") {
		t.Fatalf("lost upstream status: %v", err)
	}
	assertSessionErrorRedacted(t, err)
	calls, err := env.runService.ListModelCalls(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected two accounted calls, got %d", len(calls))
	}
	for _, call := range calls {
		if call.Status != "failed" || call.ErrorMessage == nil || strings.Contains(*call.ErrorMessage, maSecret) {
			t.Fatalf("call record: %+v", call)
		}
	}
}

func TestMAProviderSession_UpstreamRetryThenSuccess(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	runID, sessID := maRunSession(t, env, "worker")
	stub := newStubServer([]stubStep{
		{status: http.StatusServiceUnavailable, body: `{"error":{"message":"unavailable"}}`},
		{status: http.StatusOK, body: chatFinal("recovered")},
	})
	defer stub.server.Close()
	p := provider.NewOpenAIProvider(stub.server.URL, maSecret, "m", stub.server.Client())
	sr := agent.NewSessionRunner(sessID, runID, "worker", env.runService, p, tools.NewRegistry(), maBudget(),
		agent.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 3, InitialBackoff: 5 * time.Millisecond, MaxBackoff: 10 * time.Millisecond}))
	res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: "go"}})
	if err != nil || res == nil || res.FinishReason != "completed" || res.FinalContent != "recovered" || stub.count() != 2 {
		t.Fatalf("5xx retry result=%+v calls=%d err=%v", res, stub.count(), err)
	}
	calls, err := env.runService.ListModelCalls(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 || calls[0].Status != "failed" || calls[1].Status != "completed" {
		t.Fatalf("model call ledger: %+v", calls)
	}
}

func TestMAProviderSession_ClientErrorIsNotRetried(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	runID, sessID := maRunSession(t, env, "mainagent")
	stub := newStubServer([]stubStep{{status: http.StatusUnauthorized, body: `{"error":{"message":"Invalid API key provided: ` + maSecret + `"}}`}})
	defer stub.server.Close()
	p := provider.NewOpenAIProvider(stub.server.URL, maSecret, "m", stub.server.Client())
	sr := agent.NewSessionRunner(sessID, runID, "mainagent", env.runService, p, tools.NewRegistry(), maBudget(),
		agent.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond}))
	res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: maPrompt}})
	if stub.count() != 1 || res == nil || res.FinishReason != "provider_error" {
		t.Fatalf("401 was retried or misclassified: calls=%d result=%+v err=%v", stub.count(), res, err)
	}
	assertSessionErrorRedacted(t, err)
}

func TestMAProviderSession_BadOutputIsNotExecutable(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	tool := &countingTool{}
	reg := tools.NewRegistry()
	reg.Register(tool)
	cases := []struct {
		name string
		body string
	}{
		{"bad_json", `{"id":"chat","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"probe_external","arguments":"{"}}]}}]}`},
		{"duplicate", `{"status":"completed","output":[{"type":"function_call","call_id":"same","name":"probe_external","arguments":"{}"},{"type":"function_call","call_id":"same","name":"probe_external","arguments":"{}"}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runID, sessID := maRunSession(t, env, "worker")
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("x-request-id", "req-bad")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			var p provider.Provider = provider.NewOpenAIProvider(server.URL, maSecret, "m", server.Client())
			if tc.name == "duplicate" {
				p = provider.NewResponsesProvider(server.URL, maSecret, "m", server.Client())
			}
			sr := agent.NewSessionRunner(sessID, runID, "worker", env.runService, p, reg, maBudget(),
				agent.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond}))
			res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: maPrompt}})
			if calls.Load() != 2 || res == nil || res.FinishReason != "protocol_error" || tool.hits.Load() != 0 {
				t.Fatalf("bad output became executable: calls=%d hits=%d result=%+v err=%v", calls.Load(), tool.hits.Load(), res, err)
			}
			assertSessionErrorRedacted(t, err)
			recorded, err := env.runService.ListAgentToolCalls(context.Background(), runID)
			if err != nil {
				t.Fatal(err)
			}
			if len(recorded) != 0 {
				t.Fatalf("bad output created tool calls: %+v", recorded)
			}
		})
	}
}

func TestMAProviderSession_TruncationAndRefusalDoNotExecuteTools(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	tool := &countingTool{}
	reg := tools.NewRegistry()
	reg.Register(tool)
	cases := []struct {
		name, body, reason string
		responses          bool
	}{
		{"truncated", `{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"reasoning","encrypted_content":"opaque_keep"},{"type":"function_call","call_id":"partial","name":"probe_external","arguments":"{"}]}`, "model_output_truncated", true},
		{"refusal", `{"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"refusal","refusal":"No"}]}]}`, "model_refused", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := tool.hits.Load()
			runID, sessID := maRunSession(t, env, "worker")
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			p := provider.NewResponsesProvider(server.URL, maSecret, "m", server.Client())
			sr := agent.NewSessionRunner(sessID, runID, "worker", env.runService, p, reg, maBudget(),
				agent.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 3}))
			res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: "go"}})
			if err != nil || res == nil || res.FinishReason != tc.reason || tool.hits.Load() != before {
				t.Fatalf("terminal output executed a tool: result=%+v hits=%d err=%v", res, tool.hits.Load(), err)
			}
			if tc.responses && tc.name == "truncated" {
				records, err := env.runService.ListAgentMessagesBySession(context.Background(), sessID)
				if err != nil {
					t.Fatal(err)
				}
				kept := false
				for _, record := range records {
					if bytesContains(record.ProviderPayload, "opaque_keep") {
						kept = true
					}
				}
				if !kept {
					t.Fatal("truncation dropped native payload")
				}
			}
		})
	}
}

func TestMAProviderSession_ChildTimeoutIsClassified(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	runID, sessID := maRunSession(t, env, "worker")
	var calls atomic.Int32
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer server.Close()
	defer close(release)
	budget := maBudget()
	budget.ModelTimeoutSeconds = 1
	p := provider.NewResponsesProvider(server.URL, maSecret, "m", server.Client())
	sr := agent.NewSessionRunner(sessID, runID, "worker", env.runService, p, tools.NewRegistry(), budget)
	res, err := sr.Run(context.Background(), []provider.ChatMessage{{Role: provider.RoleUser, Content: maPrompt}})
	if calls.Load() != 1 || res == nil || res.FinishReason != "provider_error" || !strings.Contains(err.Error(), "upstream_timeout") {
		t.Fatalf("child timeout result=%+v calls=%d err=%v", res, calls.Load(), err)
	}
	assertSessionErrorRedacted(t, err)
}

func TestMAProviderSession_InterruptedBatchDoesNotReplay(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	tool := &countingTool{}
	reg := tools.NewRegistry()
	reg.Register(tool)
	runID, sessID := maRunSession(t, env, "worker")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"chat","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_a","type":"function","function":{"name":"probe_external","arguments":"{}"}},{"id":"call_b","type":"function","function":{"name":"probe_external","arguments":"{}"}}]}}]}`))
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var started atomic.Int32
	p := provider.NewOpenAIProvider(server.URL, maSecret, "m", server.Client())
	sr := agent.NewSessionRunner(sessID, runID, "worker", env.runService, p, reg, maBudget(), agent.WithBeforeToolHook(func(hookCtx context.Context, name string) error {
		if started.Add(1) > 1 {
			cancel()
			return hookCtx.Err()
		}
		return nil
	}))
	res, err := sr.Run(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: "batch"}})
	if tool.hits.Load() != 1 || res == nil || res.FinishReason != "user_requested_stop" {
		t.Fatalf("batch interrupt result=%+v hits=%d err=%v", res, tool.hits.Load(), err)
	}
	records, err := env.runService.ListAgentMessagesBySession(context.Background(), sessID)
	if err != nil {
		t.Fatal(err)
	}
	var assistant provider.ChatMessage
	for _, record := range records {
		if record.Role == "assistant" && len(record.ToolCalls) > 0 {
			var calls []provider.ToolCall
			if err := json.Unmarshal(record.ToolCalls, &calls); err != nil {
				t.Fatal(err)
			}
			assistant = provider.ChatMessage{Role: provider.RoleAssistant, ToolCalls: calls}
		}
	}
	if len(assistant.ToolCalls) != 2 {
		t.Fatalf("assistant batch was not persisted: %+v", records)
	}
	recoveredRun, recoveredSess := maRunSession(t, env, "worker")
	var sawSaved atomic.Bool
	recovery := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "recovered_result") && strings.Contains(string(raw), "call_a") {
			sawSaved.Store(true)
		}
		_, _ = w.Write([]byte(chatFinal("continued")))
	}))
	defer recovery.Close()
	recoveryProvider := provider.NewOpenAIProvider(recovery.URL, maSecret, "m", recovery.Client())
	recoveryRunner := agent.NewSessionRunner(recoveredSess, recoveredRun, "worker", env.runService, recoveryProvider, reg, maBudget(),
		agent.WithSavedOutcomes(map[string]provider.SavedToolOutcome{
			"call_a": {CallID: "call_a", Status: "success", Content: `{"status":"success","recovered_result":true}`},
		}))
	before := tool.hits.Load()
	recovered, err := recoveryRunner.Run(context.Background(), []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "batch"},
		assistant,
	})
	if err != nil || recovered == nil || recovered.FinishReason != "completed" || tool.hits.Load() != before || !sawSaved.Load() {
		t.Fatalf("recovery replayed an external action: result=%+v hits=%d saw=%v err=%v", recovered, tool.hits.Load(), sawSaved.Load(), err)
	}
	var notReplayed int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM run_events WHERE session_id=$1 AND type='tool_call_not_replayed'`, recoveredSess).Scan(&notReplayed); err != nil {
		t.Fatal(err)
	}
	if notReplayed != 1 {
		t.Fatalf("open call was replayed or dropped silently: %d", notReplayed)
	}
}
