package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/maacceptance/labapp"
	"agent-workbench/backend/internal/maacceptance/modeldouble"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

const acceptanceDBName = "agent_workbench_ma_integration"

func acceptanceRounds() int {
	if raw := os.Getenv("MA_ACCEPTANCE_ROUNDS"); raw != "" {
		var n int
		_, _ = fmt.Sscan(raw, &n)
		if n > 0 {
			return n
		}
	}
	return 20
}

func requireAcceptanceDB(t *testing.T) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	if raw == "" {
		t.Fatal("TEST_DATABASE_URL is required and must name agent_workbench_ma_integration")
	}
	denied := []string{
		"/agent_workbench?",
		"/agent_workbench_test",
		"oma_artex_test",
		"oma_phase2_review_4484633",
		"agent_workbench_ma1",
		"agent_workbench_ma2",
		"agent_workbench_ma3",
		"agent_workbench_ma4",
		"agent_workbench_ma5",
		"agent_workbench_ma6",
		"agent_workbench_ma7",
	}
	for _, name := range denied {
		if strings.Contains(raw, name) || strings.HasSuffix(raw, "/agent_workbench") {
			t.Fatalf("refusing to run acceptance tests against %s", name)
		}
	}
	if !strings.Contains(raw, "/"+acceptanceDBName) {
		t.Fatalf("acceptance tests must use %s, got a different database", acceptanceDBName)
	}
}

type roundFailure struct {
	Round  int    `json:"round"`
	Check  string `json:"check"`
	Detail string `json:"detail"`
}

type acceptanceReport struct {
	ID         string         `json:"id"`
	Status     string         `json:"status"`
	Owner      string         `json:"owner"`
	Rounds     int            `json:"rounds"`
	Failures   []roundFailure `json:"failures"`
	Observed   map[string]any `json:"observed,omitempty"`
	Command    string         `json:"command"`
	FinishedAt string         `json:"finished_at"`
}

func (r *acceptanceReport) fail(round int, check, detail string) {
	r.Failures = append(r.Failures, roundFailure{Round: round, Check: check, Detail: detail})
}

func (r *acceptanceReport) finish(t *testing.T) {
	t.Helper()
	if len(r.Failures) == 0 {
		r.Status = "pass"
	} else {
		r.Status = "fail"
	}
	r.FinishedAt = time.Now().UTC().Format(time.RFC3339)
	path := writeAcceptanceEvidence(t, r.ID, r)
	if r.Status == "fail" {
		t.Errorf("%s failed %d assertion(s); evidence %s; first: %+v", r.ID, len(r.Failures), path, r.Failures[0])
	} else {
		t.Logf("%s passed; evidence %s", r.ID, path)
	}
}

func writeAcceptanceEvidence(t *testing.T, id string, payload any) string {
	t.Helper()
	dir := os.Getenv("MA_ACCEPTANCE_EVIDENCE")
	if dir == "" {
		// Integration acceptance must not overwrite the pre-merge ma7 baseline.
		dir = filepath.Join("..", "..", "docs", "多Agent核心与工作台-实施计划", "证据", "integration-20round")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Logf("evidence directory: %v", err)
		return ""
	}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Logf("evidence marshal: %v", err)
		return ""
	}
	path := filepath.Join(dir, id+".json")
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		t.Logf("evidence write: %v", err)
		return ""
	}
	return path
}

func acceptanceJSON(t *testing.T, env *TestEnv, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	return w
}

func configureLocalModel(t *testing.T, double *modeldouble.Server, format string) {
	t.Helper()
	t.Setenv("LLM_API_KEY", "local-ma7-fixture")
	t.Setenv("LLM_BASE_URL", double.BaseURL())
	t.Setenv("LLM_MODEL", "local-ma7-model")
	if format == "" {
		format = "chat_completions"
	}
	t.Setenv("LLM_API_FORMAT", format)
}

func createDiscoveryTask(t *testing.T, env *TestEnv, app *labapp.Server) model.Task {
	t.Helper()
	scope := map[string]any{
		"mode":  "discovery",
		"seeds": []string{app.HTTPBase() + app.EntryPath()},
		"discovery_policy": map[string]any{
			"origin":              app.HTTPBase(),
			"mode":                "same_origin_html",
			"allowed_methods":     []string{"GET"},
			"max_pages":           8,
			"max_depth":           2,
			"request_interval_ms": 50,
		},
	}
	raw, _ := json.Marshal(scope)
	task, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{
		Name:      "ma7-discovery-" + app.Marker(),
		Objective: "Read the authorized lab catalog and keep each direction separate.",
		Scope:     raw,
		Config:    json.RawMessage(`{"executor":"orchestrator"}`),
	})
	if err != nil {
		t.Fatalf("create discovery task: %v", err)
	}
	return *task
}

func createSampleTask(t *testing.T, env *TestEnv) model.Task {
	t.Helper()
	task, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{
		Name:      "ma7-samples",
		Objective: "Analyze the authorized sample directions without leaving the scope.",
		Scope:     json.RawMessage(`{"endpoints":["sample_home","sample_login","sample_health"]}`),
		Config:    json.RawMessage(`{"executor":"orchestrator"}`),
	})
	if err != nil {
		t.Fatalf("create sample task: %v", err)
	}
	return *task
}

