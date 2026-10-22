package execution

import (
	"context"
	"errors"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const ma4DatabaseURL = "postgres://postgres:postgres@127.0.0.1:25432/agent_workbench_ma4?sslmode=disable"

var (
	ma4Once sync.Once
	ma4Pool *pgxpool.Pool
	ma4Err  error
)

type ma4Harness struct {
	t         *testing.T
	pool      *pgxpool.Pool
	broker    *Broker
	server    *httptest.Server
	exec      *Executor
	root      string
	brokerID  string
	decoys    []string
	runIDs    []uuid.UUID
	taskIDs   []uuid.UUID
	assetIDs  []uuid.UUID
}

func migrationsDir(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("migration path is unavailable")
	}
	dir := filepath.Join(filepath.Dir(file), "..", "..", "migrations")
	if _, err := os.Stat(filepath.Join(dir, "029_execution_jobs.up.sql")); err != nil {
		t.Fatalf("migration 029 is not in %s", dir)
	}
	return dir
}

func openMa4(t *testing.T) *pgxpool.Pool {
	t.Helper()
	parsed, err := url.Parse(ma4DatabaseURL)
	if err != nil || parsed.Path != "/agent_workbench_ma4" {
		t.Fatalf("refusing database URL %q", ma4DatabaseURL)
	}
	if parsed.Path == "/agent_workbench" || strings.Contains(parsed.Path, "agent_workbench_test") || strings.Contains(parsed.Path, "oma_artex_test") {
		t.Fatal("refusing a shared database")
	}
	ma4Once.Do(func() {
		if err := db.RunMigrations(ma4DatabaseURL, migrationsDir(t)); err != nil {
			ma4Err = err
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		pool, err := db.Connect(ctx, ma4DatabaseURL)
		if err != nil {
			ma4Err = err
			return
		}
		var name string
		if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name); err != nil {
			pool.Close()
			ma4Err = err
			return
		}
		if name != "agent_workbench_ma4" {
			pool.Close()
			ma4Err = fmt.Errorf("connected to %s", name)
			return
		}
		ma4Pool = pool
	})
	if ma4Err != nil {
		t.Fatal(ma4Err)
	}
	return ma4Pool
}

func newMa4Harness(t *testing.T, cfg Config) *ma4Harness {
	t.Helper()
	if _, err := os.Stat("/var/run/docker.sock"); err != nil {
		t.Fatalf("docker socket is required for execution acceptance: %v", err)
	}
	pool := openMa4(t)
	root := t.TempDir()
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if len(cfg.AllowedImages) == 0 {
		cfg.AllowedImages = []string{cfg.Image}
	}
	cfg.WorkspaceRoot = root
	cfg.DockerSocket = "/var/run/docker.sock"
	cfg.BrokerToken = "ma4-" + uuid.NewString()
	brokerID := uuid.NewString()
	broker, err := NewBroker(cfg, brokerID)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(broker.Handler())
	exec, err := NewExecutor(cfg, pool, server.URL, nil)
	if err != nil {
		server.Close()
		t.Fatal(err)
	}
	h := &ma4Harness{
		t: t, pool: pool, broker: broker, server: server, exec: exec,
		root: root, brokerID: brokerID,
	}
	t.Cleanup(h.cleanup)
	return h
}

func (h *ma4Harness) cleanup() {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, _ = h.broker.Cleanup(ctx)
	h.server.Close()
	if api, err := newDockerAPI("/var/run/docker.sock"); err == nil {
		for _, id := range h.decoys {
			_ = api.killContainer(ctx, id, "SIGKILL")
			_ = api.removeContainer(ctx, id)
		}
	}
	for _, runID := range h.runIDs {
		h.deleteRun(ctx, runID)
	}
	for _, assetID := range h.assetIDs {
		_, _ = h.pool.Exec(ctx, `DELETE FROM assets WHERE id=$1`, assetID)
	}
	for _, taskID := range h.taskIDs {
		_, _ = h.pool.Exec(ctx, `DELETE FROM tasks WHERE id=$1`, taskID)
	}
}

func (h *ma4Harness) deleteRun(ctx context.Context, runID uuid.UUID) {
	stmts := []string{
		`DELETE FROM execution_job_traffic WHERE job_id IN (SELECT id FROM execution_jobs WHERE run_id=$1)`,
		`DELETE FROM execution_job_artifacts WHERE job_id IN (SELECT id FROM execution_jobs WHERE run_id=$1)`,
		`DELETE FROM execution_files WHERE run_id=$1`,
		`DELETE FROM execution_jobs WHERE run_id=$1`,
		`DELETE FROM execution_workspaces WHERE run_id=$1`,
		`DELETE FROM findings WHERE run_id=$1`,
		`DELETE FROM check_results WHERE run_id=$1`,
		`DELETE FROM exploration_changes WHERE run_id=$1`,
		`DELETE FROM artifacts WHERE run_id=$1`,
		`DELETE FROM work_item_assets WHERE work_item_id IN (SELECT id FROM work_items WHERE run_id=$1)`,
		`DELETE FROM attempts WHERE work_item_id IN (SELECT id FROM work_items WHERE run_id=$1)`,
		`DELETE FROM work_items WHERE run_id=$1`,
		`DELETE FROM run_assets WHERE run_id=$1`,
		`DELETE FROM runs WHERE id=$1`,
	}
	for _, stmt := range stmts {
		if _, err := h.pool.Exec(ctx, stmt, runID); err != nil {
			h.t.Errorf("cleanup %s: %v", stmt, err)
		}
	}
}

