package controlplane

import (
	"context"

	"agent-workbench/backend/internal/worker"

	"github.com/google/uuid"
)

// LiveControl signals the one registered executor. It must not start another
// engine and must not persist a second copy of a steer message.
// Steer is armed through ReplanPort, not through Manager.SteerWorkItem, because
// the baseline manager method writes the steer again.
type LiveControl interface {
	Pause(runID uuid.UUID) error
	Resume(runID uuid.UUID) error
	Stop(ctx context.Context, runID uuid.UUID) error
	Kill(runID, workItemID uuid.UUID, reason string) error
	Nudge(runID uuid.UUID)
	RefreshBudget(ctx context.Context, runID uuid.UUID) error
	Quiesce(ctx context.Context, runID uuid.UUID) error
	Revoke(runID uuid.UUID, oldEpoch int)
}

// ManagerLive adapts the existing worker manager. Kill cancels the live worker
// context. It does not call SteerWorkItem.
type ManagerLive struct {
	Manager *worker.Manager
}

func (m *ManagerLive) Pause(runID uuid.UUID) error {
	if m == nil || m.Manager == nil {
		return nil
	}
	return m.Manager.Pause(runID)
}

func (m *ManagerLive) Resume(runID uuid.UUID) error {
	if m == nil || m.Manager == nil {
		return nil
	}
	return m.Manager.Resume(runID)
}

func (m *ManagerLive) Stop(ctx context.Context, runID uuid.UUID) error {
	if m == nil || m.Manager == nil {
		return nil
	}
	return m.Manager.StopAndFinalizeIfIdle(ctx, runID)
}

func (m *ManagerLive) Kill(runID, workItemID uuid.UUID, reason string) error {
	if m == nil || m.Manager == nil {
		return nil
	}
	return m.Manager.KillWorkItem(runID, workItemID, reason)
}

func (m *ManagerLive) Nudge(runID uuid.UUID) {
	if m == nil || m.Manager == nil {
		return
	}
	m.Manager.Nudge(runID)
}

func (m *ManagerLive) RefreshBudget(ctx context.Context, runID uuid.UUID) error {
	if m == nil || m.Manager == nil {
		return nil
	}
	return m.Manager.RefreshBudget(ctx, runID)
}

func (m *ManagerLive) Quiesce(ctx context.Context, runID uuid.UUID) error {
	if m == nil || m.Manager == nil {
		return nil
	}
	return m.Manager.QuiesceEpoch(ctx, runID)
}

func (m *ManagerLive) Revoke(runID uuid.UUID, oldEpoch int) {
	if m == nil || m.Manager == nil {
		return
	}
	m.Manager.RevokeEpoch(runID, oldEpoch)
}
