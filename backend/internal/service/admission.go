package service

import (
	"agent-workbench/backend/internal/model"
	"context"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type epochContextKey struct{}

func WithExecutionEpoch(ctx context.Context, epoch int) context.Context {
	return context.WithValue(ctx, epochContextKey{}, epoch)
}
func validateContextEpoch(ctx context.Context, epoch int) error {
	if expected, ok := ctx.Value(epochContextKey{}).(int); ok && expected != epoch {
		return ErrStaleEpoch
	}
	return nil
}

func lockSessionAdmission(ctx context.Context, tx pgx.Tx, runID uuid.UUID, sessionID *uuid.UUID, allowPaused ...bool) error {
	if sessionID == nil {
		return lockRunAdmission(ctx, tx, runID)
	}
	var runStatus model.RunStatus
	var current int
	if err := tx.QueryRow(ctx, `SELECT status,current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&runStatus, &current); err != nil {
		return err
	}
	if err := validateContextEpoch(ctx, current); err != nil {
		return err
	}
	var role, status string
	var epoch int
	if err := tx.QueryRow(ctx, `SELECT role,status,epoch_number FROM agent_sessions WHERE id=$1 AND run_id=$2`, sessionID, runID).Scan(&role, &status, &epoch); err != nil {
		return ErrCrossRunBoundary
	}
	if epoch != current {
		return ErrStaleEpoch
	}
	if status != "running" {
		return ErrInvalidState
	}
	if role == "verifier" {
		var active bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM finding_retests WHERE session_id=$1 AND run_id=$2 AND status='running')`, sessionID, runID).Scan(&active); err != nil {
			return err
		}
		if !active {
			return ErrInvalidState
		}
		return nil // independent verification has its own bounded session budget
	}
	if runStatus.IsTerminal() || runStatus == model.RunStatusStopping {
		return ErrTerminalState
	}
	if runStatus == model.RunStatusPaused && (len(allowPaused) == 0 || !allowPaused[0]) {
		return ErrRunPaused
	}
	return nil
}

func (s *RunService) RunExecutionUsage(ctx context.Context, runID uuid.UUID) (int, int, error) {
	var models, tools int
	err := s.pool.QueryRow(ctx, `SELECT
	(SELECT count(*) FROM model_calls c LEFT JOIN agent_sessions s ON s.id=c.session_id WHERE c.run_id=$1 AND COALESCE(s.role,'')!='verifier'),
	(SELECT count(*) FROM agent_tool_calls c LEFT JOIN agent_sessions s ON s.id=c.session_id WHERE c.run_id=$1 AND COALESCE(s.role,'')!='verifier')`, runID).Scan(&models, &tools)
	return models, tools, err
}
