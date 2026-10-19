package worker

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

const totalSteps = 10

type FakeWorker struct {
	runID      uuid.UUID
	failAtStep *int
	runService *service.RunService
}

func (w *FakeWorker) Run(ctx context.Context) {
	select {
	case <-ctx.Done():
		w.handleCancellation()
		return
	default:
	}

	// Transition from queued to running
	_, err := w.runService.TransitionState(ctx, w.runID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		if ctx.Err() != nil {
			w.handleCancellation()
			return
		}
		slog.Error("failed to transition to running", "run_id", w.runID, "error", err)
		return
	}

	for step := 1; step <= totalSteps; step++ {
		// Check for cancellation
		select {
		case <-ctx.Done():
			w.handleCancellation()
			return
		default:
		}

		// Check if should fail at this step
		if w.failAtStep != nil && step == *w.failAtStep {
			reason := fmt.Sprintf("failed at step %d (simulated)", step)
			_, err := w.runService.TransitionState(
				context.Background(), w.runID,
				model.RunStatusRunning, model.RunStatusFailed, &reason)
			if err != nil {
				slog.Error("failed to transition to failed", "run_id", w.runID, "error", err)
			}
			return
		}

		// Insert progress event
		message := fmt.Sprintf("Processing step %d/%d", step, totalSteps)
		if err := w.runService.InsertProgressEvent(
			context.Background(), w.runID, step, totalSteps, message); err != nil {
			slog.Error("failed to insert progress event", "run_id", w.runID, "step", step, "error", err)
			return
		}

		// Sleep 1 second
		select {
		case <-ctx.Done():
			w.handleCancellation()
			return
		case <-time.After(1 * time.Second):
		}
	}

	// All steps completed
	reason := "all steps completed"
	_, err = w.runService.TransitionState(
		context.Background(), w.runID,
		model.RunStatusRunning, model.RunStatusCompleted, &reason)
	if err != nil {
		slog.Error("failed to transition to completed", "run_id", w.runID, "error", err)
	}
}

func (w *FakeWorker) handleCancellation() {
	// Use a fresh context since the original is cancelled
	ctx := context.Background()

	// Read current status
	run, err := w.runService.Get(ctx, w.runID)
	if err != nil {
		slog.Error("failed to get run during cancellation", "run_id", w.runID, "error", err)
		return
	}

	if run.Status.IsTerminal() {
		slog.Info("run already in terminal state during cancellation", "run_id", w.runID, "status", run.Status)
		return
	}

	if run.Status == model.RunStatusStopping {
		reason := "user requested stop"
		_, err := w.runService.TransitionState(ctx, w.runID,
			model.RunStatusStopping, model.RunStatusStopped, &reason)
		if err != nil {
			slog.Error("failed to transition to stopped", "run_id", w.runID, "error", err)
		}
		return
	}

	if run.Status == model.RunStatusQueued {
		reason := "cancelled before starting"
		// Transition queued -> stopping -> stopped
		_, err := w.runService.TransitionState(ctx, w.runID,
			model.RunStatusQueued, model.RunStatusStopping, nil)
		if err == nil {
			_, err = w.runService.TransitionState(ctx, w.runID,
				model.RunStatusStopping, model.RunStatusStopped, &reason)
		}
		if err != nil {
			slog.Error("failed to transition queued to stopped", "run_id", w.runID, "error", err)
		}
		return
	}

	slog.Warn("unexpected status during cancellation", "run_id", w.runID, "status", run.Status)
}
