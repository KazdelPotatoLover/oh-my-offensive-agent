package execution

import (
	"strings"

	"agent-workbench/backend/internal/contract"
)

func invalidInput(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodeInvalidInput,
		Scope:           contract.ScopeRequest,
		Message:         message,
		SuggestedAction: contract.ActionNone,
	}
}

func scopeDenied(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodeScopeDenied,
		Scope:           contract.ScopeAttempt,
		Message:         message,
		SuggestedAction: contract.ActionNone,
	}
}

func unavailable(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodeExecutionUnavailable,
		Scope:           contract.ScopeAttempt,
		Message:         message,
		Retryable:       true,
		SuggestedAction: contract.ActionRetryDirection,
	}
}

func staleEpoch(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodeStaleEpoch,
		Scope:           contract.ScopeEpoch,
		Message:         message,
		SuggestedAction: contract.ActionNone,
	}
}

func invalidState(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodeInvalidState,
		Scope:           contract.ScopeAttempt,
		Message:         message,
		SuggestedAction: contract.ActionNone,
	}
}

func notFound(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodeNotFound,
		Scope:           contract.ScopeRequest,
		Message:         message,
		SuggestedAction: contract.ActionNone,
	}
}

func unavailableCause(message string, err error) *contract.Error {
	if err == nil {
		return unavailable(message)
	}
	detail := sanitizeOperatorDetail(err.Error())
	if detail == "" {
		return unavailable(message)
	}
	return unavailable(message + ": " + detail)
}

// sanitizeOperatorDetail keeps a short Docker status for operators and tests.
// Host paths and the socket path are not returned to the caller.
func sanitizeOperatorDetail(raw string) string {
	raw = strings.Join(strings.Fields(raw), " ")
	if len(raw) > 180 {
		raw = raw[:180]
	}
	for _, forbidden := range []string{"/home/", "/var/run/docker.sock", "/tmp/"} {
		if strings.Contains(raw, forbidden) {
			return ""
		}
	}
	return raw
}

func persistence(message string) *contract.Error {
	return &contract.Error{
		Code:            contract.CodePersistenceError,
		Scope:           contract.ScopeRun,
		Message:         message,
		Retryable:       false,
		SuggestedAction: contract.ActionStopRun,
	}
}
