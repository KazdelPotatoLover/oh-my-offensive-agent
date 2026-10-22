package controlplane

import (
	"errors"
	"fmt"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/service"
)

func invalidInput(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: message, SuggestedAction: contract.ActionNone}
}

func unauthorized(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeUnauthorized, Scope: contract.ScopeRequest, Message: message, SuggestedAction: contract.ActionNone}
}

func staleEpoch(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeStaleEpoch, Scope: contract.ScopeEpoch, Message: message, SuggestedAction: contract.ActionNone}
}

func invalidState(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeInvalidState, Scope: contract.ScopeRun, Message: message, SuggestedAction: contract.ActionNone}
}

func notFound(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeNotFound, Scope: contract.ScopeRun, Message: message, SuggestedAction: contract.ActionNone}
}

func scopeDenied(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeScopeDenied, Scope: contract.ScopeRun, Message: message, SuggestedAction: contract.ActionNone}
}

func idempotencyConflict(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeIdempotencyConflict, Scope: contract.ScopeRequest, Message: message, SuggestedAction: contract.ActionNone}
}

func persistence(message string) *contract.Error {
	return &contract.Error{Code: contract.CodePersistenceError, Scope: contract.ScopeRequest, Message: message, SuggestedAction: contract.ActionNone}
}

func executionUnavailable(message string) *contract.Error {
	return &contract.Error{Code: contract.CodeExecutionUnavailable, Scope: contract.ScopeRun, Message: message, SuggestedAction: contract.ActionWait}
}

// mapServiceErr converts an existing service error into a contract error.
// It does not invent a new state transition.
func mapServiceErr(err error) error {
	if err == nil {
		return nil
	}
	var ce *contract.Error
	if errors.As(err, &ce) {
		return ce
	}
	switch {
	case errors.Is(err, service.ErrNotFound):
		return notFound("run or work item not found")
	case errors.Is(err, service.ErrStaleEpoch):
		return staleEpoch("caller epoch does not match the current run epoch")
	case errors.Is(err, service.ErrConflict):
		return idempotencyConflict("idempotency key was already used with different content")
	case errors.Is(err, service.ErrCrossRunBoundary):
		return scopeDenied("work item belongs to another run")
	case errors.Is(err, service.ErrAttemptConflict),
		errors.Is(err, service.ErrInvalidState),
		errors.Is(err, service.ErrInvalidWorkItemState),
		errors.Is(err, service.ErrTerminalState),
		errors.Is(err, service.ErrRunNotRunning),
		errors.Is(err, service.ErrInvalidTransition),
		errors.Is(err, service.ErrRunPaused):
		return invalidState(err.Error())
	default:
		return persistence(fmt.Sprintf("control persistence failed: %v", err))
	}
}
