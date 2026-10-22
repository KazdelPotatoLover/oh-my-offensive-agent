package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

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
	pool            *pgxpool.Pool
	runService      *service.RunService
	mu              sync.Mutex
	closed          bool
	cancels         map[uuid.UUID]context.CancelCauseFunc
	executors       map[uuid.UUID]Executor
	executorEpochs  map[uuid.UUID]int
	executorDone    map[uuid.UUID]chan struct{}
	retestCancels   map[uuid.UUID]context.CancelCauseFunc
	pendingCancel   map[uuid.UUID]bool
	defaultOpts     []WorkerOption
	externalRevoker contract.ResourceRevoker
	wg              sync.WaitGroup
}

// UseResourceRevoker sets the job and capture revoker used by stop, kill,
// epoch rollover, and shutdown. It does not replace an engine's in-process revoker.
func (m *Manager) UseResourceRevoker(revoker contract.ResourceRevoker) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.externalRevoker = revoker
	m.mu.Unlock()
}

func NewManager(pool *pgxpool.Pool, runService *service.RunService) *Manager {
	return &Manager{
		pool:           pool,
		runService:     runService,
		cancels:        make(map[uuid.UUID]context.CancelCauseFunc),
		executors:      make(map[uuid.UUID]Executor),
		executorEpochs: make(map[uuid.UUID]int),
		executorDone:   make(map[uuid.UUID]chan struct{}),
		retestCancels:  make(map[uuid.UUID]context.CancelCauseFunc),
		pendingCancel:  make(map[uuid.UUID]bool),
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

// ControllableExecutor allows external run-time intervention on active engines.
type ControllableExecutor interface {
	Executor
	Epoch() int
	Pause(ctx context.Context) error
	Resume(ctx context.Context) error
	RefreshBudget(ctx context.Context) error
	KillWorkItem(ctx context.Context, workItemID uuid.UUID, reason string) error
	SteerWorkItem(ctx context.Context, workItemID uuid.UUID, message string) error
	Nudge()
}

// Start launches a FakeWorker for a run.
func (m *Manager) Start(runID uuid.UUID, failAtStep *int, opts ...WorkerOption) error {
	m.mu.Lock()
	combinedOpts := append(append([]WorkerOption{}, m.defaultOpts...), opts...)
	m.mu.Unlock()

	worker := NewFakeWorker(runID, failAtStep, m.runService, combinedOpts...)
	return m.StartExecutor(runID, worker)
}

// StartExecutor launches any Executor (FakeWorker, AgentRunner, Engine) for a run.
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

	epoch := 0
	if ctrl, ok := executor.(ControllableExecutor); ok {
		epoch = ctrl.Epoch()
	}
	if epoch <= 0 && m.runService != nil {
		if run, err := m.runService.Get(context.Background(), runID); err == nil {
			epoch = run.CurrentEpoch
		}
	}
	if epoch <= 0 {
		epoch = 1
	}

	ctx, cancel := context.WithCancelCause(context.Background())
	// If stop was already requested for this run before worker registered:
	if m.pendingCancel[runID] {
		delete(m.pendingCancel, runID)
		cancel(model.CauseUserStop)
	}
	m.cancels[runID] = cancel
	m.executors[runID] = executor
	m.executorEpochs[runID] = epoch
	done := make(chan struct{})
	m.executorDone[runID] = done
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			activeEpoch := m.executorEpochs[runID]
			isCurrent := (activeEpoch == epoch)
			if isCurrent {
				delete(m.cancels, runID)
				delete(m.executors, runID)
				delete(m.executorEpochs, runID)
				delete(m.executorDone, runID)
			}
			m.mu.Unlock()

			// Only finalize if this executor instance matches the run's current epoch.
			// Stale executors from prior epochs revoked by StartNewEpoch must not finalize the run.
			if isCurrent && m.runService != nil {
				cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 5*time.Second)
				if run, err := m.runService.Get(cleanupCtx, runID); err == nil && run.Status.IsActive() && (ctx.Err() != nil || run.Status == model.RunStatusStopping) {
					cause := model.CauseInternalError
					if ctx.Err() != nil {
						cause = model.ResolveCancelCause(ctx)
					}
					if run.Status == model.RunStatusStopping {
						cause = model.CauseUserStop
					}
					if err = m.runService.FinalizeRunWrapupWithEpoch(cleanupCtx, runID, cause, nil, epoch); err != nil {
						slog.Error("manager: failed to finalize stop", "run_id", runID, "epoch", epoch, "error", err)
					}
				}
				cleanupCancel()
			}
			m.wg.Done()
			close(done)
			slog.Info("executor finished", "run_id", runID, "epoch", epoch)
		}()
		executor.Run(ctx)
	}()

	slog.Info("executor started", "run_id", runID, "epoch", epoch)
	return nil
}

