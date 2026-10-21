package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrStaleEpoch   = errors.New("stale execution epoch: caller epoch is older than current run epoch")
	ErrInvalidState = errors.New("invalid run or work item state for requested control operation")
	ErrRunPaused    = errors.New("run admission is paused")
)

// PauseRun pauses an actively running run.
func (s *RunService) PauseRun(ctx context.Context, runID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if status != model.RunStatusRunning {
		return fmt.Errorf("%w: cannot pause run in status %s", ErrInvalidState, status)
	}

	_, err = tx.Exec(ctx, `UPDATE runs SET status = 'paused', updated_at = now() WHERE id = $1`, runID)
	if err != nil {
		return fmt.Errorf("update run status to paused: %w", err)
	}

	payload, _ := json.Marshal(map[string]any{"from": "running", "to": "paused"})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'state_change', $2)`,
		runID, payload)
	if err != nil {
		return fmt.Errorf("insert pause event: %w", err)
	}

	return tx.Commit(ctx)
}

// ResumeRun resumes a paused run.
func (s *RunService) ResumeRun(ctx context.Context, runID uuid.UUID) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}

	if status != model.RunStatusPaused {
		return fmt.Errorf("%w: cannot resume run in status %s (must be paused)", ErrInvalidState, status)
	}

	_, err = tx.Exec(ctx, `UPDATE runs SET status = 'running', updated_at = now() WHERE id = $1`, runID)
	if err != nil {
		return fmt.Errorf("update run status to running: %w", err)
	}

	payload, _ := json.Marshal(map[string]any{"from": "paused", "to": "running"})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'state_change', $2)`,
		runID, payload)
	if err != nil {
		return fmt.Errorf("insert resume event: %w", err)
	}

	return tx.Commit(ctx)
}

