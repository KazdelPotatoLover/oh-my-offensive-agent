package provider

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultMaxOutputTokens = 4096
	HardMaxOutputTokens    = 8192
)

// ProtocolClass separates upstream availability from model-output defects.
// A protocol defect is not evidence of a worker race.
type ProtocolClass string

const (
	ClassIncomplete  ProtocolClass = "incomplete_output"
	ClassBadSchema   ProtocolClass = "bad_schema"
	ClassDuplicate   ProtocolClass = "duplicate_call_id"
	ClassMissingCall ProtocolClass = "missing_call_id"
	ClassRefusal     ProtocolClass = "refusal"
	ClassTruncated   ProtocolClass = "truncated"
	ClassRateLimited ProtocolClass = "upstream_rate_limited"
	ClassUpstream    ProtocolClass = "upstream_http"
	ClassClientHTTP  ProtocolClass = "upstream_client_http"
	ClassTimeout     ProtocolClass = "upstream_timeout"
	ClassTransport   ProtocolClass = "upstream_transport"
	ClassOutputLimit ProtocolClass = "output_limit"
)

// CallError is the redacted diagnosis of one Provider.Call. It never contains
// credentials, Authorization headers, or prompt text. DiagnosticRef is a hash
// of the raw body so an operator can match a controlled fixture out of band.
type CallError struct {
	Class             ProtocolClass
	HTTPStatus        int
	Retryable         bool
	RetryAfter        time.Duration
	RequestID         string
	SourceModelCallID string
	DiagnosticRef     string
	Summary           string
	unwrap            []error
}

func (e *CallError) Error() string {
	if e == nil {
		return ""
	}
	return e.Summary
}

func (e *CallError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return e.unwrap
}

// AnnotateSource records the model_calls row that observed this failure.
func AnnotateSource(err error, modelCallID string) error {
	var ce *CallError
	if modelCallID == "" || !errors.As(err, &ce) {
		return err
	}
	cp := *ce
	cp.SourceModelCallID = modelCallID
	if !strings.Contains(cp.Summary, "source_model_call=") {
		cp.Summary += " source_model_call=" + modelCallID
	}
	return &cp
}

func BoundOutputTokens(requested int) (int, error) {
	if requested == 0 {
		return DefaultMaxOutputTokens, nil
	}
	if requested < 0 || requested > HardMaxOutputTokens {
		return 0, &CallError{
			Class:     ClassOutputLimit,
			Retryable: false,
			Summary:   fmt.Sprintf("%s: max output tokens %d is outside 1..%d", ErrProtocol.Error(), requested, HardMaxOutputTokens),
			unwrap:    []error{ErrProtocol},
		}
	}
	return requested, nil
}

// NormalizeArguments accepts a JSON string or a complete object/array.
// It does not invent "{}", substitute an item id, or repair truncated JSON.
func NormalizeArguments(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", incompleteArguments()
	}
	if trimmed[0] == '"' {
		var decoded string
		if err := json.Unmarshal(trimmed, &decoded); err != nil || !completeJSONValue([]byte(decoded)) {
			return "", incompleteArguments()
		}
		return decoded, nil
	}
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return "", incompleteArguments()
	}
	if !completeJSONValue(trimmed) {
		return "", incompleteArguments()
	}
	var value any
	if err := json.Unmarshal(trimmed, &value); err != nil {
		return "", incompleteArguments()
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return "", incompleteArguments()
	}
	return string(compact), nil
}

func incompleteArguments() error {
	return &CallError{
		Class:     ClassIncomplete,
		Retryable: true,
		Summary:   ErrProtocol.Error() + ": incomplete function arguments or status",
		unwrap:    []error{ErrProtocol},
	}
}

func completeJSONValue(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	var value any
	if err := dec.Decode(&value); err != nil {
		return false
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return false
	}
	return true
}

func callStatusAllowed(status string) bool {
	return status == "" || status == "completed"
}

type parsedFunctionCall struct {
	ID        string
	Name      string
	Arguments string
	Status    string
	ArgsOK    bool
}

// ExecutableToolCalls returns a batch only when every call is complete.
// A later incomplete or duplicate call drops the whole batch.
func ExecutableToolCalls(calls []parsedFunctionCall) ([]ToolCall, error) {
	seen := map[string]bool{}
	out := make([]ToolCall, 0, len(calls))
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == "" || strings.TrimSpace(call.Name) == "" {
			return nil, &CallError{
				Class:     ClassMissingCall,
				Retryable: true,
				Summary:   ErrProtocol.Error() + ": missing or duplicate function call_id/name",
				unwrap:    []error{ErrProtocol},
			}
		}
		if seen[call.ID] {
			return nil, &CallError{
				Class:     ClassDuplicate,
				Retryable: true,
				Summary:   ErrProtocol.Error() + ": missing or duplicate function call_id/name",
				unwrap:    []error{ErrProtocol},
			}
		}
		seen[call.ID] = true
		if !call.ArgsOK || !callStatusAllowed(call.Status) {
			return nil, incompleteArguments()
		}
		out = append(out, ToolCall{
			ID:   call.ID,
			Type: "function",
			Function: FunctionCall{
				Name:      call.Name,
				Arguments: call.Arguments,
			},
		})
	}
	return out, nil
}

