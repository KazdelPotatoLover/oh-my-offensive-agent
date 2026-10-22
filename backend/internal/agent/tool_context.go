package agent

import (
	"context"
	"encoding/json"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/execution"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

// toolExecutionContext applies the role timeout and the server phase. The
// historical 5s tool cap is not the command ceiling. A parent deadline still
// wins, and this function does not extend it.
func toolExecutionContext(parent context.Context, name string, args []byte, budget model.BudgetConfig, inSettlement bool, toolCallID uuid.UUID, epoch int) (context.Context, context.CancelFunc) {
	timeout := time.Duration(budget.ToolTimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	switch name {
	case "http_request":
		want := requestedTimeout(args)
		if want <= 0 {
			want = contract.DefaultHTTPToolTimeout
		}
		if want > contract.MaxHTTPToolTimeout {
			want = contract.MaxHTTPToolTimeout
		}
		if want > timeout {
			timeout = want
		}
	case "run_command", "write_file":
		runRem, attemptRem := parentRemainder(parent)
		clamped, err := execution.ClampCommandTimeout(requestedTimeout(args), runRem, attemptRem)
		if err != nil {
			cancelled, cancel := context.WithCancel(parent)
			cancel()
			return annotateToolContext(cancelled, inSettlement, toolCallID, epoch), cancel
		}
		timeout = clamped
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	return annotateToolContext(ctx, inSettlement, toolCallID, epoch), cancel
}

func annotateToolContext(ctx context.Context, inSettlement bool, toolCallID uuid.UUID, epoch int) context.Context {
	phase := execution.PhaseExplore
	if inSettlement {
		phase = execution.PhaseSettlement
	}
	ctx = execution.WithPhase(ctx, phase)
	if toolCallID != uuid.Nil {
		ctx = execution.WithSourceToolCall(ctx, toolCallID)
	}
	if epoch >= 1 {
		ctx = execution.WithEpoch(ctx, epoch)
	}
	return ctx
}

func requestedTimeout(args []byte) time.Duration {
	var in struct {
		TimeoutSeconds int `json:"timeout_seconds"`
	}
	if json.Unmarshal(args, &in) != nil || in.TimeoutSeconds <= 0 {
		return 0
	}
	return time.Duration(in.TimeoutSeconds) * time.Second
}

func parentRemainder(parent context.Context) (time.Duration, time.Duration) {
	deadline, ok := parent.Deadline()
	if !ok {
		return 0, 0
	}
	left := time.Until(deadline)
	if left < 0 {
		left = 0
	}
	return left, left
}

func currentEpoch(ctx context.Context, sr *SessionRunner) int {
	if sr == nil || sr.runService == nil || sr.runService.Pool() == nil {
		return 0
	}
	var epoch int
	if err := sr.runService.Pool().QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1`, sr.runID).Scan(&epoch); err != nil || epoch < 1 {
		return 0
	}
	return epoch
}