// SteerWorkItem updates instructions and guidance for an active or pending work item without restarting.
func (s *RunService) SteerWorkItem(
	ctx context.Context,
	runID, workItemID uuid.UUID,
	instructions string,
	inputDataOverride json.RawMessage,
	idempotencyKeys ...string,
) error {
	if strings.TrimSpace(instructions) == "" && inputDataOverride == nil {
		return fmt.Errorf("instructions or inputDataOverride required for steering")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var runStatus model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if runStatus.IsTerminal() {
		return fmt.Errorf("%w: cannot steer work item in terminal run %s", ErrInvalidState, runStatus)
	}

	var wiRunID uuid.UUID
	var status model.WorkItemStatus
	var currentObjective string
	err = tx.QueryRow(ctx,
		`SELECT run_id, status, objective FROM work_items WHERE id = $1 FOR UPDATE`,
		workItemID,
	).Scan(&wiRunID, &status, &currentObjective)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if wiRunID != runID {
		return fmt.Errorf("%w: work item belongs to another run", ErrCrossRunBoundary)
	}
	if status == model.WorkItemStatusCompleted || status == model.WorkItemStatusFailed || status == model.WorkItemStatusCancelled {
		return fmt.Errorf("%w: cannot steer work item in terminal status %s", ErrInvalidState, status)
	}

	if len(inputDataOverride) > 0 && string(inputDataOverride) != "null" {
		return fmt.Errorf("%w: steer cannot change the immutable input/profile; submit a new authorized direction instead", ErrInvalidState)
	}
	key := uuid.NewString()
	if len(idempotencyKeys) > 0 && idempotencyKeys[0] != "" {
		key = idempotencyKeys[0]
	}
	var existingWorkID uuid.UUID
	var existingText string
	err = tx.QueryRow(ctx, `SELECT work_item_id,message FROM work_control_messages WHERE run_id=$1 AND idempotency_key=$2`, runID, key).Scan(&existingWorkID, &existingText)
	if err == nil {
		if existingWorkID != workItemID || existingText != instructions {
			return ErrConflict
		}
		return nil
	}
	if err != pgx.ErrNoRows {
		return err
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_control_messages(run_id,work_item_id,epoch_number,message,idempotency_key) SELECT id,$2,current_epoch,$3,$4 FROM runs WHERE id=$1`, runID, workItemID, instructions, key); err != nil {
		return err
	}

	// Insert event
	eventPayload, _ := json.Marshal(map[string]any{
		"work_item_id": workItemID,
		"instructions": instructions,
		"override":     inputDataOverride,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'work_item_steered', $2)`,
		runID, eventPayload)
	if err != nil {
		return fmt.Errorf("insert steer event: %w", err)
	}

	// Record a strategic hint in the exploration graph so planners and workers discover it
	hintPayload, _ := json.Marshal(map[string]any{
		"text": fmt.Sprintf("[Steer]: %s", instructions),
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO exploration_nodes (run_id, kind, payload, state, origin,epoch_number)
		 VALUES ($1, 'hint', $2, 'open', 'human_steer',(SELECT current_epoch FROM runs WHERE id=$1))`,
		runID, hintPayload)
	if err != nil {
		return fmt.Errorf("insert steer hint: %w", err)
	}

	return tx.Commit(ctx)
}

// KillWorkItem cancels a single work item and its active attempt without stopping the whole run.
func (s *RunService) KillWorkItem(ctx context.Context, runID, workItemID uuid.UUID, reason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var runStatus model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if runStatus.IsTerminal() {
		return fmt.Errorf("%w: cannot kill work item in terminal run %s", ErrInvalidState, runStatus)
	}

	var wiRunID uuid.UUID
	var status model.WorkItemStatus
	err = tx.QueryRow(ctx,
		`SELECT run_id, status FROM work_items WHERE id = $1 FOR UPDATE`,
		workItemID,
	).Scan(&wiRunID, &status)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if wiRunID != runID {
		return fmt.Errorf("%w: work item belongs to another run", ErrCrossRunBoundary)
	}
	if status == model.WorkItemStatusCompleted || status == model.WorkItemStatusCancelled {
		return nil // already terminal
	}

	if reason == "" {
		reason = "killed_by_user"
	}

	// Cancel work item
	_, err = tx.Exec(ctx,
		`UPDATE work_items SET status = 'cancelled', updated_at = now() WHERE id = $1`,
		workItemID)
	if err != nil {
		return fmt.Errorf("update work item cancelled: %w", err)
	}

	// Stop running attempts
	now := time.Now()
	_, err = tx.Exec(ctx,
		`UPDATE attempts SET status = 'stopped', finish_reason = $1, finished_at = $2
		 WHERE work_item_id = $3 AND status = 'running'`,
		reason, now, workItemID)
	if err != nil {
		return fmt.Errorf("stop active attempts: %w", err)
	}

	// Stop running sessions
	_, err = tx.Exec(ctx,
		`UPDATE agent_sessions SET status = 'stopped', finish_reason = $1, updated_at = $2
		 WHERE work_item_id = $3 AND status = 'running'`,
		reason, now, workItemID)
	if err != nil {
		return fmt.Errorf("stop active sessions: %w", err)
	}

	// Record event
	eventPayload, _ := json.Marshal(map[string]any{
		"work_item_id": workItemID,
		"reason":       reason,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'work_item_killed', $2)`,
		runID, eventPayload)
	if err != nil {
		return fmt.Errorf("insert kill event: %w", err)
	}

	return tx.Commit(ctx)
}

// GrantBudget records additional authorized budget to the run ledger without creating a new Run.
func (s *RunService) GrantBudget(
	ctx context.Context,
	runID uuid.UUID,
	additionalModelCalls, additionalToolCalls, additionalSeconds int,
	reason string,
	idempotencyKey *string,
) (*model.BudgetGrant, error) {
	if additionalModelCalls < 0 || additionalToolCalls < 0 || additionalSeconds < 0 {
		return nil, fmt.Errorf("grant amounts must be non-negative")
	}
	if additionalModelCalls == 0 && additionalToolCalls == 0 && additionalSeconds == 0 {
		return nil, fmt.Errorf("at least one budget metric must be positive")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var currentEpoch int
	err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}

	if idempotencyKey != nil && *idempotencyKey != "" {
		var existing model.BudgetGrant
		err := tx.QueryRow(ctx,
			`SELECT id, run_id, epoch_number, granted_by, additional_model_calls, additional_tool_calls, additional_seconds, reason, idempotency_key, created_at
			 FROM budget_grants WHERE run_id = $1 AND idempotency_key = $2`,
			runID, *idempotencyKey).Scan(
			&existing.ID, &existing.RunID, &existing.EpochNumber, &existing.GrantedBy,
			&existing.AdditionalModelCalls, &existing.AdditionalToolCalls, &existing.AdditionalSeconds,
			&existing.Reason, &existing.IdempotencyKey, &existing.CreatedAt)
		if err == nil {
			if existing.AdditionalModelCalls != additionalModelCalls ||
				existing.AdditionalToolCalls != additionalToolCalls ||
				existing.AdditionalSeconds != additionalSeconds {
				return nil, fmt.Errorf("%w: budget grant idempotency key %s already used with different amounts", ErrConflict, *idempotencyKey)
			}
			return &existing, nil
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("check existing budget grant: %w", err)
		}
	}

	var grant model.BudgetGrant
	err = tx.QueryRow(ctx,
		`INSERT INTO budget_grants (run_id, epoch_number, granted_by, additional_model_calls, additional_tool_calls, additional_seconds, reason, idempotency_key)
		 VALUES ($1, $2, 'user', $3, $4, $5, $6, $7)
		 RETURNING id, run_id, epoch_number, granted_by, additional_model_calls, additional_tool_calls, additional_seconds, reason, idempotency_key, created_at`,
		runID, currentEpoch, additionalModelCalls, additionalToolCalls, additionalSeconds, reason, idempotencyKey,
	).Scan(
		&grant.ID, &grant.RunID, &grant.EpochNumber, &grant.GrantedBy,
		&grant.AdditionalModelCalls, &grant.AdditionalToolCalls, &grant.AdditionalSeconds,
		&grant.Reason, &grant.IdempotencyKey, &grant.CreatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert budget grant: %w", err)
	}

	eventPayload, _ := json.Marshal(grant)
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'budget_granted', $2)`,
		runID, eventPayload)
	if err != nil {
		return nil, fmt.Errorf("insert budget grant event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &grant, nil
}

// StartNewEpoch increments the execution epoch of a run and creates an epoch tracking record.
func (s *RunService) StartNewEpoch(ctx context.Context, runID uuid.UUID, reason, summary string, options ...EpochStartOptions) (int, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var currentEpoch int
	var runStatus model.RunStatus
	err = tx.QueryRow(ctx, `SELECT current_epoch,status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentEpoch, &runStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}

	opt := EpochStartOptions{}
	if len(options) > 0 {
		opt = options[0]
	}
	if opt.IdempotencyKey != "" {
		var storedHash string
		var result []byte
		err := tx.QueryRow(ctx, `SELECT request_hash,result FROM control_commands WHERE run_id=$1 AND kind='epoch_start' AND idempotency_key=$2`, runID, opt.IdempotencyKey).Scan(&storedHash, &result)
		if err == nil {
			if storedHash != epochCommandHash(reason, summary, opt.ExpectedEpoch) {
				return 0, ErrConflict
			}
			var saved struct {
				Epoch int `json:"epoch_number"`
			}
			if err := json.Unmarshal(result, &saved); err != nil {
				return 0, err
			}
			return saved.Epoch, nil
		}
		if err != pgx.ErrNoRows {
			return 0, err
		}
	}
	if opt.ExpectedEpoch > 0 && opt.ExpectedEpoch != currentEpoch {
		return 0, ErrStaleEpoch
	}
	if runStatus == model.RunStatusCompleted || runStatus == model.RunStatusStopping || strings.TrimSpace(reason) == "" {
		return 0, fmt.Errorf("%w: a reason and a resumable run are required", ErrInvalidState)
	}

	// Ensure current epoch record exists before closing it
	if _, err = tx.Exec(ctx,
		`INSERT INTO execution_epochs (run_id, epoch_number, status, started_at,finished_at,finish_reason,summary)
		 SELECT id,$2,CASE WHEN status IN ('completed','failed','stopped','interrupted') THEN status ELSE 'running' END,
		 COALESCE(started_at,now()),finished_at,finish_reason,final_output FROM runs WHERE id=$1
		 ON CONFLICT (run_id, epoch_number) DO NOTHING`,
		runID, currentEpoch); err != nil {
		return 0, err
	}

	// Close old epoch
	now := time.Now()
	if _, err = tx.Exec(ctx,
		`UPDATE execution_epochs
		 SET status = 'stopped', finished_at = COALESCE((SELECT finished_at FROM runs WHERE id=$4),$1), finish_reason = COALESCE((SELECT finish_reason FROM runs WHERE id=$4),$2), summary = COALESCE((SELECT final_output FROM runs WHERE id=$4),$3)
		 WHERE run_id = $4 AND status = 'running'`,
		now, reason, summary, runID); err != nil {
		return 0, err
	}

	// Revoke and isolate any unfinalized attempts or sessions from the old epoch
	if _, err = tx.Exec(ctx,
		`UPDATE attempts SET status = 'stopped', finish_reason = 'superseded_by_epoch', finished_at = $1
		 WHERE work_item_id IN (SELECT id FROM work_items WHERE run_id = $2) AND status = 'running'`,
		now, runID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx,
		`UPDATE agent_sessions SET status = 'stopped', finish_reason = 'superseded_by_epoch', updated_at = $1
		 WHERE run_id = $2 AND status = 'running'`,
		now, runID); err != nil {
		return 0, err
	}

	// Running work becomes an explicit continuation decision for the new Planner.
	// Preserve completed/failed/exhausted items and their original attempt history.
	if _, err = tx.Exec(ctx, `UPDATE work_items SET status='exhausted',updated_at=now() WHERE run_id=$1 AND status='running'`, runID); err != nil {
		return 0, err
	}
	// Run closeout cancels unfinished directions. Explicitly approving a new
	// epoch makes those directions eligible for Planner continuation; it does
	// not dispatch them. A direction explicitly killed by the operator stays
	// cancelled, regardless of the parent Run's later recovery.
	resumable := []uuid.UUID{}
	rows, err := tx.Query(ctx, `UPDATE work_items wi SET status='exhausted',updated_at=now()
		WHERE wi.run_id=$1 AND wi.status='cancelled' AND NOT EXISTS(
			SELECT 1 FROM run_events e WHERE e.run_id=$1 AND e.type='work_item_killed'
			AND e.payload->>'work_item_id'=wi.id::text)
		RETURNING wi.id`, runID)
	if err != nil {
		return 0, err
	}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		resumable = append(resumable, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE planner_rounds SET status='stopped',finished_at=now() WHERE run_id=$1 AND status='running'`, runID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE agent_tool_calls SET status='interrupted',finished_at=now(),error_message='superseded_by_epoch' WHERE run_id=$1 AND status IN ('pending','running')`, runID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE model_calls SET status='interrupted',finished_at=now(),error_message='superseded_by_epoch' WHERE run_id=$1 AND status='started'`, runID); err != nil {
		return 0, err
	}
	if _, err = tx.Exec(ctx, `UPDATE finding_retests SET status='stopped',finished_at=now() WHERE run_id=$1 AND status='running'`, runID); err != nil {
		return 0, err
	}

	newEpoch := currentEpoch + 1
	_, err = tx.Exec(ctx,
		`UPDATE runs SET current_epoch = $1, status = 'running', finished_at=NULL,finish_reason=NULL,final_output=NULL,updated_at = now() WHERE id = $2`,
		newEpoch, runID)
	if err != nil {
		return 0, fmt.Errorf("increment current_epoch: %w", err)
	}

	// Insert new epoch record
	_, err = tx.Exec(ctx,
		`INSERT INTO execution_epochs (run_id, epoch_number, status, started_at)
		 VALUES ($1, $2, 'running', now())`,
		runID, newEpoch)
	if err != nil {
		return 0, fmt.Errorf("insert execution epoch: %w", err)
	}

	eventPayload, _ := json.Marshal(map[string]any{
		"epoch":                   newEpoch,
		"reason":                  reason,
		"resumable_work_item_ids": resumable,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, (SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1), 'epoch_started', $2)`,
		runID, eventPayload)
	if err != nil {
		return 0, fmt.Errorf("insert epoch event: %w", err)
	}

	if opt.IdempotencyKey != "" {
		result, _ := json.Marshal(map[string]any{"epoch_number": newEpoch})
		if _, err = tx.Exec(ctx, `INSERT INTO control_commands(run_id,kind,idempotency_key,request_hash,result) VALUES($1,'epoch_start',$2,$3,$4)`, runID, opt.IdempotencyKey, epochCommandHash(reason, summary, opt.ExpectedEpoch), result); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newEpoch, nil
}

// ValidateEpoch checks whether a caller's epoch matches the run's current epoch.
func (s *RunService) ValidateEpoch(ctx context.Context, runID uuid.UUID, callerEpoch int) error {
	var currentEpoch int
	err := s.pool.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id = $1`, runID).Scan(&currentEpoch)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		return err
	}
	if callerEpoch != currentEpoch {
		return fmt.Errorf("%w: caller epoch %d does not match current epoch %d", ErrStaleEpoch, callerEpoch, currentEpoch)
	}
	return nil
}

type BudgetGrantTotal struct {
	AdditionalModelCalls int
	AdditionalToolCalls  int
	AdditionalSeconds    int
}

func (s *RunService) GetBudgetGrantsTotal(ctx context.Context, runID uuid.UUID) (BudgetGrantTotal, error) {
	var total BudgetGrantTotal
	err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(SUM(additional_model_calls), 0),
		        COALESCE(SUM(additional_tool_calls), 0),
		        COALESCE(SUM(additional_seconds), 0)
		 FROM budget_grants
		 WHERE run_id = $1`,
		runID,
	).Scan(&total.AdditionalModelCalls, &total.AdditionalToolCalls, &total.AdditionalSeconds)
	if err != nil {
		return total, fmt.Errorf("sum budget grants: %w", err)
	}
	return total, nil
}

