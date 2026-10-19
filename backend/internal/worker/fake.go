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

const defaultTotalSteps = 10

type FakeWorker struct {
	runID      uuid.UUID
	failAtStep *int
	runService *service.RunService
	totalSteps int
	stepDelay  time.Duration
	onStepHook func(step int)
}

func NewFakeWorker(runID uuid.UUID, failAtStep *int, runService *service.RunService, opts ...WorkerOption) *FakeWorker {
	w := &FakeWorker{
		runID:      runID,
		failAtStep: failAtStep,
		runService: runService,
		totalSteps: defaultTotalSteps,
		stepDelay:  1 * time.Second,
	}
	for _, opt := range opts {
		opt(w)
	}
	return w
}

func (w *FakeWorker) Run(ctx context.Context) {
	// GUARANTEE: When this worker exits, if the run's DB status is "stopping",
	// it MUST be transitioned to "stopped" so it never gets permanently stuck.
	defer func() {
		w.ensureStoppingTerminal()
	}()

	// 1. Initial check: Was stop already requested before worker even began?
	if ctx.Err() != nil {
		w.handleCancellation()
		return
	}

	// Also check DB status directly in case stop intent was saved in DB
	// while memory cancel signal had no receiver.
	run, err := w.runService.Get(context.Background(), w.runID)
	if err != nil {
		slog.Error("failed to get run status before starting", "run_id", w.runID, "error", err)
		return
	}
	if run.Status.IsTerminal() {
		slog.Info("run already in terminal state prior to start", "run_id", w.runID, "status", run.Status)
		return
	}
	if run.Status == model.RunStatusStopping {
		w.handleCancellation()
		return
	}

	// 2. Transition from queued to running
	_, err = w.runService.TransitionState(ctx, w.runID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		// If transition failed, check if it's because stop intent was saved in DB
		curRun, getErr := w.runService.Get(context.Background(), w.runID)
		if getErr == nil && curRun.Status == model.RunStatusStopping {
			w.handleCancellation()
			return
		}
		if ctx.Err() != nil {
			w.handleCancellation()
			return
		}
		slog.Error("failed to transition to running", "run_id", w.runID, "error", err)
		return
	}

	// 3. Execution steps
	for step := 1; step <= w.totalSteps; step++ {
		// Check cancellation at start of step
		select {
		case <-ctx.Done():
			w.handleCancellation()
			return
		default:
		}

		// Trigger optional test hook (for deterministic concurrency tests)
		if w.onStepHook != nil {
			w.onStepHook(step)
		}

		// Check if should fail at this step
		if w.failAtStep != nil && step == *w.failAtStep {
			reason := fmt.Sprintf("failed at step %d (simulated)", step)
			_, err := w.runService.TransitionState(
				context.Background(), w.runID,
				model.RunStatusRunning, model.RunStatusFailed, &reason)
			if err != nil {
				w.ensureStoppingTerminal()
				slog.Error("failed to transition to failed", "run_id", w.runID, "error", err)
			}
			return
		}

		// Insert progress event
		message := fmt.Sprintf("Processing step %d/%d", step, w.totalSteps)
		if err := w.runService.InsertProgressEvent(
			context.Background(), w.runID, step, w.totalSteps, message); err != nil {
			// Progress event failed (e.g. status was moved to stopping concurrently)
			slog.Warn("progress event rejected, checking stop status", "run_id", w.runID, "step", step, "error", err)
			w.ensureStoppingTerminal()
			return
		}

		// Sleep / delay between steps
		if w.stepDelay > 0 {
			select {
			case <-ctx.Done():
				w.handleCancellation()
				return
			case <-time.After(w.stepDelay):
			}
		}
	}

	// 4. All steps completed
	reason := "all steps completed"
	_, err = w.runService.TransitionState(
		context.Background(), w.runID,
		model.RunStatusRunning, model.RunStatusCompleted, &reason)
	if err != nil {
		w.ensureStoppingTerminal()
		slog.Error("failed to transition to completed", "run_id", w.runID, "error", err)
	}
}

func (w *FakeWorker) handleCancellation() {
	ctx := context.Background()
	run, err := w.runService.Get(ctx, w.runID)
	if err != nil {
		slog.Error("failed to get run during cancellation", "run_id", w.runID, "error", err)
		return
	}

	if run.Status.IsTerminal() {
		slog.Info("run already in terminal state during cancellation", "run_id", w.runID, "status", run.Status)
		return
	}

	reason := "user requested stop"
	if run.Status == model.RunStatusStopping {
		_, err := w.runService.TransitionState(ctx, w.runID,
			model.RunStatusStopping, model.RunStatusStopped, &reason)
		if err != nil {
			slog.Error("failed to transition stopping to stopped", "run_id", w.runID, "error", err)
		}
		return
	}

	if run.Status == model.RunStatusQueued {
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

	if run.Status == model.RunStatusRunning {
		// Transition running -> stopping -> stopped
		_, err := w.runService.TransitionState(ctx, w.runID,
			model.RunStatusRunning, model.RunStatusStopping, nil)
		if err == nil {
			_, err = w.runService.TransitionState(ctx, w.runID,
				model.RunStatusStopping, model.RunStatusStopped, &reason)
		}
		if err != nil {
			slog.Error("failed to transition running to stopped", "run_id", w.runID, "error", err)
		}
		return
	}

	slog.Warn("unexpected status during cancellation", "run_id", w.runID, "status", run.Status)
}

func (w *FakeWorker) ensureStoppingTerminal() {
	ctx := context.Background()
	run, err := w.runService.Get(ctx, w.runID)
	if err == nil && run.Status == model.RunStatusStopping {
		reason := "user requested stop"
		_, transErr := w.runService.TransitionState(ctx, w.runID, model.RunStatusStopping, model.RunStatusStopped, &reason)
		if transErr != nil {
			slog.Error("ensureStoppingTerminal failed", "run_id", w.runID, "error", transErr)
		} else {
			slog.Info("ensureStoppingTerminal transitioned stopping to stopped", "run_id", w.runID)
		}
	}
}
