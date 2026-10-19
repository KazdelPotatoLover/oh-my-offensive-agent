package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func Connect(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, fmt.Errorf("unable to create connection pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("unable to ping database: %w", err)
	}
	return pool, nil
}

func RunMigrations(databaseURL, migrationsPath string) error {
	m, err := migrate.New(
		"file://"+migrationsPath,
		databaseURL,
	)
	if err != nil {
		return fmt.Errorf("failed to create migrate instance: %w", err)
	}
	defer m.Close()

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("failed to run migrations: %w", err)
	}
	return nil
}

func MarkInterruptedRuns(ctx context.Context, pool *pgxpool.Pool) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Get all active runs
	rows, err := tx.Query(ctx,
		`SELECT id, status FROM runs WHERE status IN ('queued', 'running', 'stopping')`)
	if err != nil {
		return fmt.Errorf("failed to query active runs: %w", err)
	}

	type activeRun struct {
		ID     uuid.UUID
		Status string
	}
	var activeRuns []activeRun
	for rows.Next() {
		var r activeRun
		if err := rows.Scan(&r.ID, &r.Status); err != nil {
			return fmt.Errorf("failed to scan run: %w", err)
		}
		activeRuns = append(activeRuns, r)
	}
	rows.Close()

	if len(activeRuns) == 0 {
		slog.Info("no active runs to mark as interrupted")
		return nil
	}

	for _, r := range activeRuns {
		// Update run status
		_, err := tx.Exec(ctx,
			`UPDATE runs SET status = 'interrupted', finish_reason = 'server restarted', 
			 finished_at = now(), updated_at = now() WHERE id = $1`, r.ID)
		if err != nil {
			return fmt.Errorf("failed to update run %s: %w", r.ID, err)
		}

		// Get next seq
		var nextSeq int
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, r.ID).Scan(&nextSeq)
		if err != nil {
			return fmt.Errorf("failed to get next seq for run %s: %w", r.ID, err)
		}

		// Insert state_change event
		payload, _ := json.Marshal(map[string]interface{}{
			"from": r.Status,
			"to":   "interrupted",
		})
		_, err = tx.Exec(ctx,
			`INSERT INTO run_events (id, run_id, seq, type, payload) 
			 VALUES (gen_random_uuid(), $1, $2, 'state_change', $3)`,
			r.ID, nextSeq, payload)
		if err != nil {
			return fmt.Errorf("failed to insert event for run %s: %w", r.ID, err)
		}

		slog.Info("marked run as interrupted", "run_id", r.ID, "old_status", r.Status)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("failed to commit transaction: %w", err)
	}

	slog.Info("marked interrupted runs", "count", len(activeRuns))
	return nil
}