// GetEffectiveBudget computes the total authorized budget = initial budget + all granted budgets.
func (s *RunService) GetEffectiveBudget(ctx context.Context, runID uuid.UUID) (model.BudgetConfig, error) {
	run, err := s.Get(ctx, runID)
	if err != nil {
		return model.BudgetConfig{}, err
	}

	var base model.BudgetConfig
	if len(run.ExecutionInput) > 0 {
		var inMap map[string]any
		if err := json.Unmarshal(run.ExecutionInput, &inMap); err == nil {
			if bBytes, err := json.Marshal(inMap["budget"]); err == nil {
				_ = json.Unmarshal(bBytes, &base)
			}
		}
	}
	if base.MaxModelCalls == 0 && len(run.ConfigSnapshot) > 0 {
		var cfg map[string]any
		if err := json.Unmarshal(run.ConfigSnapshot, &cfg); err == nil {
			if bBytes, err := json.Marshal(cfg["budget"]); err == nil {
				_ = json.Unmarshal(bBytes, &base)
			}
		}
	}

	grants, err := s.GetBudgetGrantsTotal(ctx, runID)
	if err != nil {
		return base, err
	}

	base.MaxModelCalls += grants.AdditionalModelCalls
	base.MaxToolCalls += grants.AdditionalToolCalls
	base.RunTimeoutSeconds += grants.AdditionalSeconds

	return base, nil
}

