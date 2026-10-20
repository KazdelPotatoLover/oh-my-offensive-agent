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
		w.handleCancellation(ctx)
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
		w.handleCancellation(ctx)
		return
	}

	// 2. Transition from queued to running
	_, err = w.runService.TransitionState(ctx, w.runID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		// If transition failed, check if it's because stop intent was saved in DB
		curRun, getErr := w.runService.Get(context.Background(), w.runID)
		if getErr == nil && curRun.Status == model.RunStatusStopping {
			w.handleCancellation(ctx)
			return
		}
		if ctx.Err() != nil {
			w.handleCancellation(ctx)
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
			w.handleCancellation(ctx)
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
			w.finalize(model.TerminationCause(reason))
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
				w.handleCancellation(ctx)
				return
			case <-time.After(w.stepDelay):
			}
		}
	}

	// 4. All steps completed. Retain the fake executor's public reason label.
	if ctx.Err() != nil {
		w.handleCancellation(ctx)
		return
	}
	w.finalize(model.TerminationCause("all steps completed"))
}

func (w *FakeWorker) finalize(cause model.TerminationCause) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := w.runService.FinalizeRunWrapup(ctx, w.runID, cause, nil); err != nil {
		slog.Error("fake worker: failed to finalize run", "run_id", w.runID, "error", err)
	}
}

func (w *FakeWorker) handleCancellation(ctx context.Context) {
	cause := model.CauseUserStop
	if ctx.Err() != nil {
		cause = model.ResolveCancelCause(ctx)
	}
	w.finalize(cause)
}

func (w *FakeWorker) ensureStoppingTerminal() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	run, err := w.runService.Get(ctx, w.runID)
	if err == nil && run.Status == model.RunStatusStopping {
		w.finalize(model.CauseUserStop)
	}
}
