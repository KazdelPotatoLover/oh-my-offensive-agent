package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

type RunService struct {
	pool *pgxpool.Pool
}

func NewRunService(pool *pgxpool.Pool) *RunService {
	return &RunService{pool: pool}
}

func (s *RunService) Pool() *pgxpool.Pool {
	return s.pool
}

func (s *RunService) ListByTask(ctx context.Context, taskID uuid.UUID) ([]model.Run, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
		        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
		 FROM runs WHERE task_id = $1 ORDER BY created_at DESC`, taskID)
	if err != nil {
		return nil, fmt.Errorf("query runs: %w", err)
	}
	defer rows.Close()

	var runs []model.Run
	for rows.Next() {
		var r model.Run
		if err := rows.Scan(&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan run: %w", err)
		}
		runs = append(runs, r)
	}
	if runs == nil {
		runs = []model.Run{}
	}
	return runs, nil
}

func (s *RunService) Get(ctx context.Context, id uuid.UUID) (*model.Run, error) {
	var r model.Run
	err := s.pool.QueryRow(ctx,
		`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
		        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
		 FROM runs WHERE id = $1`, id).Scan(
		&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
		&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query run: %w", err)
	}
	return &r, nil
}

// Create creates a new run for a task. Returns the run, whether it already existed (idempotent hit), and any error.
func (s *RunService) Create(ctx context.Context, taskID uuid.UUID, req model.CreateRunRequest, idempotencyKey *string) (*model.Run, bool, error) {
	// If idempotency key provided, check for existing run
	if idempotencyKey != nil && *idempotencyKey != "" {
		var existing model.Run
		err := s.pool.QueryRow(ctx,
			`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
			        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
			 FROM runs WHERE task_id = $1 AND idempotency_key = $2`,
			taskID, *idempotencyKey).Scan(
			&existing.ID, &existing.TaskID, &existing.IdempotencyKey, &existing.ConfigSnapshot, &existing.ExecutionInput,
			&existing.Status, &existing.FinishReason, &existing.FailAtStep, &existing.FinalOutput, &existing.StartedAt,
			&existing.FinishedAt, &existing.CreatedAt, &existing.UpdatedAt)
		if err == nil {
			// Found existing - check if params match
			if !runRequestMatches(existing, req) {
				return nil, false, ErrConflict
			}
			return &existing, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return nil, false, fmt.Errorf("check idempotency: %w", err)
		}
	}

	// Get task to snapshot full execution input (objective, scope, config, and parameters)
	var taskName string
	var taskObjective string
	var taskScope json.RawMessage
	var taskConfig json.RawMessage
	err := s.pool.QueryRow(ctx,
		`SELECT name, objective, scope, config FROM tasks WHERE id = $1`, taskID).Scan(
		&taskName, &taskObjective, &taskScope, &taskConfig)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, fmt.Errorf("get task config: %w", err)
	}

	// Resolve executor configuration: defaults to "fake" if not specified
	executor := "fake"
	providerType := "mock"
	mockScenario := "normal"
	budget := model.DefaultBudgetConfig()

	// Check Task.Config
	if len(taskConfig) > 0 && string(taskConfig) != "{}" {
		var cfgMap map[string]interface{}
		if err := json.Unmarshal(taskConfig, &cfgMap); err == nil {
			if ex, ok := cfgMap["executor"].(string); ok && ex != "" {
				executor = ex
			}
			if prov, ok := cfgMap["provider"].(string); ok && prov != "" {
				providerType = prov
			}
			if scen, ok := cfgMap["mock_scenario"].(string); ok && scen != "" {
				mockScenario = scen
			}
			if bMap, ok := cfgMap["budget"].(map[string]interface{}); ok {
				if bBytes, err := json.Marshal(bMap); err == nil {
					json.Unmarshal(bBytes, &budget)
				}
			}
		}
	}

	// Request parameters override Task.Config
	if req.Executor != nil && *req.Executor != "" {
		executor = *req.Executor
	}
	if req.Provider != nil && *req.Provider != "" {
		providerType = *req.Provider
	}
	if req.MockScenario != nil && *req.MockScenario != "" {
		mockScenario = *req.MockScenario
	}
	if req.Budget != nil {
		budget = *req.Budget
	}

	// If real model is selected, validate environment configuration!
	var sanitizedEndpoint string
	var modelName string
	if executor == "agent" && (providerType == "openai" || providerType == "remote") {
		apiKey := os.Getenv("LLM_API_KEY")
		baseURL := os.Getenv("LLM_BASE_URL")
		modelName = os.Getenv("LLM_MODEL")
		if apiKey == "" || baseURL == "" || modelName == "" {
			return nil, false, provider.ErrModelConfigMissing
		}
		sanitizedEndpoint = baseURL
	}

	executionInputMap := map[string]interface{}{
		"objective": taskObjective,
		"scope":     taskScope,
		"config":    taskConfig,
		"parameters": map[string]interface{}{
			"fail_at_step": req.FailAtStep,
		},
	}
	if executor == "agent" {
		agentConfig := map[string]interface{}{
			"executor":       "agent",
			"provider":       providerType,
			"prompt_version": "v1",
			"tools_version":  "v1",
			"budget":         budget,
		}
		if providerType == "mock" {
			agentConfig["mock_scenario"] = mockScenario
		} else {
			agentConfig["model"] = modelName
			agentConfig["endpoint"] = sanitizedEndpoint
		}
		executionInputMap["agent_config"] = agentConfig
		executionInputMap["budget"] = budget
	}

	executionInputBytes, err := json.Marshal(executionInputMap)
	if err != nil {
		return nil, false, fmt.Errorf("marshal execution input: %w", err)
	}

	// Create run in transaction
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var r model.Run
	err = tx.QueryRow(ctx,
		`INSERT INTO runs (task_id, idempotency_key, config_snapshot, execution_input, status, fail_at_step) 
		 VALUES ($1, $2, $3, $4, 'queued', $5) 
		 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
		          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
		taskID, idempotencyKey, taskConfig, executionInputBytes, req.FailAtStep).Scan(
		&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
		&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if idempotencyKey != nil && *idempotencyKey != "" {
				// Check if conflict is due to concurrent insert with same idempotency key
				var existing model.Run
				readErr := s.pool.QueryRow(ctx,
					`SELECT id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
					        fail_at_step, final_output, started_at, finished_at, created_at, updated_at 
					 FROM runs WHERE task_id = $1 AND idempotency_key = $2`,
					taskID, *idempotencyKey).Scan(
					&existing.ID, &existing.TaskID, &existing.IdempotencyKey, &existing.ConfigSnapshot, &existing.ExecutionInput,
					&existing.Status, &existing.FinishReason, &existing.FailAtStep, &existing.FinalOutput, &existing.StartedAt,
					&existing.FinishedAt, &existing.CreatedAt, &existing.UpdatedAt)
				if readErr == nil {
					if runRequestMatches(existing, req) {
						return &existing, true, nil
					}
					return nil, false, ErrConflict
				}
			}
			if pgErr.ConstraintName == "idx_runs_one_active_per_task" {
				return nil, false, ErrAlreadyActive
			}
			if pgErr.ConstraintName == "idx_runs_idempotency" {
				return nil, false, ErrConflict
			}
		}
		return nil, false, fmt.Errorf("insert run: %w", err)
	}

	// Insert initial event
	payload, _ := json.Marshal(map[string]interface{}{
		"from": nil,
		"to":   "queued",
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, 1, 'state_change', $2)`,
		r.ID, payload)
	if err != nil {
		return nil, false, fmt.Errorf("insert initial event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, false, fmt.Errorf("commit transaction: %w", err)
	}

	slog.Info("created run", "run_id", r.ID, "task_id", taskID, "task_name", taskName)
	return &r, false, nil
}

func (s *RunService) Stop(ctx context.Context, id uuid.UUID) (*model.Run, error) {
	// Get current run
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	if current.Status.IsTerminal() {
		return nil, fmt.Errorf("%w: run is already %s", ErrTerminalState, current.Status)
	}

	var newStatus model.RunStatus
	switch current.Status {
	case model.RunStatusQueued, model.RunStatusRunning:
		newStatus = model.RunStatusStopping
	case model.RunStatusStopping:
		// Already stopping
		return current, nil
	default:
		return nil, fmt.Errorf("%w: cannot stop run in status %s", ErrInvalidTransition, current.Status)
	}

	run, err := s.TransitionState(ctx, id, current.Status, newStatus, nil)
	if err != nil {
		return nil, err
	}
	return run, nil
}

// TransitionState atomically updates run status and inserts a state_change event.
func (s *RunService) TransitionState(ctx context.Context, runID uuid.UUID, fromStatus, toStatus model.RunStatus, finishReason *string) (*model.Run, error) {
	if !model.IsAllowedTransition(fromStatus, toStatus) {
		return nil, fmt.Errorf("%w: transition from %s to %s is not permitted by state machine rules", ErrInvalidTransition, fromStatus, toStatus)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify current status and lock the row
	var currentStatus model.RunStatus
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get run for update: %w", err)
	}

	if currentStatus.IsTerminal() {
		return nil, fmt.Errorf("%w: run is already in terminal status %s", ErrTerminalState, currentStatus)
	}

	if currentStatus != fromStatus {
		return nil, fmt.Errorf("%w: expected status %s but got %s", ErrInvalidTransition, fromStatus, currentStatus)
	}

	if !model.IsAllowedTransition(currentStatus, toStatus) {
		return nil, fmt.Errorf("%w: transition from %s to %s is not permitted by state machine rules", ErrInvalidTransition, currentStatus, toStatus)
	}

	// Build update query
	now := time.Now()
	var r model.Run
	if toStatus.IsTerminal() {
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, finish_reason = $2, finished_at = $3, updated_at = $3 
			 WHERE id = $4 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			toStatus, finishReason, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
	} else if toStatus == model.RunStatusRunning {
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, started_at = $2, updated_at = $2 
			 WHERE id = $3 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			toStatus, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
	} else {
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, updated_at = $2 
			 WHERE id = $3 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			toStatus, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
	}
	if err != nil {
		return nil, fmt.Errorf("update run status: %w", err)
	}

	// Get next seq
	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, fmt.Errorf("get next seq: %w", err)
	}

	// Insert state_change event
	payload, _ := json.Marshal(map[string]interface{}{
		"from": string(fromStatus),
		"to":   string(toStatus),
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, $2, 'state_change', $3)`,
		runID, nextSeq, payload)
	if err != nil {
		return nil, fmt.Errorf("insert state_change event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit transition: %w", err)
	}

	slog.Info("run state transition", "run_id", runID, "from", fromStatus, "to", toStatus)
	return &r, nil
}

