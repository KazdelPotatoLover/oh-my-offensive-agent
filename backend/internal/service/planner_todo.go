package service

import (
	"context"
	"fmt"

	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ListPlannerTodos retrieves all planner todos for a given run ordered by creation time.
func (s *RunService) ListPlannerTodos(ctx context.Context, runID uuid.UUID) ([]model.PlannerTodo, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, item_key, description, status, depends_on, version, created_at, updated_at
		 FROM planner_todos
		 WHERE run_id = $1
		 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query planner todos: %w", err)
	}
	defer rows.Close()

	var todos []model.PlannerTodo
	for rows.Next() {
		var t model.PlannerTodo
		if err := rows.Scan(&t.ID, &t.RunID, &t.ItemKey, &t.Description, &t.Status, &t.DependsOn, &t.Version, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan planner todo: %w", err)
		}
		todos = append(todos, t)
	}
	if todos == nil {
		todos = []model.PlannerTodo{}
	}
	return todos, nil
}

// AddOrUpdatePlannerTodo creates or updates a persistent planner todo item with CAS versioning.
func (s *RunService) AddOrUpdatePlannerTodo(
	ctx context.Context,
	runID uuid.UUID,
	itemKey, description, status string,
	dependsOn []string,
) (*model.PlannerTodo, error) {
	if itemKey == "" {
		return nil, fmt.Errorf("item_key cannot be empty")
	}
	if status == "" {
		status = "pending"
	}
	if dependsOn == nil {
		dependsOn = []string{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var todo model.PlannerTodo
	var existingID uuid.UUID
	var existingVersion int
	err = tx.QueryRow(ctx,
		`SELECT id, version FROM planner_todos WHERE run_id = $1 AND item_key = $2 FOR UPDATE`,
		runID, itemKey).Scan(&existingID, &existingVersion)

	if err != nil && err != pgx.ErrNoRows {
		return nil, fmt.Errorf("check existing todo: %w", err)
	}

	if err == pgx.ErrNoRows {
		// Insert new
		err = tx.QueryRow(ctx,
			`INSERT INTO planner_todos (id, run_id, item_key, description, status, depends_on, version, created_at, updated_at)
			 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 1, now(), now())
			 RETURNING id, run_id, item_key, description, status, depends_on, version, created_at, updated_at`,
			runID, itemKey, description, status, dependsOn).Scan(
			&todo.ID, &todo.RunID, &todo.ItemKey, &todo.Description, &todo.Status, &todo.DependsOn, &todo.Version, &todo.CreatedAt, &todo.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("insert planner todo: %w", err)
		}
	} else {
		// Update existing with version bump
		err = tx.QueryRow(ctx,
			`UPDATE planner_todos
			 SET description = $1, status = $2, depends_on = $3, version = version + 1, updated_at = now()
			 WHERE id = $4
			 RETURNING id, run_id, item_key, description, status, depends_on, version, created_at, updated_at`,
			description, status, dependsOn, existingID).Scan(
			&todo.ID, &todo.RunID, &todo.ItemKey, &todo.Description, &todo.Status, &todo.DependsOn, &todo.Version, &todo.CreatedAt, &todo.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("update planner todo: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit planner todo: %w", err)
	}
	return &todo, nil
}

// DropPlannerTodo marks a planner todo as dropped.
func (s *RunService) DropPlannerTodo(ctx context.Context, runID uuid.UUID, itemKey string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE planner_todos SET status = 'dropped', updated_at = now() WHERE run_id = $1 AND item_key = $2`,
		runID, itemKey)
	return err
}
