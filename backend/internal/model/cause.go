package model

import (
	"context"
	"errors"
)

type TerminationCause string

const (
	CauseCompleted            TerminationCause = "completed"
	CauseUserStop             TerminationCause = "user_requested_stop"
	CauseTimeout              TerminationCause = "timeout"
	CauseModelBudgetExhausted TerminationCause = "model_calls_exhausted"
	CauseToolBudgetExhausted  TerminationCause = "tool_calls_exhausted"
	CauseInternalError        TerminationCause = "internal_error"
	CauseShutdown             TerminationCause = "shutdown"
)

func (c TerminationCause) String() string {
	return string(c)
}

// Error implements error so TerminationCause can be used with context.WithCancelCause
func (c TerminationCause) Error() string {
	return string(c)
}

// TargetRunStatus maps the termination cause to its corresponding RunStatus
func (c TerminationCause) TargetRunStatus() RunStatus {
	switch c {
	case CauseCompleted, TerminationCause("final_answer"), TerminationCause("all steps completed"):
		return RunStatusCompleted
	case CauseUserStop, CauseTimeout:
		return RunStatusStopped
	case CauseModelBudgetExhausted, CauseToolBudgetExhausted:
		return RunStatusStopped
	case CauseInternalError:
		return RunStatusFailed
	case CauseShutdown:
		return RunStatusInterrupted
	default:
		return RunStatusFailed
	}
}

// ResolveCancelCause extracts the typed TerminationCause from context,
// falling back to timeout or user_requested_stop if standard context errors occur.
func ResolveCancelCause(ctx context.Context) TerminationCause {
	if ctx == nil {
		return CauseInternalError
	}
	cause := context.Cause(ctx)
	if cause != nil {
		var tc TerminationCause
		if errors.As(cause, &tc) {
			return tc
		}
		switch cause.Error() {
		case string(CauseCompleted):
			return CauseCompleted
		case string(CauseUserStop):
			return CauseUserStop
		case string(CauseTimeout):
			return CauseTimeout
		case string(CauseModelBudgetExhausted):
			return CauseModelBudgetExhausted
		case string(CauseToolBudgetExhausted):
			return CauseToolBudgetExhausted
		case string(CauseShutdown):
			return CauseShutdown
		case string(CauseInternalError):
			return CauseInternalError
		}
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return CauseTimeout
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return CauseUserStop
	}
	return CauseInternalError
}