// QuiesceEpoch waits for the local executor's cancellation and finalization
// before an epoch transition. A failed drain must not create a phantom epoch.
func (m *Manager) QuiesceEpoch(ctx context.Context, runID uuid.UUID) error {
	m.mu.Lock()
	done := m.executorDone[runID]
	if cancel := m.cancels[runID]; cancel != nil {
		cancel(model.CauseShutdown)
	}
	m.mu.Unlock()
	if done == nil {
		return nil
	}
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// RevokeEpoch revokes and cancels any executor registered with epoch <= oldEpoch.
func (m *Manager) RevokeEpoch(runID uuid.UUID, oldEpoch int) {
	m.mu.Lock()
	exec := m.executors[runID]
	ep, ok := m.executorEpochs[runID]
	if ok && ep <= oldEpoch {
		if cancel, ok := m.cancels[runID]; ok {
			cancel(model.TerminationCause("epoch_revoked"))
		}
		delete(m.cancels, runID)
		delete(m.executors, runID)
		delete(m.executorEpochs, runID)
		delete(m.executorDone, runID)
		slog.Info("manager: revoked stale executor", "run_id", runID, "epoch", ep)
	} else {
		exec = nil
	}
	m.mu.Unlock()
	if oldEpoch >= 1 {
		req := m.runRevokeRequest(runID, oldEpoch, "epoch_revoked")
		if exec != nil {
			m.revokeExecutor(exec, req)
		}
		m.revokeExternal(req)
	}
}

// Stop requests cancellation for a run. If worker is not yet registered, records pending cancel.
func (m *Manager) Stop(runID uuid.UUID) {
	m.mu.Lock()
	exec := m.executors[runID]
	epoch := m.executorEpochs[runID]
	if cancel, ok := m.cancels[runID]; ok {
		cancel(model.CauseUserStop)
		m.mu.Unlock()
		slog.Info("worker stop requested (active cancel called)", "run_id", runID)
		m.revokeRun(exec, runID, epoch, string(model.CauseUserStop))
		return
	}
	m.pendingCancel[runID] = true
	m.mu.Unlock()
	slog.Info("worker stop requested before registration (pending cancel recorded)", "run_id", runID)
	m.revokeExternal(m.runRevokeRequest(runID, epoch, string(model.CauseUserStop)))
}

// StopAndFinalizeIfIdle also acknowledges a stop when its executor already exited.
// pendingCancel ensures a registration racing with this path receives cancellation.
func (m *Manager) StopAndFinalizeIfIdle(ctx context.Context, runID uuid.UUID) error {
	m.mu.Lock()
	exec := m.executors[runID]
	epoch := m.executorEpochs[runID]
	if cancel, ok := m.cancels[runID]; ok {
		cancel(model.CauseUserStop)
		m.mu.Unlock()
		m.revokeRun(exec, runID, epoch, string(model.CauseUserStop))
		return nil
	}
	m.pendingCancel[runID] = true
	m.mu.Unlock()
	return m.runService.FinalizeCancelledRun(ctx, runID, "user_requested_stop")
}

// StopAll cancels all active worker executions and retest sessions.
func (m *Manager) StopAll() {
	m.mu.Lock()
	type stopTarget struct {
		runID uuid.UUID
		epoch int
		exec  Executor
	}
	targets := make([]stopTarget, 0, len(m.cancels))
	for runID, cancel := range m.cancels {
		cancel(model.CauseShutdown)
		targets = append(targets, stopTarget{runID: runID, epoch: m.executorEpochs[runID], exec: m.executors[runID]})
		slog.Info("worker stop requested via StopAll", "run_id", runID)
	}
	for retestID, cancel := range m.retestCancels {
		cancel(model.CauseShutdown)
		slog.Info("retest stop requested via StopAll", "retest_id", retestID)
	}
	m.mu.Unlock()
	for _, target := range targets {
		m.revokeRun(target.exec, target.runID, target.epoch, string(model.CauseShutdown))
	}
	slog.Info("all active workers and retests stop requested")
}

// Pause pauses active execution of the run via controllable executor.
func (m *Manager) Pause(runID uuid.UUID) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()
	if ok {
		if ctrl, isCtrl := exec.(ControllableExecutor); isCtrl {
			return ctrl.Pause(context.Background())
		}
	}
	return nil
}

