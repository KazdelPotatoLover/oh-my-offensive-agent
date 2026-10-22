// Package modeldouble is a local Chat Completions and Responses stand-in.
// It never dials the network. Tests use barriers to force session interleaving.
package modeldouble

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const (
	FormatChat      = "chat_completions"
	FormatResponses = "responses"

	KindChatTool           = "chat_tool"
	KindResponsesFunction  = "responses_function"
	KindIncompleteArgs     = "incomplete_arguments"
	KindIncompleteStatus   = "incomplete_status"
	KindStatus             = "http_status"
	KindTimeout            = "timeout"
	KindRefusal            = "refusal"
	KindTruncated          = "truncated"
	KindText               = "text"
)

// Step is one scripted provider response. Match is a substring of the raw body.
// An empty Match is the fallback. Barrier, when set, is entered before the
// response is written so overlapping calls stay in flight.
type Step struct {
	Match      string
	Kind       string
	Name       string
	Arguments  string
	CallID     string
	StatusCode int
	Barrier    string
	Text       string
	Once       bool
}

// Event is one observed provider request. Bodies and credentials are not stored.
type Event struct {
	Seq             int       `json:"seq"`
	Format          string    `json:"format"`
	Path            string    `json:"path"`
	Started         time.Time `json:"started"`
	Ended           time.Time `json:"ended"`
	Status          int       `json:"status"`
	Cancelled       bool      `json:"cancelled"`
	Rule            string    `json:"rule"`
	CallIDs         []string  `json:"call_ids,omitempty"`
	InFlightAtStart int       `json:"in_flight_at_start"`
	SessionKey      string    `json:"session_key"`
	RetryOrdinal    int       `json:"retry_ordinal"`
}

// Dynamic builds a step from the raw request. ok false falls through to Steps.
type Dynamic func(body []byte) (Step, bool)

// Barrier holds callers until Release or the request context ends.
type Barrier struct {
	name string

	mu       sync.Mutex
	arrived  int
	released bool
	waiters  []chan struct{}
	notify   chan struct{}
}

func newBarrier(name string) *Barrier {
	return &Barrier{name: name, notify: make(chan struct{}, 1)}
}

// Enter records arrival and blocks until Release or ctx is done.
func (b *Barrier) Enter(ctx context.Context) error {
	b.mu.Lock()
	b.arrived++
	released := b.released
	ch := make(chan struct{})
	if !released {
		b.waiters = append(b.waiters, ch)
	}
	select {
	case b.notify <- struct{}{}:
	default:
	}
	b.mu.Unlock()
	if released {
		return nil
	}
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Arrived is the number of callers that have entered.
func (b *Barrier) Arrived() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.arrived
}

// Release unblocks every current and future Enter.
func (b *Barrier) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.released {
		return
	}
	b.released = true
	for _, ch := range b.waiters {
		close(ch)
	}
	b.waiters = nil
}

// WaitArrived waits until at least n callers have entered.
func (b *Barrier) WaitArrived(n int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		if b.Arrived() >= n {
			return true
		}
		select {
		case <-deadline.C:
			return b.Arrived() >= n
		case <-b.notify:
		case <-time.After(15 * time.Millisecond):
		}
	}
}

// Server is the local protocol double.
type Server struct {
	httpServer *http.Server
	listener   net.Listener

	mu        sync.Mutex
	steps     []Step
	usedOnce  map[int]bool
	dynamic   Dynamic
	barriers  map[string]*Barrier
	events    []Event
	inFlight  int
	peak      int
	bySession map[string]int
	closed    bool
}

// New binds 127.0.0.1:0 and serves until Close. It has no outbound client.
func New() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{
		listener:  ln,
		usedOnce:  map[int]bool{},
		barriers:  map[string]*Barrier{},
		bySession: map[string]int{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handle)
	s.httpServer = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = s.httpServer.Serve(ln) }()
	return s, nil
}

