package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"agent-workbench/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// FinalizeRunWrapup executes the guarded unified wrap-up transaction:
//  1. Locks the Run row.
//  2. If the Run is already in a terminal status:
//     Keeps the existing terminal status and finish_reason (First terminal writer wins),
//     but STILL executes child cleanup queries idempotently to ensure no running items/attempts/sessions remain.
//  3. If the Run is NOT terminal:
//     Executes legal status transitions to the target terminal status, records state_change event,
//     updates finish_reason, final_output (if provided), and finished_at.
//  4. Closes all unfinished children (work_items -> cancelled, attempts -> stopped, sessions -> stopped,
//     calls -> interrupted, planner_rounds -> stopped).
func (s *RunService) FinalizeRunWrapupWithEpoch(ctx context.Context, runID uuid.UUID, cause model.TerminationCause, finalOutput *string, expectedEpoch int) error {
	return s.finalizeRunWrapup(ctx, runID, cause, finalOutput, false, expectedEpoch)
}

func (s *RunService) FinalizeRunWrapup(ctx context.Context, runID uuid.UUID, cause model.TerminationCause, finalOutput *string) error {
	return s.finalizeRunWrapup(ctx, runID, cause, finalOutput, false, 0)
}

func (s *RunService) finalizeRunWrapup(ctx context.Context, runID uuid.UUID, cause model.TerminationCause, finalOutput *string, rejectTerminal bool, expectedEpoch int) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var currentStatus model.RunStatus
	var currentReason *string
	var currentEpoch int
	if err = tx.QueryRow(ctx, `SELECT status, finish_reason, current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&currentStatus, &currentReason, &currentEpoch); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if expectedEpoch > 0 && expectedEpoch != currentEpoch {
		return fmt.Errorf("%w: cannot finalize run from stale epoch %d (current is %d)", ErrStaleEpoch, expectedEpoch, currentEpoch)
	}

	reasonStr := string(cause)
	if reasonStr == "" {
		reasonStr = string(model.CauseUserStop)
	}

	// Persist epoch termination as part of the same fenced transaction.
	epochStatus, epochReason := cause.TargetRunStatus(), reasonStr
	if currentStatus.IsTerminal() {
		epochStatus = currentStatus
		if currentReason != nil {
			epochReason = *currentReason
		}
	}
	if currentStatus == model.RunStatusStopping {
		epochStatus = model.RunStatusStopped
		epochReason = string(model.CauseUserStop)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO execution_epochs(run_id,epoch_number,status,started_at)
		SELECT id,current_epoch,'running',COALESCE(started_at,now()) FROM runs WHERE id=$1
		ON CONFLICT(run_id,epoch_number) DO NOTHING`, runID); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE execution_epochs SET status=CASE WHEN status='running' THEN $1 ELSE status END,
		finished_at=COALESCE(finished_at,(SELECT finished_at FROM runs WHERE id=$4),now()),finish_reason=COALESCE(finish_reason,$2),summary=COALESCE(summary,$3)
		WHERE run_id=$4 AND epoch_number=$5`, string(epochStatus), epochReason, finalOutput, runID, currentEpoch); err != nil {
		return err
	}

	// Helper for cleaning up children
	cleanupChildren := func(childStatus, childReason string) error {
		workStatus := "cancelled"
		if childStatus == "interrupted" {
			workStatus = "failed"
		}
		if childReason == string(model.CauseModelBudgetExhausted) || childReason == string(model.CauseToolBudgetExhausted) {
			// A run-level budget stop distinguishes work that had started from
			// work that never began. Both remain visible in the final snapshot.
			if _, err := tx.Exec(ctx, `UPDATE work_items SET status='exhausted',updated_at=now() WHERE run_id=$1 AND status='running'`, runID); err != nil {
				return fmt.Errorf("cleanup exhausted work items: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE work_items SET status='cancelled',updated_at=now() WHERE run_id=$1 AND status IN ('pending','blocked')`, runID); err != nil {
				return fmt.Errorf("cleanup unstarted work items: %w", err)
			}
		} else if _, err := tx.Exec(ctx, `UPDATE work_items SET status=$2,updated_at=now() WHERE run_id=$1 AND status IN ('pending','running','blocked')`, runID, workStatus); err != nil {
			return fmt.Errorf("cleanup work items: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE planner_rounds SET status=CASE WHEN $1='interrupted' THEN 'stopped' ELSE $1 END,finished_at=now() WHERE run_id=$2 AND status='running'`, childStatus, runID); err != nil {
			return fmt.Errorf("cleanup planner rounds: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE attempts SET status=$1,finish_reason=$2,finished_at=now() WHERE status='running' AND work_item_id IN (SELECT id FROM work_items WHERE run_id=$3)`, childStatus, childReason, runID); err != nil {
			return fmt.Errorf("cleanup attempts: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_sessions SET status=$1,finish_reason=$2,updated_at=now() WHERE run_id=$3 AND status='running' AND role!='verifier'`, childStatus, childReason, runID); err != nil {
			return fmt.Errorf("cleanup agent_sessions: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE model_calls SET status='interrupted',error_message=$1,finished_at=now() WHERE run_id=$2 AND status='started' AND (session_id IS NULL OR session_id IN (SELECT id FROM agent_sessions WHERE role!='verifier'))`, childReason, runID); err != nil {
			return fmt.Errorf("cleanup model_calls: %w", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE agent_tool_calls SET status='interrupted',error_message=$1,finished_at=now() WHERE run_id=$2 AND status IN ('pending','running') AND (session_id IS NULL OR session_id IN (SELECT id FROM agent_sessions WHERE role!='verifier'))`, childReason, runID); err != nil {
			return fmt.Errorf("cleanup agent_tool_calls: %w", err)
		}
		return nil
	}

	// If already terminal, keep existing status and reason, but IDEMPOTENTLY CLEAN UP ANY LEFTOVER CHILDREN!
	if currentStatus.IsTerminal() {
		if rejectTerminal {
			return ErrTerminalState
		}
		effectiveReason := reasonStr
		if currentReason != nil && *currentReason != "" {
			effectiveReason = *currentReason
		}
		childStatus := "stopped"
		if currentStatus == model.RunStatusInterrupted {
			childStatus = "interrupted"
		}
		if currentStatus == model.RunStatusFailed {
			childStatus = "failed"
		}
		if err := cleanupChildren(childStatus, effectiveReason); err != nil {
			return err
		}
		if finalOutput != nil && *finalOutput != "" && (cause.TargetRunStatus() != model.RunStatusCompleted || currentStatus == model.RunStatusCompleted) {
			if _, err = tx.Exec(ctx, `UPDATE runs SET final_output = COALESCE(final_output, $1) WHERE id = $2`, *finalOutput, runID); err != nil {
				return err
			}
		}
		return tx.Commit(ctx)
	}

	// An explicit stop request takes precedence over other reasons if stopping
	if currentStatus == model.RunStatusStopping {
		cause = model.CauseUserStop
		reasonStr = string(model.CauseUserStop)
	}

	targetStatus := cause.TargetRunStatus()

	var seq int
	if err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0) FROM run_events WHERE run_id=$1`, runID).Scan(&seq); err != nil {
		return err
	}

	transition := func(to model.RunStatus) error {
		if currentStatus == to {
			return nil
		}
		if !model.IsAllowedTransition(currentStatus, to) {
			return fmt.Errorf("%w: %s to %s", ErrInvalidTransition, currentStatus, to)
		}
		if _, err := tx.Exec(ctx, `UPDATE runs SET status=$1,updated_at=now() WHERE id=$2`, to, runID); err != nil {
			return err
		}
		seq++
		payload, _ := json.Marshal(map[string]interface{}{"from": currentStatus, "to": to, "reason": reasonStr})
		if _, err := tx.Exec(ctx, `INSERT INTO run_events(id,run_id,seq,type,payload) VALUES(gen_random_uuid(),$1,$2,'state_change',$3)`, runID, seq, payload); err != nil {
			return err
		}
		currentStatus = to
		return nil
	}

	if targetStatus == model.RunStatusStopped {
		if currentStatus != model.RunStatusStopping {
			if err = transition(model.RunStatusStopping); err != nil {
				return err
			}
		}
		if err = transition(model.RunStatusStopped); err != nil {
			return err
		}
	} else if targetStatus == model.RunStatusFailed {
		if currentStatus == model.RunStatusRunning || currentStatus == model.RunStatusQueued {
			if err = transition(model.RunStatusFailed); err != nil {
				return err
			}
		} else if currentStatus == model.RunStatusStopping {
			targetStatus = model.RunStatusStopped
			reasonStr = string(model.CauseUserStop)
			if err = transition(model.RunStatusStopped); err != nil {
				return err
			}
		}
	} else {
		if err = transition(targetStatus); err != nil {
			return err
		}
	}

	if finalOutput != nil && *finalOutput != "" {
		if _, err = tx.Exec(ctx, `UPDATE runs SET finish_reason=$1,final_output=$2,finished_at=now(),updated_at=now() WHERE id=$3`, reasonStr, *finalOutput, runID); err != nil {
			return err
		}
	} else {
		if _, err = tx.Exec(ctx, `UPDATE runs SET finish_reason=$1,finished_at=now(),updated_at=now() WHERE id=$2`, reasonStr, runID); err != nil {
			return err
		}
	}

	if targetStatus == model.RunStatusCompleted && finalOutput != nil && *finalOutput != "" {
		seq++
		payload, _ := json.Marshal(map[string]interface{}{"output": *finalOutput})
		if _, err = tx.Exec(ctx, `INSERT INTO run_events(id,run_id,seq,type,payload) VALUES(gen_random_uuid(),$1,$2,'final_answer',$3)`, runID, seq, payload); err != nil {
			return err
		}
	}

	childStatus := string(targetStatus)
	if childStatus != "stopped" && childStatus != "failed" && childStatus != "interrupted" {
		childStatus = "stopped"
	}
	if err := cleanupChildren(childStatus, reasonStr); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// FinalizeTerminatedRun is kept for compatibility and delegates to FinalizeRunWrapup.
func (s *RunService) FinalizeTerminatedRun(ctx context.Context, runID uuid.UUID, targetStatus model.RunStatus, reason string) error {
	cause := model.TerminationCause(reason)
	if targetStatus == model.RunStatusStopped {
		switch cause {
		case model.CauseTimeout, model.CauseModelBudgetExhausted, model.CauseToolBudgetExhausted:
		default:
			cause = model.CauseUserStop
		}
	}
	return s.FinalizeRunWrapup(ctx, runID, cause, nil)
}

// FinalizeCancelledRun is a convenience wrapper for FinalizeTerminatedRun with targetStatus = RunStatusStopped.
func (s *RunService) FinalizeCancelledRun(ctx context.Context, runID uuid.UUID, reason string) error {
	return s.FinalizeTerminatedRun(ctx, runID, model.RunStatusStopped, reason)
}

// RecoverActiveRuns recovers any runs left in active states across process restarts.
func (s *RunService) RecoverActiveRuns(ctx context.Context) ([]uuid.UUID, error) {
	// Independent verifier sessions are also processes lost by a server restart,
	// even when their original scan Run was already terminal.
	if _, err := s.pool.Exec(ctx, `WITH stopped AS (UPDATE finding_retests SET status='stopped',summary='interrupted_by_restart',finished_at=now() WHERE status='running' RETURNING session_id)
		UPDATE agent_sessions SET status='interrupted',finish_reason='interrupted_by_restart',updated_at=now() WHERE id IN (SELECT session_id FROM stopped) AND status='running'`); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE model_calls SET status='interrupted',error_message='interrupted_by_restart',finished_at=now() WHERE status='started' AND session_id IN (SELECT id FROM agent_sessions WHERE role='verifier' AND status='interrupted')`); err != nil {
		return nil, err
	}
	if _, err := s.pool.Exec(ctx, `UPDATE agent_tool_calls SET status='interrupted',error_message='interrupted_by_restart',finished_at=now() WHERE status IN ('pending','running') AND session_id IN (SELECT id FROM agent_sessions WHERE role='verifier' AND status='interrupted')`); err != nil {
		return nil, err
	}

	rows, err := s.pool.Query(ctx, `SELECT id, status FROM runs WHERE status IN ('queued', 'running', 'stopping')`)
	if err != nil {
		return nil, err
	}

	type activeRun struct {
		id     uuid.UUID
		status model.RunStatus
	}
	var toRecover []activeRun
	for rows.Next() {
		var r activeRun
		if err := rows.Scan(&r.id, &r.status); err != nil {
			rows.Close()
			return nil, err
		}
		toRecover = append(toRecover, r)
	}

	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var recoveredIDs []uuid.UUID
	for _, r := range toRecover {
		cause := model.CauseShutdown
		if r.status == model.RunStatusStopping {
			cause = model.CauseUserStop
		}
		fallbackSummary := fmt.Sprintf("Run recovered on process startup: interrupted while in status '%s'", r.status)
		if err := s.FinalizeRunWrapup(ctx, r.id, cause, &fallbackSummary); err != nil {
			slog.Error("failed to recover active run", "run_id", r.id, "error", err)
			return recoveredIDs, err
		} else {
			recoveredIDs = append(recoveredIDs, r.id)
			slog.Info("recovered active run on startup", "run_id", r.id, "from_status", r.status)
		}
	}
	return recoveredIDs, nil
}