func protocolCallError(class ProtocolClass, summary string, retryable bool) *CallError {
	return &CallError{
		Class:     class,
		Retryable: retryable,
		Summary:   ErrProtocol.Error() + ": " + summary,
		unwrap:    []error{ErrProtocol},
	}
}

func diagnosticRef(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func annotateDiagnosis(err error, status int, requestID string, raw []byte) error {
	if err == nil {
		return nil
	}
	var ce *CallError
	if !errors.As(err, &ce) {
		return err
	}
	cp := *ce
	if cp.HTTPStatus == 0 {
		cp.HTTPStatus = status
	}
	if cp.RequestID == "" {
		cp.RequestID = boundRequestID(requestID)
	}
	if cp.DiagnosticRef == "" {
		cp.DiagnosticRef = diagnosticRef(raw)
	}
	cp.Summary = withDiagnosis(cp.Summary, cp)
	return &cp
}

func withDiagnosis(summary string, ce CallError) string {
	if ce.Class != "" && !strings.Contains(summary, "class=") {
		summary += " class=" + string(ce.Class)
	}
	if ce.HTTPStatus != 0 && !strings.Contains(summary, "HTTP ") {
		summary += fmt.Sprintf(" HTTP %d", ce.HTTPStatus)
	}
	if ce.RequestID != "" && !strings.Contains(summary, "request_id=") {
		summary += " request_id=" + ce.RequestID
	}
	if ce.DiagnosticRef != "" && !strings.Contains(summary, "diagnostic_ref=") {
		summary += " diagnostic_ref=" + ce.DiagnosticRef
	}
	if ce.RetryAfter > 0 && !strings.Contains(summary, "retry_after=") {
		summary += " retry_after=" + ce.RetryAfter.String()
	}
	return summary
}

func httpCallError(status int, requestID, retryAfterHeader string, raw []byte, secrets []string) *CallError {
	class := ClassClientHTTP
	retryable := false
	if status == http.StatusTooManyRequests {
		class = ClassRateLimited
		retryable = true
	} else if status >= 500 {
		class = ClassUpstream
		retryable = true
	}
	summary := fmt.Sprintf("%s: HTTP %d", ErrOpenAIUpstream.Error(), status)
	if detail := safeUpstreamDetail(extractUpstreamMessage(raw), secrets); detail != "" {
		summary += ": " + detail
	}
	ce := &CallError{
		Class:         class,
		HTTPStatus:    status,
		Retryable:     retryable,
		RetryAfter:    parseRetryAfter(retryAfterHeader, time.Now()),
		RequestID:     boundRequestID(requestID),
		DiagnosticRef: diagnosticRef(raw),
		unwrap:        []error{ErrOpenAIUpstream},
	}
	ce.Summary = withDiagnosis(summary, *ce)
	return ce
}

func transportCallError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		ce := &CallError{
			Class:     ClassTimeout,
			Retryable: true,
			unwrap:    []error{ErrOpenAIUpstream, context.DeadlineExceeded},
		}
		ce.Summary = withDiagnosis(ErrOpenAIUpstream.Error()+": upstream timeout", *ce)
		return ce
	}
	ce := &CallError{
		Class:     ClassTransport,
		Retryable: true,
		unwrap:    []error{ErrOpenAIUpstream},
	}
	ce.Summary = withDiagnosis(ErrOpenAIUpstream.Error()+": temporary upstream transport failure", *ce)
	return ce
}

func extractUpstreamMessage(raw []byte) string {
	var body struct {
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ""
	}
	if body.Error != nil && body.Error.Message != "" {
		return body.Error.Message
	}
	return body.Message
}

func safeUpstreamDetail(message string, secrets []string) string {
	message = strings.TrimSpace(message)
	if message == "" || len(message) > 180 || strings.ContainsAny(message, "\r\n") {
		return ""
	}
	lower := strings.ToLower(message)
	if strings.Contains(lower, "authorization") || strings.Contains(lower, "bearer ") || strings.Contains(lower, "sk-") {
		return ""
	}
	for _, secret := range secrets {
		if len(secret) >= 4 && strings.Contains(message, secret) {
			return ""
		}
	}
	return message
}

func messageSecrets(apiKey string, messages []ChatMessage) []string {
	secrets := make([]string, 0, len(messages)+1)
	if apiKey != "" {
		secrets = append(secrets, apiKey)
	}
	for _, message := range messages {
		if message.Content != "" {
			secrets = append(secrets, message.Content)
		}
	}
	return secrets
}

func parseRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(header); err == nil && seconds >= 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(header); err == nil {
		delay := when.Sub(now)
		if delay < 0 {
			return 0
		}
		return delay
	}
	return 0
}

func boundRequestID(id string) string {
	if len(id) > 255 {
		return id[:255]
	}
	return id
}

func readLimitedBody(body io.Reader) ([]byte, error) {
	const maxBody = 16 << 20
	raw, err := io.ReadAll(io.LimitReader(body, maxBody+1))
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}
	if len(raw) > maxBody {
		return nil, protocolCallError(ClassBadSchema, "response body exceeds limit", false)
	}
	return raw, nil
}