// InsertProgressEvent inserts a progress event for a run.
func (s *RunService) InsertProgressEvent(ctx context.Context, runID uuid.UUID, step, total int, message string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify run is still running
	var status model.RunStatus
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&status)
	if err != nil {
		return fmt.Errorf("get run status: %w", err)
	}
	if status.IsTerminal() {
		slog.Warn("skipping progress event for terminal run", "run_id", runID, "status", status)
		return ErrTerminalState
	}
	if status != model.RunStatusRunning {
		slog.Warn("skipping progress event for non-running run", "run_id", runID, "status", status)
		return fmt.Errorf("%w: run is in status %s", ErrInvalidTransition, status)
	}

	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return fmt.Errorf("get next seq: %w", err)
	}

	payload, _ := json.Marshal(map[string]interface{}{
		"step":    step,
		"total":   total,
		"message": message,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, $2, 'progress', $3)`,
		runID, nextSeq, payload)
	if err != nil {
		return fmt.Errorf("insert progress event: %w", err)
	}

	return tx.Commit(ctx)
}

func (s *RunService) GetEvents(ctx context.Context, runID uuid.UUID) ([]model.RunEvent, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, seq, type, payload, created_at 
		 FROM run_events WHERE run_id = $1 ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var events []model.RunEvent
	for rows.Next() {
		var e model.RunEvent
		if err := rows.Scan(&e.ID, &e.RunID, &e.Seq, &e.Type, &e.Payload, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, e)
	}
	if events == nil {
		events = []model.RunEvent{}
	}
	return events, nil
}

func intPtrEqual(a, b *int) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func runRequestMatches(existing model.Run, req model.CreateRunRequest) bool {
	if !intPtrEqual(existing.FailAtStep, req.FailAtStep) {
		return false
	}
	if len(existing.ExecutionInput) > 0 {
		var inMap map[string]interface{}
		if err := json.Unmarshal(existing.ExecutionInput, &inMap); err == nil {
			if req.Executor != nil && *req.Executor != "" {
				if ex, _ := inMap["executor"].(string); ex != "" && ex != *req.Executor {
					return false
				}
			}
			if req.Provider != nil && *req.Provider != "" {
				if ac, ok := inMap["agent_config"].(map[string]interface{}); ok {
					if p, _ := ac["provider"].(string); p != "" && p != *req.Provider {
						return false
					}
				}
			}
			if req.MockScenario != nil && *req.MockScenario != "" {
				if ac, ok := inMap["agent_config"].(map[string]interface{}); ok {
					if sc, _ := ac["mock_scenario"].(string); sc != "" && sc != *req.MockScenario {
						return false
					}
				}
			}
		}
	}
	return true
}

