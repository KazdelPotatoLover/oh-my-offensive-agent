package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	ErrNotFound       = errors.New("not found")
	ErrHasActiveRuns  = errors.New("task has active runs")
	ErrAlreadyActive  = errors.New("task already has an active run")
	ErrConflict       = errors.New("idempotency key conflict: same key but different request parameters")
	ErrTerminalState  = errors.New("run is in a terminal state")
	ErrInvalidTransition = errors.New("invalid state transition")
)

type TaskService struct {
	pool *pgxpool.Pool
}

func NewTaskService(pool *pgxpool.Pool) *TaskService {
	return &TaskService{pool: pool}
}

func (s *TaskService) List(ctx context.Context) ([]model.Task, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, objective, scope, config, created_at, updated_at 
		 FROM tasks ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("query tasks: %w", err)
	}
	defer rows.Close()

	var tasks []model.Task
	for rows.Next() {
		var t model.Task
		if err := rows.Scan(&t.ID, &t.Name, &t.Objective, &t.Scope, &t.Config, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if tasks == nil {
		tasks = []model.Task{}
	}
	return tasks, nil
}

func (s *TaskService) Get(ctx context.Context, id uuid.UUID) (*model.Task, error) {
	var t model.Task
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, objective, scope, config, created_at, updated_at 
		 FROM tasks WHERE id = $1`, id).Scan(
		&t.ID, &t.Name, &t.Objective, &t.Scope, &t.Config, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("query task: %w", err)
	}
	return &t, nil
}

func (s *TaskService) Create(ctx context.Context, req model.CreateTaskRequest) (*model.Task, error) {
	scope := req.Scope
	if scope == nil {
		scope = json.RawMessage(`{}`)
	}
	cfg := req.Config
	if cfg == nil {
		cfg = json.RawMessage(`{}`)
	}

	var t model.Task
	err := s.pool.QueryRow(ctx,
		`INSERT INTO tasks (name, objective, scope, config) 
		 VALUES ($1, $2, $3, $4) 
		 RETURNING id, name, objective, scope, config, created_at, updated_at`,
		req.Name, req.Objective, scope, cfg).Scan(
		&t.ID, &t.Name, &t.Objective, &t.Scope, &t.Config, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert task: %w", err)
	}
	return &t, nil
}

func (s *TaskService) Update(ctx context.Context, id uuid.UUID, req model.UpdateTaskRequest) (*model.Task, error) {
	// Get current task
	current, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}

	name := current.Name
	if req.Name != nil {
		name = *req.Name
	}
	objective := current.Objective
	if req.Objective != nil {
		objective = *req.Objective
	}
	scope := current.Scope
	if req.Scope != nil {
		scope = *req.Scope
	}
	cfg := current.Config
	if req.Config != nil {
		cfg = *req.Config
	}

	var t model.Task
	err = s.pool.QueryRow(ctx,
		`UPDATE tasks SET name = $1, objective = $2, scope = $3, config = $4, updated_at = now() 
		 WHERE id = $5 
		 RETURNING id, name, objective, scope, config, created_at, updated_at`,
		name, objective, scope, cfg, id).Scan(
		&t.ID, &t.Name, &t.Objective, &t.Scope, &t.Config, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("update task: %w", err)
	}
	return &t, nil
}

func (s *TaskService) Delete(ctx context.Context, id uuid.UUID) error {
	// Check for active runs
	var count int
	err := s.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM runs WHERE task_id = $1 AND status IN ('queued', 'running', 'stopping')`,
		id).Scan(&count)
	if err != nil {
		return fmt.Errorf("check active runs: %w", err)
	}
	if count > 0 {
		return ErrHasActiveRuns
	}

	result, err := s.pool.Exec(ctx, `DELETE FROM tasks WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete task: %w", err)
	}
	if result.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}