func startOrchestratedRun(t *testing.T, env *TestEnv, taskID uuid.UUID, workers int) (uuid.UUID, *httptest.ResponseRecorder) {
	t.Helper()
	body := map[string]any{
		"executor": "orchestrator",
		"provider": "openai",
		"budget": map[string]any{
			"max_parallel_workers":   workers,
			"max_model_calls":        40,
			"max_tool_calls":         40,
			"run_timeout_seconds":    25,
			"model_timeout_seconds":  15,
			"tool_timeout_seconds":   8,
			"max_planner_rounds":     6,
			"max_work_items":         8,
			"worker_max_model_calls": 3,
			"worker_max_tool_calls":  4,
			"worker_timeout_seconds": 20,
		},
	}
	w := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/tasks/%s/runs", taskID), body, nil)
	var run model.Run
	_ = json.Unmarshal(w.Body.Bytes(), &run)
	return run.ID, w
}

var assetIDPattern = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func planArguments(prompt string, n int, workType string) string {
	ids := assetLineIDs(prompt)
	items := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		item := map[string]any{
			"key":        fmt.Sprintf("dir-%d", i),
			"title":      fmt.Sprintf("Direction %d", i),
			"objective":  fmt.Sprintf("Advance authorized direction %d and stop after one observation.", i),
			"work_type":  "aggregate_summary",
			"input_data": map[string]any{},
		}
		if workType == "inspect_http_endpoint" && len(ids) > 0 {
			id := ids[i%len(ids)]
			item["work_type"] = "inspect_http_endpoint"
			item["asset_ids"] = []string{id}
			item["input_data"] = map[string]any{"asset_id": id}
		}
		items = append(items, item)
	}
	raw, _ := json.Marshal(map[string]any{"summary": "scripted acceptance plan", "items": items})
	return string(raw)
}

func assetLineIDs(prompt string) []string {
	matches := assetLinePattern.FindAllStringSubmatch(prompt, -1)
	seen := map[string]struct{}{}
	out := make([]string, 0, len(matches))
	for _, match := range matches {
		if len(match) != 2 {
			continue
		}
		if _, ok := seen[match[1]]; ok {
			continue
		}
		seen[match[1]] = struct{}{}
		out = append(out, match[1])
	}
	return out
}

func planStep(body []byte, arguments, kind string) modeldouble.Step {
	if strings.Contains(string(body), "Plan submitted successfully") || strings.Contains(string(body), `"created_count"`) {
		return modeldouble.Step{Kind: modeldouble.KindText, Text: "Plan submitted."}
	}
	if kind == "" {
		kind = modeldouble.KindChatTool
	}
	return modeldouble.Step{Kind: kind, Name: "submit_plan", Arguments: arguments}
}

func classifyModelBody(body []byte) string {
	text := string(body)
	switch {
	case strings.Contains(text, "Work Item:"):
		return "worker"
	case strings.Contains(text, "Review current execution progress"):
		return "planner_followup"
	case strings.Contains(text, "You are the Lead Security Planner"):
		return "planner_initial"
	default:
		return "other"
	}
}

type attemptRow struct {
	Key     string
	ID      uuid.UUID
	Status  string
	Started time.Time
	Ended   *time.Time
}

func listAttempts(t *testing.T, env *TestEnv, runID uuid.UUID) []attemptRow {
	t.Helper()
	rows, err := env.pool.Query(context.Background(), `
		SELECT w.key, a.id, a.status, a.started_at, a.finished_at
		FROM attempts a JOIN work_items w ON w.id = a.work_item_id
		WHERE w.run_id = $1
		ORDER BY a.started_at`, runID)
	if err != nil {
		t.Fatalf("list attempts: %v", err)
	}
	defer rows.Close()
	var out []attemptRow
	for rows.Next() {
		var row attemptRow
		if err := rows.Scan(&row.Key, &row.ID, &row.Status, &row.Started, &row.Ended); err != nil {
			t.Fatal(err)
		}
		out = append(out, row)
	}
	return out
}

func runningChildCounts(t *testing.T, env *TestEnv, runID uuid.UUID) (attempts, sessions, modelCalls int) {
	t.Helper()
	ctx := context.Background()
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM attempts a JOIN work_items w ON w.id=a.work_item_id WHERE w.run_id=$1 AND a.status='running'`, runID).Scan(&attempts)
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM agent_sessions WHERE run_id=$1 AND status='running'`, runID).Scan(&sessions)
	_ = env.pool.QueryRow(ctx, `SELECT count(*) FROM model_calls WHERE run_id=$1 AND finished_at IS NULL`, runID).Scan(&modelCalls)
	return attempts, sessions, modelCalls
}

func snapshotWorkerCount(t *testing.T, env *TestEnv, runID uuid.UUID) int {
	t.Helper()
	var raw []byte
	if err := env.pool.QueryRow(context.Background(), `SELECT execution_input FROM runs WHERE id=$1`, runID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Budget struct {
			MaxParallelWorkers int `json:"max_parallel_workers"`
		} `json:"budget"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return parsed.Budget.MaxParallelWorkers
}

func stopRun(t *testing.T, env *TestEnv, runID uuid.UUID) {
	t.Helper()
	_ = acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/stop", runID), map[string]any{}, nil)
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		run, err := env.runService.Get(context.Background(), runID)
		if err == nil && run.Status.IsTerminal() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}