// ListBudgetGrants retrieves all authorized grants for a run.
func (s *RunService) ListBudgetGrants(ctx context.Context, runID uuid.UUID) ([]model.BudgetGrant, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, epoch_number, granted_by, additional_model_calls, additional_tool_calls, additional_seconds, reason, idempotency_key, created_at
		 FROM budget_grants
		 WHERE run_id = $1
		 ORDER BY created_at ASC`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("query budget grants: %w", err)
	}
	defer rows.Close()

	var grants []model.BudgetGrant
	for rows.Next() {
		var g model.BudgetGrant
		if err := rows.Scan(
			&g.ID, &g.RunID, &g.EpochNumber, &g.GrantedBy,
			&g.AdditionalModelCalls, &g.AdditionalToolCalls, &g.AdditionalSeconds,
			&g.Reason, &g.IdempotencyKey, &g.CreatedAt,
		); err != nil {
			return nil, err
		}
		grants = append(grants, g)
	}
	if grants == nil {
		grants = []model.BudgetGrant{}
	}
	return grants, nil
}

// ListExecutionEpochs retrieves the history of execution epochs for a run.
func (s *RunService) ListExecutionEpochs(ctx context.Context, runID uuid.UUID) ([]model.ExecutionEpoch, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, epoch_number, status, started_at, finished_at, finish_reason, summary, created_at
		 FROM execution_epochs
		 WHERE run_id = $1
		 ORDER BY epoch_number ASC`,
		runID)
	if err != nil {
		return nil, fmt.Errorf("query execution epochs: %w", err)
	}
	defer rows.Close()

	var epochs []model.ExecutionEpoch
	for rows.Next() {
		var ep model.ExecutionEpoch
		if err := rows.Scan(
			&ep.ID, &ep.RunID, &ep.EpochNumber, &ep.Status,
			&ep.StartedAt, &ep.FinishedAt, &ep.FinishReason, &ep.Summary, &ep.CreatedAt,
		); err != nil {
			return nil, err
		}
		epochs = append(epochs, ep)
	}
	if epochs == nil {
		epochs = []model.ExecutionEpoch{}
	}
	return epochs, nil
}

// ExecutionBudgetStart excludes time between epochs, preserving elapsed execution
// time across resumes. Counts remain cumulative across the entire Run.
func (s *RunService) ExecutionBudgetStart(ctx context.Context, runID uuid.UUID) (time.Time, error) {
	var started time.Time
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(e.started_at,r.started_at,now()) -
		COALESCE((SELECT SUM(GREATEST(finished_at-started_at,interval '0')) FROM execution_epochs WHERE run_id=r.id AND epoch_number<r.current_epoch),interval '0')
		FROM runs r LEFT JOIN execution_epochs e ON e.run_id=r.id AND e.epoch_number=r.current_epoch WHERE r.id=$1`, runID).Scan(&started)
	return started, err
}
