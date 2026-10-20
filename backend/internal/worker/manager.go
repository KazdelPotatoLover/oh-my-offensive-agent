package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrManagerClosed = errors.New("worker manager is closed")
)

type WorkerOption func(*FakeWorker)

func WithStepDelay(d time.Duration) WorkerOption {
	return func(w *FakeWorker) {
		w.stepDelay = d
	}
}

func WithOnStepHook(hook func(step int)) WorkerOption {
	return func(w *FakeWorker) {
		w.onStepHook = hook
	}
}

type Manager struct {
	pool          *pgxpool.Pool
	runService    *service.RunService
	mu            sync.Mutex
	closed        bool
	cancels       map[uuid.UUID]context.CancelCauseFunc
	pendingCancel map[uuid.UUID]bool
	defaultOpts   []WorkerOption
	wg            sync.WaitGroup
}

func NewManager(pool *pgxpool.Pool, runService *service.RunService) *Manager {
	return &Manager{
		pool:          pool,
		runService:    runService,
		cancels:       make(map[uuid.UUID]context.CancelCauseFunc),
		pendingCancel: make(map[uuid.UUID]bool),
	}
}

// SetDefaultWorkerOptions configures default options applied to all workers started by this manager.
func (m *Manager) SetDefaultWorkerOptions(opts ...WorkerOption) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.defaultOpts = opts
}

type Executor interface {
	Run(ctx context.Context)
}

// Start launches a FakeWorker for a run.
func (m *Manager) Start(runID uuid.UUID, failAtStep *int, opts ...WorkerOption) error {
	m.mu.Lock()
	combinedOpts := append(append([]WorkerOption{}, m.defaultOpts...), opts...)
	m.mu.Unlock()

	worker := NewFakeWorker(runID, failAtStep, m.runService, combinedOpts...)
	return m.StartExecutor(runID, worker)
}

// StartExecutor launches any Executor (FakeWorker or AgentRunner) for a run.
func (m *Manager) StartExecutor(runID uuid.UUID, executor Executor) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}

	if _, exists := m.cancels[runID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("executor already registered for run %s", runID)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	// If stop was already requested for this run before worker registered:
	if m.pendingCancel[runID] {
		delete(m.pendingCancel, runID)
		cancel(model.CauseUserStop)
	}
	m.cancels[runID] = cancel
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.cancels, runID)
			m.mu.Unlock()
			// Delete first: a concurrent Stop either sees this executor and this
			// check observes stopping, or sees no executor and finalizes it itself.
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
			if run, err := m.runService.Get(cleanupCtx, runID); err == nil && run.Status.IsActive() && (ctx.Err() != nil || run.Status == model.RunStatusStopping) {
				cause := model.CauseInternalError
				if ctx.Err() != nil {
					cause = model.ResolveCancelCause(ctx)
				}
				if run.Status == model.RunStatusStopping {
					cause = model.CauseUserStop
				}
				if err = m.runService.FinalizeRunWrapup(cleanupCtx, runID, cause, nil); err != nil {
					slog.Error("manager: failed to finalize stop", "run_id", runID, "error", err)
				}
			}
			cleanupCancel()
			m.wg.Done()
			slog.Info("executor finished", "run_id", runID)
		}()
		executor.Run(ctx)
	}()

	slog.Info("executor started", "run_id", runID)
	return nil
}

// Stop requests cancellation for a run. If worker is not yet registered, records pending cancel.
func (m *Manager) Stop(runID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if cancel, ok := m.cancels[runID]; ok {
		cancel(model.CauseUserStop)
		slog.Info("worker stop requested (active cancel called)", "run_id", runID)
	} else {
		// Stop requested before worker registered!
		m.pendingCancel[runID] = true
		slog.Info("worker stop requested before registration (pending cancel recorded)", "run_id", runID)
	}
}

// StopAndFinalizeIfIdle also acknowledges a stop when its executor already exited.
// pendingCancel ensures a registration racing with this path receives cancellation.
func (m *Manager) StopAndFinalizeIfIdle(ctx context.Context, runID uuid.UUID) error {
	m.mu.Lock()
	if cancel, ok := m.cancels[runID]; ok {
		cancel(model.CauseUserStop)
		m.mu.Unlock()
		return nil
	}
	m.pendingCancel[runID] = true
	m.mu.Unlock()
	return m.runService.FinalizeCancelledRun(ctx, runID, "user_requested_stop")
}

// StopAll cancels all active worker executions.
func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()

	for runID, cancel := range m.cancels {
		cancel(model.CauseShutdown)
		slog.Info("worker stop requested via StopAll", "run_id", runID)
	}
	slog.Info("all active workers stop requested")
}

// Close marks the manager as closed, stopping reception of new work.
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	slog.Info("worker manager closed (no new workers accepted)")
}

// Wait waits for all registered workers to exit, or returns error if timeout occurs.
func (m *Manager) Wait(timeout time.Duration) error {
	done := make(chan struct{})
	go func() {
		m.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return nil
	case <-time.After(timeout):
		return fmt.Errorf("timed out after %v waiting for workers to exit", timeout)
	}
}

// Shutdown gracefully stops all work: stops accepting, cancels all, and waits up to timeout.
func (m *Manager) Shutdown(timeout time.Duration) error {
	m.Close()
	m.StopAll()
	return m.Wait(timeout)
}
