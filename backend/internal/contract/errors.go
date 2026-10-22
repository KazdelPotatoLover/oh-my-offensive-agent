package contract

import (
	"errors"
	"net/http"
)

// ErrorCode values are stable API and tool-result codes. UI copy is not a code.
type ErrorCode string

const (
	CodeInvalidInput          ErrorCode = "invalid_input"
	CodeInvalidModelOutput    ErrorCode = "invalid_model_output"
	CodeWorkerExhausted       ErrorCode = "worker_exhausted"
	CodeRunBudgetExhausted    ErrorCode = "run_budget_exhausted"
	CodeUpstreamRateLimited   ErrorCode = "upstream_rate_limited"
	CodeCredentialUnavailable ErrorCode = "credential_unavailable"
	CodeScopeDenied           ErrorCode = "scope_denied"
	CodeExecutionUnavailable  ErrorCode = "execution_unavailable"
	CodePersistenceError      ErrorCode = "persistence_error"
	CodeConcurrencyOutOfRange ErrorCode = "concurrency_out_of_range"
	CodeIdempotencyConflict   ErrorCode = "idempotency_conflict"
	CodeStaleEpoch            ErrorCode = "stale_epoch"
	CodeInvalidState          ErrorCode = "invalid_state"
	CodeNotFound              ErrorCode = "not_found"
	CodePayloadTooLarge       ErrorCode = "payload_too_large"
	CodeUnauthorized          ErrorCode = "unauthorized"
	CodeNotImplemented        ErrorCode = "not_implemented"
)

func (c ErrorCode) String() string { return string(c) }

// DefaultHTTPStatus is used only when this code is an API error. A run finish
// reason or tool result carries the same code on its own successful read.
// An unmet natural-language goal is not an HTTP 500.
func (c ErrorCode) DefaultHTTPStatus() int {
	switch c {
	case CodeInvalidInput, CodeConcurrencyOutOfRange, CodeInvalidModelOutput:
		return http.StatusBadRequest
	case CodeUnauthorized, CodeScopeDenied, CodeCredentialUnavailable:
		return http.StatusForbidden
	case CodeNotFound:
		return http.StatusNotFound
	case CodeIdempotencyConflict, CodeStaleEpoch, CodeInvalidState, CodeWorkerExhausted, CodeRunBudgetExhausted:
		return http.StatusConflict
	case CodePayloadTooLarge:
		return http.StatusRequestEntityTooLarge
	case CodeUpstreamRateLimited:
		return http.StatusTooManyRequests
	case CodeNotImplemented:
		return http.StatusNotImplemented
	case CodeExecutionUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// ErrorScope is the blast radius of a failure.
type ErrorScope string

const (
	ScopeAttempt      ErrorScope = "attempt"
	ScopeSession      ErrorScope = "session"
	ScopeWorkItem     ErrorScope = "work_item"
	ScopePlannerRound ErrorScope = "planner_round"
	ScopeRun          ErrorScope = "run"
	ScopeEpoch        ErrorScope = "epoch"
	ScopeRequest      ErrorScope = "request"
)

// SuggestedAction is a machine value. The UI may translate it.
type SuggestedAction string

const (
	ActionNone           SuggestedAction = "none"
	ActionRetryDirection SuggestedAction = "retry_direction"
	ActionSteerWork      SuggestedAction = "steer_work"
	ActionWait           SuggestedAction = "wait"
	ActionGrantBudget    SuggestedAction = "grant_budget"
	ActionStopRun        SuggestedAction = "stop_run"
)

// Error is the typed failure returned by contract helpers and ports.
type Error struct {
	Code            ErrorCode
	Scope           ErrorScope
	Message         string
	Retryable       bool
	SourceID        string
	SuggestedAction SuggestedAction
	HTTPStatus      int
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	if e.Message == "" {
		return e.Code.String()
	}
	return e.Code.String() + ": " + e.Message
}

func (e *Error) Is(target error) bool {
	var other *Error
	if !errors.As(target, &other) || other == nil {
		return false
	}
	if other.Code == "" {
		return true
	}
	return e != nil && e.Code == other.Code
}

func (e *Error) Status() int {
	if e == nil {
		return http.StatusInternalServerError
	}
	if e.HTTPStatus != 0 {
		return e.HTTPStatus
	}
	return e.Code.DefaultHTTPStatus()
}

// ErrorBody is the public JSON object. It must not contain credentials, raw
// upstream headers, or SQL.
type ErrorBody struct {
	Code            ErrorCode       `json:"code"`
	Scope           ErrorScope      `json:"scope"`
	Message         string          `json:"message"`
	Retryable       bool            `json:"retryable"`
	SourceID        string          `json:"source_id,omitempty"`
	SuggestedAction SuggestedAction `json:"suggested_action,omitempty"`
}

// Envelope is the error body for new command, main-message, and workbench routes.
// Existing routes keep their historical string error until their owner migrates them.
type Envelope struct {
	Error ErrorBody `json:"error"`
}

func (e *Error) Envelope() Envelope {
	if e == nil {
		return Envelope{Error: ErrorBody{Code: CodePersistenceError, Scope: ScopeRequest, Message: "nil error"}}
	}
	return Envelope{Error: ErrorBody{
		Code:            e.Code,
		Scope:           e.Scope,
		Message:         e.Message,
		Retryable:       e.Retryable,
		SourceID:        e.SourceID,
		SuggestedAction: e.SuggestedAction,
	}}
}

// LegacyError is the pre-contract handler shape. New routes must not emit it.
type LegacyError struct {
	Error string `json:"error"`
}

func invalidInput(message string) *Error {
	return &Error{Code: CodeInvalidInput, Scope: ScopeRequest, Message: message, SuggestedAction: ActionNone}
}

func unauthorized(message string) *Error {
	return &Error{Code: CodeUnauthorized, Scope: ScopeRequest, Message: message, SuggestedAction: ActionNone}
}

// ErrNotImplemented is the fail-closed result of every unwired port.
var ErrNotImplemented = &Error{
	Code:            CodeNotImplemented,
	Scope:           ScopeRequest,
	Message:         "contract port has no production implementation",
	Retryable:       false,
	SuggestedAction: ActionNone,
	HTTPStatus:      http.StatusNotImplemented,
}