// Resume unpauses active execution of the run via controllable executor.
func (m *Manager) Resume(runID uuid.UUID) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()
	if ok {
		if ctrl, isCtrl := exec.(ControllableExecutor); isCtrl {
			return ctrl.Resume(context.Background())
		}
	}
	return nil
}

// RefreshBudget informs active executor that additional budget grants have been authorized.
func (m *Manager) RefreshBudget(ctx context.Context, runID uuid.UUID) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()
	if ok {
		if ctrl, isCtrl := exec.(ControllableExecutor); isCtrl {
			return ctrl.RefreshBudget(ctx)
		}
	}
	return nil
}

// KillWorkItem cancels one direction. A run-scoped stop is not used here.
func (m *Manager) KillWorkItem(runID, workItemID uuid.UUID, reason string) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	epoch := m.executorEpochs[runID]
	m.mu.Unlock()
	if !ok {
		m.revokeWorkItem(nil, runID, workItemID, epoch, reason)
		return nil
	}
	if epoch < 1 {
		epoch = 1
	}
	var revokeErr error
	revoked := false
	if revoker, isRevoker := exec.(contract.ResourceRevoker); isRevoker {
		item := workItemID
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		_, revokeErr = revoker.Revoke(ctx, contract.RevokeRequest{
			Scope: contract.ExecutionScope{
				RunID:       runID,
				EpochNumber: epoch,
				Role:        contract.RoleWorker,
				WorkItemID:  &item,
			},
			Kind:   contract.RevokeAttempt,
			Reason: reason,
		})
		cancel()
		revoked = revokeErr == nil
	}
	m.revokeWorkItem(exec, runID, workItemID, epoch, reason)
	if revoked {
		return nil
	}
	if ctrl, isCtrl := exec.(ControllableExecutor); isCtrl {
		return ctrl.KillWorkItem(context.Background(), workItemID, reason)
	}
	return revokeErr
}

// NoteSteer records guidance for the pre-tool replan port without writing it again.
func (m *Manager) NoteSteer(runID, workItemID uuid.UUID, message string) {
	m.mu.Lock()
	exec := m.executors[runID]
	m.mu.Unlock()
	if noter, ok := exec.(interface{ NoteSteer(uuid.UUID, string) }); ok {
		noter.NoteSteer(workItemID, message)
	}
}

func (m *Manager) revokeRun(exec Executor, runID uuid.UUID, epoch int, reason string) {
	req := m.runRevokeRequest(runID, epoch, reason)
	m.revokeExecutor(exec, req)
	m.revokeExternal(req)
}

func (m *Manager) runRevokeRequest(runID uuid.UUID, epoch int, reason string) contract.RevokeRequest {
	return contract.RevokeRequest{
		Scope: contract.ExecutionScope{
			RunID:       runID,
			EpochNumber: m.scopeEpoch(runID, epoch),
			Role:        contract.RolePlanner,
		},
		Kind:   contract.RevokeAttempt,
		Reason: reason,
	}
}

func (m *Manager) revokeWorkItem(exec Executor, runID, workItemID uuid.UUID, epoch int, reason string) {
	attemptID, ok := m.attemptForKill(exec, runID, workItemID)
	if !ok {
		slog.Warn("manager: work item kill has no attempt for job revoke", "run_id", runID, "work_item_id", workItemID)
		return
	}
	item := workItemID
	m.revokeExternal(contract.RevokeRequest{
		Scope: contract.ExecutionScope{
			RunID:       runID,
			EpochNumber: m.scopeEpoch(runID, epoch),
			Role:        contract.RoleWorker,
			WorkItemID:  &item,
			AttemptID:   &attemptID,
		},
		Kind:   contract.RevokeAttempt,
		Reason: reason,
	})
}

func (m *Manager) attemptForKill(exec Executor, runID, workItemID uuid.UUID) (uuid.UUID, bool) {
	if lookup, ok := exec.(interface {
		ActiveAttempt(uuid.UUID) (uuid.UUID, bool)
	}); ok {
		if id, found := lookup.ActiveAttempt(workItemID); found {
			return id, true
		}
	}
	if m == nil || m.runService == nil || m.runService.Pool() == nil {
		return uuid.Nil, false
	}
	var id uuid.UUID
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := m.runService.Pool().QueryRow(ctx, `
		SELECT a.id FROM attempts a
		JOIN work_items w ON w.id = a.work_item_id
		WHERE w.run_id=$1 AND w.id=$2 AND a.status='running'
		ORDER BY a.created_at DESC LIMIT 1`, runID, workItemID).Scan(&id)
	if err != nil || id == uuid.Nil {
		return uuid.Nil, false
	}
	return id, true
}

