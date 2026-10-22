package orchestration

import (
	"context"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

// Revoke implements contract.ResourceRevoker so job and HTTP owners can cancel
// through the same engine. Kill is a worker scope; Stop and epoch rollover are
// a run scope.
func (e *Engine) Revoke(ctx context.Context, req contract.RevokeRequest) (*contract.RevokeReport, error) {
	return e.RevokeResources(ctx, req)
}

// RevokeResources cancels in-process work for a scope and forwards the same
// request to an injected HTTP/job revoker. A worker scope cancels only that
// direction. A run scope cancels every old attempt. The reason is preserved.
func (e *Engine) RevokeResources(ctx context.Context, req contract.RevokeRequest) (*contract.RevokeReport, error) {
	if req.Scope.RunID == uuid.Nil {
		req.Scope.RunID = e.runID
	}
	if req.Scope.RunID != e.runID {
		return nil, contract.ErrNotImplemented
	}
	if req.Scope.EpochNumber == 0 {
		req.Scope.EpochNumber = e.epoch
	}
	if req.Scope.WorkItemID != nil {
		req.Scope.Role = contract.RoleWorker
		if req.Scope.AttemptID == nil || *req.Scope.AttemptID == uuid.Nil {
			if att, ok := e.activeAttempts.Load(*req.Scope.WorkItemID); ok {
				if id, isID := att.(uuid.UUID); isID {
					req.Scope.AttemptID = &id
				}
			}
		}
	} else if req.Scope.Role == "" {
		req.Scope.Role = contract.RolePlanner
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	cause := revokeCause(req.Reason)
	report := &contract.RevokeReport{Kind: req.Kind}
	if req.Scope.WorkItemID != nil && *req.Scope.WorkItemID != uuid.Nil {
		if err := e.KillWorkItem(ctx, *req.Scope.WorkItemID, string(cause)); err != nil {
			return nil, err
		}
		report.Cancelled = 1
	} else {
		e.cancelAdmittedWork(cause)
		report.Cancelled = int(e.activeWorkerCount.Load())
	}
	if e.revoker != nil {
		forward, err := e.revoker.Revoke(ctx, req)
		if err != nil {
			return report, err
		}
		if forward != nil {
			report.Cancelled += forward.Cancelled
			report.OutcomeUnknown += forward.OutcomeUnknown
		}
	}
	return report, nil
}

func (e *Engine) cancelAdmittedWork(cause model.TerminationCause) {
	e.admission.Store(false)
	e.notifyDispatcher()
	if e.cancelWorkerCause != nil {
		e.cancelWorkerCause(cause)
	}
	e.workerCancels.Range(func(_, value any) bool {
		if cancel, ok := value.(context.CancelCauseFunc); ok {
			cancel(cause)
		}
		return true
	})
	if e.modelLimiter != nil {
		// Wake queued model calls by cancelling their contexts through the
		// worker cancel above. The limiter itself has no separate queue token.
	}
}

func revokeCause(reason string) model.TerminationCause {
	switch reason {
	case "", string(model.CauseInternalError):
		return model.CauseUserStop
	case "killed_by_operator", string(model.CauseWorkerKilled):
		return model.CauseWorkerKilled
	case "epoch_revoked":
		return model.TerminationCause("epoch_revoked")
	default:
		return model.TerminationCause(reason)
	}
}