type ma4Scope struct {
	TaskID     uuid.UUID
	RunID      uuid.UUID
	WorkItemID uuid.UUID
	AttemptID  uuid.UUID
	Scope      contract.ExecutionScope
}

func (h *ma4Harness) seed(t *testing.T) ma4Scope {
	t.Helper()
	ctx := context.Background()
	out := ma4Scope{
		TaskID:     uuid.New(),
		RunID:      uuid.New(),
		WorkItemID: uuid.New(),
		AttemptID:  uuid.New(),
	}
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `INSERT INTO tasks (id, name) VALUES ($1, 'ma4')`, out.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO runs (id, task_id, status, current_epoch) VALUES ($1, $2, 'running', 1)`, out.RunID, out.TaskID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO work_items (id, run_id, key, title) VALUES ($1, $2, $3, 'w')`, out.WorkItemID, out.RunID, "w-"+out.WorkItemID.String()[:8]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO attempts (id, work_item_id, attempt_number, epoch_number, status) VALUES ($1, $2, 1, 1, 'running')`, out.AttemptID, out.WorkItemID); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	h.taskIDs = append(h.taskIDs, out.TaskID)
	h.runIDs = append(h.runIDs, out.RunID)
	out.Scope = contract.ExecutionScope{
		RunID:       out.RunID,
		EpochNumber: 1,
		Role:        contract.RoleWorker,
		WorkItemID:  &out.WorkItemID,
		AttemptID:   &out.AttemptID,
	}
	return out
}

func (h *ma4Harness) addAttempt(t *testing.T, scope ma4Scope, number, epoch int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := h.pool.Exec(context.Background(), `
		INSERT INTO attempts (id, work_item_id, attempt_number, epoch_number, status)
		VALUES ($1, $2, $3, $4, 'running')`, id, scope.WorkItemID, number, epoch)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func (h *ma4Harness) bindEndpoint(t *testing.T, scope ma4Scope, rawURL string) uuid.UUID {
	t.Helper()
	assetID := uuid.New()
	key := "ep" + strings.ReplaceAll(assetID.String(), "-", "")[:12]
	ctx := context.Background()
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO assets (id, namespace, kind, external_key, name)
		VALUES ($1, 'ma4', 'http_endpoint', $2, 'fixture')`, assetID, key); err != nil {
		t.Fatal(err)
	}
	snapshot := fmt.Sprintf(`{"metadata":{"url":%q}}`, rawURL)
	if _, err := h.pool.Exec(ctx, `INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, $3::jsonb)`, scope.RunID, assetID, snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := h.pool.Exec(ctx, `INSERT INTO work_item_assets (work_item_id, asset_id) VALUES ($1, $2)`, scope.WorkItemID, assetID); err != nil {
		t.Fatal(err)
	}
	h.assetIDs = append(h.assetIDs, assetID)
	return assetID
}

func (h *ma4Harness) artifactText(t *testing.T, id *uuid.UUID) string {
	t.Helper()
	if id == nil {
		t.Fatal("artifact id is nil")
	}
	var content string
	if err := h.pool.QueryRow(context.Background(), `SELECT content FROM artifacts WHERE id=$1`, *id).Scan(&content); err != nil {
		t.Fatal(err)
	}
	return content
}

func (h *ma4Harness) ownedRunning(t *testing.T) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	api, err := newDockerAPI("/var/run/docker.sock")
	if err != nil {
		t.Fatal(err)
	}
	listed, err := api.listContainers(ctx, map[string]string{LabelOwner: OwnerValue, LabelBroker: h.brokerID})
	if err != nil {
		t.Fatal(err)
	}
	running := 0
	for _, item := range listed {
		if item.State == "running" {
			running++
		}
	}
	return running
}

func hostCmdlineHas(marker string) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		raw, err := os.ReadFile("/proc/" + entry.Name() + "/cmdline")
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), marker) {
			return true
		}
	}
	return false
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func (h *ma4Harness) waitJobContainer(t *testing.T, attemptID uuid.UUID) (uuid.UUID, error) {
	t.Helper()
	deadline := time.Now().Add(25 * time.Second)
	api, err := newDockerAPI("/var/run/docker.sock")
	if err != nil {
		return uuid.Nil, err
	}
	for time.Now().Before(deadline) {
		var id uuid.UUID
		err := h.pool.QueryRow(context.Background(), `
			SELECT id FROM execution_jobs WHERE attempt_id=$1 ORDER BY created_at DESC LIMIT 1`, attemptID).Scan(&id)
		if err == nil {
			listed, listErr := api.listContainers(context.Background(), map[string]string{
				LabelJob:    id.String(),
				LabelBroker: h.brokerID,
			})
			if listErr == nil {
				for _, item := range listed {
					if item.State == "running" {
						return id, nil
					}
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	return uuid.Nil, errors.New("execution container did not start")
}

func (h *ma4Harness) insertToolCall(t *testing.T, runID uuid.UUID, name, output string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	_, err := h.pool.Exec(context.Background(), `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status, epoch_number)
		VALUES ($1, $2, $3, $4, '{}'::jsonb, $5, 'success', 1)`,
		id, runID, "call-"+id.String()[:8], name, output)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func requireCode(t *testing.T, err error, code contract.ErrorCode) {
	t.Helper()
	var ce *contract.Error
	if !errors.As(err, &ce) || ce.Code != code {
		t.Fatalf("got %v, want %s", err, code)
	}
}
