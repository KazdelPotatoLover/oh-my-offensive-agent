package test

import (
	"context"
	"os"
	"testing"

	"agent-workbench/backend/internal/model"
)

func TestBudgetMigration_UpDownAndHistoricalStateGuard(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	down, err := os.ReadFile("../migrations/012_budget_and_observation_semantics.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	up, err := os.ReadFile("../migrations/012_budget_and_observation_semantics.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	task := createTestTask(t, env, "migration semantics")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := env.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, string(down)); err != nil {
		t.Fatalf("downgrade empty new states: %v", err)
	}
	if _, err = tx.Exec(ctx, string(up)); err != nil {
		t.Fatalf("upgrade: %v", err)
	}
	if _, err = tx.Exec(ctx, `INSERT INTO work_items(run_id,key,title,status) VALUES($1,'exhausted','Exhausted','exhausted')`, run.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `SAVEPOINT before_downgrade`); err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, string(down)); err == nil {
		t.Fatal("downgrade silently accepted new historical state")
	}
	if _, err = tx.Exec(ctx, `ROLLBACK TO SAVEPOINT before_downgrade`); err != nil {
		t.Fatal(err)
	}
	var status string
	if err = tx.QueryRow(ctx, `SELECT status FROM work_items WHERE run_id=$1`, run.ID).Scan(&status); err != nil || status != "exhausted" {
		t.Fatalf("historical state changed: %s %v", status, err)
	}
}