func (m *Manager) scopeEpoch(runID uuid.UUID, known int) int {
	if known >= 1 {
		return known
	}
	if m != nil && m.runService != nil && m.runService.Pool() != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		var epoch int
		if err := m.runService.Pool().QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1`, runID).Scan(&epoch); err == nil && epoch >= 1 {
			return epoch
		}
	}
	return 1
}

func (m *Manager) revokeExecutor(exec Executor, req contract.RevokeRequest) {
	revoker, ok := exec.(contract.ResourceRevoker)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := revoker.Revoke(ctx, req); err != nil {
		slog.Warn("manager: resource revoke failed", "run_id", req.Scope.RunID, "reason", req.Reason, "error", err)
	}
}

func (m *Manager) revokeExternal(req contract.RevokeRequest) {
	if m == nil {
		return
	}
	m.mu.Lock()
	revoker := m.externalRevoker
	m.mu.Unlock()
	if revoker == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), contract.CommandCleanupWindow)
	defer cancel()
	if _, err := revoker.Revoke(ctx, req); err != nil {
		slog.Warn("manager: external resource revoke failed", "run_id", req.Scope.RunID, "reason", req.Reason, "error", err)
	}
}

// SteerWorkItem delivers confirmable steering guidance to an active work item at its next turn boundary.
func (m *Manager) SteerWorkItem(runID, workItemID uuid.UUID, message string) error {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()
	if ok {
		if ctrl, isCtrl := exec.(ControllableExecutor); isCtrl {
			return ctrl.SteerWorkItem(context.Background(), workItemID, message)
		}
	}
	return nil
}

// Nudge wakes up the run engine scheduler.
func (m *Manager) Nudge(runID uuid.UUID) {
	m.mu.Lock()
	exec, ok := m.executors[runID]
	m.mu.Unlock()
	if ok {
		if ctrl, isCtrl := exec.(ControllableExecutor); isCtrl {
			ctrl.Nudge()
		}
	}
}

// StartRetest launches an independent verifier session for a finding.
func (m *Manager) StartRetest(runID, findingID, retestID uuid.UUID, p provider.Provider, reg *tools.Registry) error {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return ErrManagerClosed
	}
	if _, exists := m.retestCancels[retestID]; exists {
		m.mu.Unlock()
		return fmt.Errorf("retest already in progress for retest_id %s", retestID)
	}
	limiter, tokens, recovery, settingsErr := provider.SessionAdmissionFromEnv()
	if settingsErr != nil {
		m.mu.Unlock()
		return settingsErr
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	m.retestCancels[retestID] = cancel
	m.wg.Add(1)
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			delete(m.retestCancels, retestID)
			m.mu.Unlock()
			m.wg.Done()
			slog.Info("retest executor finished", "retest_id", retestID, "run_id", runID)
		}()

		retestCtx, retestCancel := context.WithTimeout(ctx, 60*time.Second)
		defer retestCancel()

		retestExec := orchestration.NewRetestExecutor(m.runService, p, reg)
		retestExec.ApplyProcessSettings(limiter, tokens, recovery)
		_, err := retestExec.ExecuteFindingRetest(retestCtx, runID, findingID, retestID)
		if err != nil {
			slog.Error("retest execution failed", "retest_id", retestID, "run_id", runID, "error", err)
			cleanupCtx, cCancel := context.WithTimeout(context.Background(), 5*time.Second)
			_, _ = m.runService.FinishFindingRetest(cleanupCtx, runID, findingID, retestID, "inconclusive", "", fmt.Sprintf("Retest execution error: %v", err))
			cCancel()
		}
	}()

	slog.Info("retest executor started", "retest_id", retestID, "run_id", runID)
	return nil
}

// StopRetest cancels an in-flight retest.
func (m *Manager) StopRetest(retestID uuid.UUID) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cancel, ok := m.retestCancels[retestID]; ok {
		cancel(model.TerminationCause("retest_cancelled"))
		slog.Info("retest cancelled", "retest_id", retestID)
	}
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