// BaseURL is the API root, including /v1, with no credentials.
func (s *Server) BaseURL() string {
	return "http://" + s.listener.Addr().String() + "/v1"
}

// SetScript replaces the script. It does not clear the trace.
func (s *Server) SetScript(steps []Step) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.steps = append([]Step(nil), steps...)
	s.usedOnce = map[int]bool{}
}

// SetDynamic installs a body-based step builder.
func (s *Server) SetDynamic(fn Dynamic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dynamic = fn
}

// Barrier returns a stable barrier by name.
func (s *Server) Barrier(name string) *Barrier {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.barriers[name]
	if !ok {
		b = newBarrier(name)
		s.barriers[name] = b
	}
	return b
}

// Peak is the highest number of requests in flight at once.
func (s *Server) Peak() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// InFlight is the current number of requests inside the handler.
func (s *Server) InFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

// Events returns a copy of the request trace.
func (s *Server) Events() []Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Event, len(s.events))
	copy(out, s.events)
	return out
}

// Close stops the listener and releases held barriers.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}
	s.closed = true
	for _, b := range s.barriers {
		b.Release()
	}
	s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.httpServer.Shutdown(ctx)
}

func (s *Server) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/control/trace" {
		s.writeTrace(w)
		return
	}
	format := ""
	switch {
	case strings.HasSuffix(r.URL.Path, "/chat/completions"):
		format = FormatChat
	case strings.HasSuffix(r.URL.Path, "/responses"):
		format = FormatResponses
	default:
		http.NotFound(w, r)
		return
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	step, rule := s.match(body)
	sessionKey := sessionKey(body)
	started := time.Now()

	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	inFlight := s.inFlight
	s.bySession[sessionKey]++
	retryOrdinal := s.bySession[sessionKey]
	s.mu.Unlock()

	status := http.StatusOK
	cancelled := false
	var callIDs []string
	defer func() {
		s.mu.Lock()
		s.inFlight--
		s.events = append(s.events, Event{
			Seq:             len(s.events) + 1,
			Format:          format,
			Path:            r.URL.Path,
			Started:         started,
			Ended:           time.Now(),
			Status:          status,
			Cancelled:       cancelled,
			Rule:            rule,
			CallIDs:         callIDs,
			InFlightAtStart: inFlight,
			SessionKey:      sessionKey,
			RetryOrdinal:    retryOrdinal,
		})
		s.mu.Unlock()
	}()

	if step.Barrier != "" {
		if err := s.Barrier(step.Barrier).Enter(r.Context()); err != nil {
			cancelled = true
			status = 499
			return
		}
	}
	if r.Context().Err() != nil {
		cancelled = true
		status = 499
		return
	}

	switch step.Kind {
	case KindTimeout:
		<-r.Context().Done()
		cancelled = true
		status = 499
		return
	case KindStatus:
		status = step.StatusCode
		if status == 0 {
			status = http.StatusInternalServerError
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"error":{"message":"scripted upstream failure","type":"scripted"}}`))
		return
	}

	callID := step.CallID
	if callID == "" {
		callID = "call_" + uuid.NewString()
	}
	callIDs = []string{callID}
	payload, code := render(format, step, callID)
	status = code
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = w.Write(payload)
}

func (s *Server) match(body []byte) (Step, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.dynamic != nil {
		if step, ok := s.dynamic(body); ok {
			return step, "dynamic"
		}
	}
	text := string(body)
	fallback := -1
	for i, step := range s.steps {
		if step.Once && s.usedOnce[i] {
			continue
		}
		if step.Match == "" {
			if fallback < 0 {
				fallback = i
			}
			continue
		}
		if strings.Contains(text, step.Match) {
			if step.Once {
				s.usedOnce[i] = true
			}
			return step, step.Match
		}
	}
	if fallback >= 0 {
		step := s.steps[fallback]
		if step.Once {
			s.usedOnce[fallback] = true
		}
		return step, "fallback"
	}
	return Step{Kind: KindText, Text: "no script matched"}, "unscripted"
}

func (s *Server) writeTrace(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"peak":      s.Peak(),
		"in_flight": s.InFlight(),
		"events":    s.Events(),
	})
}

func sessionKey(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:8])
}

func render(format string, step Step, callID string) ([]byte, int) {
	switch step.Kind {
	case KindRefusal:
		if format == FormatResponses {
			raw, _ := json.Marshal(map[string]any{
				"id":     "resp_" + callID,
				"status": "completed",
				"output": []any{map[string]any{
					"type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "refusal", "text": "refused"}},
				}},
			})
			return raw, http.StatusOK
		}
		raw, _ := json.Marshal(map[string]any{
			"id": "chatcmpl_" + callID,
			"choices": []any{map[string]any{
				"index":         0,
				"finish_reason": "content_filter",
				"message":       map[string]any{"role": "assistant", "content": "refused"},
			}},
		})
		return raw, http.StatusOK
	case KindTruncated:
		if format == FormatResponses {
			raw, _ := json.Marshal(map[string]any{
				"id":     "resp_" + callID,
				"status": "incomplete",
				"incomplete_details": map[string]any{"reason": "max_output_tokens"},
				"output": []any{map[string]any{
					"type": "function_call", "call_id": callID, "name": step.Name,
					"arguments": step.Arguments, "status": "incomplete",
				}},
			})
			return raw, http.StatusOK
		}
		raw, _ := json.Marshal(map[string]any{
			"id": "chatcmpl_" + callID,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "length",
				"message": map[string]any{"role": "assistant", "content": step.Text},
			}},
		})
		return raw, http.StatusOK
	case KindIncompleteArgs:
		args := step.Arguments
		if args == "" {
			args = `{"items":`
		}
		return functionPayload(format, step.Name, args, callID, "completed", "in_progress")
	case KindIncompleteStatus:
		args := step.Arguments
		if args == "" {
			args = `{}`
		}
		return functionPayload(format, step.Name, args, callID, "completed", "in_progress")
	case KindResponsesFunction, KindChatTool:
		args := step.Arguments
		if args == "" {
			args = `{}`
		}
		return functionPayload(format, step.Name, args, callID, "completed", "completed")
	default:
		text := step.Text
		if text == "" {
			text = "ok"
		}
		if format == FormatResponses {
			raw, _ := json.Marshal(map[string]any{
				"id": "resp_" + callID, "status": "completed",
				"output": []any{map[string]any{
					"type": "message", "role": "assistant", "phase": "final_answer",
					"content": []any{map[string]any{"type": "output_text", "text": text}},
				}},
			})
			return raw, http.StatusOK
		}
		raw, _ := json.Marshal(map[string]any{
			"id": "chatcmpl_" + callID,
			"choices": []any{map[string]any{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": text},
			}},
		})
		return raw, http.StatusOK
	}
}

func functionPayload(format, name, args, callID, responseStatus, functionStatus string) ([]byte, int) {
	if name == "" {
		name = "submit_plan"
	}
	if format == FormatResponses {
		item := map[string]any{
			"type": "function_call", "call_id": callID, "name": name, "arguments": args,
		}
		if functionStatus != "" {
			item["status"] = functionStatus
		}
		raw, _ := json.Marshal(map[string]any{
			"id": "resp_" + callID, "status": responseStatus, "output": []any{item},
		})
		return raw, http.StatusOK
	}
	raw, _ := json.Marshal(map[string]any{
		"id": "chatcmpl_" + callID,
		"choices": []any{map[string]any{
			"index": 0, "finish_reason": "tool_calls",
			"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id": callID, "type": "function",
					"function": map[string]any{"name": name, "arguments": args},
				}},
			},
		}},
	})
	return raw, http.StatusOK
}
