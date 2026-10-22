package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/db"
	"agent-workbench/backend/internal/maacceptance/labapp"
	"agent-workbench/backend/internal/maacceptance/modeldouble"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

var workerAssetPattern = regexp.MustCompile(`(?:"asset_id"|\\"asset_id\\")\s*:\s*(?:"|\\")([0-9a-fA-F-]{36})`)
var assetLinePattern = regexp.MustCompile(`\[id: ([0-9a-fA-F-]{36})\] \(kind:`)

func TestMA_A09_AuthenticatedHTTPIdentityAndTLS(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A09", Owner: "agent-3", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A09_AuthenticatedHTTPIdentityAndTLS",
		Observed: map[string]any{},
	}
	app, err := labapp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	t.Setenv("LLM_API_KEY", "local-ma7-fixture")
	t.Setenv("LLM_BASE_URL", "http://127.0.0.1:1/v1")
	t.Setenv("LLM_MODEL", "local-ma7-model")
	t.Setenv("LLM_API_FORMAT", "chat_completions")

	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	httpsTask, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{
		Name:      "ma7-https-" + app.Marker(),
		Objective: "Read the authorized TLS catalog without mixing identities.",
		Scope: json.RawMessage(fmt.Sprintf(`{"mode":"discovery","seeds":[%q],"discovery_policy":{"origin":%q,"mode":"same_origin_html","allowed_methods":["GET"],"max_pages":4,"max_depth":1,"request_interval_ms":1}}`,
			app.HTTPSBase()+app.EntryPath(), app.HTTPSBase())),
		Config: json.RawMessage(`{"executor":"orchestrator"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	_, httpsRun := startOrchestratedRun(t, env, httpsTask.ID, 2)
	report.Observed["https_create"] = map[string]any{"status": httpsRun.Code, "body": httpsRun.Body.String()}
	if httpsRun.Code != 201 {
		report.fail(1, "https seed accepted", fmt.Sprintf("status %d body %s", httpsRun.Code, httpsRun.Body.String()))
	}
	tlsHits := 0
	for _, row := range app.Requests() {
		if row.TLS {
			tlsHits++
		}
	}
	if tlsHits == 0 {
		report.fail(1, "tls request recorded", "the lab recorded no TLS request; a database row is not a substitute")
	}

	env2 := setupTestEnv(t)
	t.Cleanup(env2.teardown)
	double, err := modeldouble.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = double.Close() })
	configureLocalModel(t, double, "chat_completions")
	var once sync.Mutex
	used := map[string]int{}
	double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
		text := string(body)
		switch classifyModelBody(body) {
		case "planner_initial":
			return planStep(body, inspectPlan(text, 1, app.RedirectPath()), modeldouble.KindChatTool), true
		case "worker":
			id := workerAssetPattern.FindStringSubmatch(text)
			asset := ""
			if len(id) == 2 {
				asset = id[1]
			}
			once.Lock()
			used[asset]++
			n := used[asset]
			once.Unlock()
			if n == 1 && asset != "" {
				return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":%q}`, asset)}, true
			}
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "done"}, true
		default:
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "wait"}, true
		}
	})
	task := createSeedTask(t, env2, app, []string{app.HTTPBase() + app.RedirectPath()}, 2)
	runID, resp := startOrchestratedRun(t, env2, task.ID, 1)
	if resp.Code != 201 {
		report.fail(1, "http discovery create", fmt.Sprintf("%d %s", resp.Code, resp.Body.String()))
	} else {
		deadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(deadline) && len(app.Requests()) == tlsHits {
			time.Sleep(30 * time.Millisecond)
		}
		stopRun(t, env2, runID)
	}
	var redirect, materials int
	idents := map[string]int{}
	for _, row := range app.Requests() {
		if strings.HasSuffix(row.Path, "/redirect") {
			redirect++
		}
		if strings.HasSuffix(row.Path, "/materials") {
			materials++
		}
		if row.Identity != "" {
			idents[row.Identity]++
		}
	}
	report.Observed["lab_requests"] = app.Requests()
	if redirect == 0 {
		report.fail(1, "real redirect request", "product did not send the redirect hop to the lab")
	}
	var output string
	_ = env2.pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(output, '\n'), '') FROM agent_tool_calls WHERE run_id=$1`, runID).Scan(&output)
	if redirect > 0 && (!strings.Contains(output, "302") && !strings.Contains(output, app.RedirectPath())) {
		report.fail(1, "per-hop receipt", "redirect response was not stored as its own hop")
	}
	if materials > 0 {
		report.fail(1, "redirect does not carry auth", "materials was fetched as a side effect of the redirect")
	}
	if len(idents) < 2 {
		report.fail(1, "identity isolation", fmt.Sprintf("product authenticated identities=%v; cookie and token aliases were not both exercised", idents))
	}
	if strings.Contains(output, "clerk-pass") || strings.Contains(output, "auditor-pass") || strings.Contains(output, "tok-") {
		report.fail(1, "credential alias", "tool output stored a password or raw token")
	}
	wb := acceptanceJSON(t, env2, http.MethodGet, fmt.Sprintf("/api/runs/%s/workbench", runID), nil, nil)
	if wb.Code != 200 || !strings.Contains(wb.Body.String(), `"ref:`) {
		report.fail(1, "credential ref", fmt.Sprintf("workbench %d did not expose ref: aliases", wb.Code))
	}
	report.finish(t)
}

func TestMA_A10_OriginLimitDedupAndNoNetworkInLock(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A10", Owner: "agent-3", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A10_OriginLimitDedupAndNoNetworkInLock",
		Observed: map[string]any{"contract_gap": "OriginPacing is a read model; no frozen numeric origin concurrency field"},
	}
	app, err := labapp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	app.HoldRequests()
	t.Cleanup(app.ReleaseRequests)
	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	double, err := modeldouble.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = double.Close() })
	configureLocalModel(t, double, "chat_completions")
	var once sync.Mutex
	used := map[string]int{}
	double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
		text := string(body)
		switch classifyModelBody(body) {
		case "planner_initial":
			return planStep(body, inspectPlan(text, 4, ""), modeldouble.KindChatTool), true
		case "worker":
			id := workerAssetPattern.FindStringSubmatch(text)
			asset := ""
			if len(id) == 2 {
				asset = id[1]
			}
			once.Lock()
			used[asset]++
			n := used[asset]
			once.Unlock()
			if n == 1 && asset != "" {
				return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":%q}`, asset)}, true
			}
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "done"}, true
		default:
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "wait"}, true
		}
	})
	seeds := []string{
		app.HTTPBase() + app.EntryPath(),
		app.HTTPBase() + app.RedirectPath(),
		app.HTTPBase() + "/" + app.Marker() + "/login",
		app.HTTPBase() + "/" + app.Marker() + "/catalog",
	}
	task := createSeedTask(t, env, app, seeds, 2)
	runID, resp := startOrchestratedRun(t, env, task.ID, 4)
	if resp.Code != 201 {
		report.fail(1, "create", resp.Body.String())
		report.finish(t)
		return
	}
	_ = app.WaitInFlight(4, 4*time.Second)
	peak := app.PeakInFlight()
	report.Observed["lab_peak"] = peak
	report.Observed["snapshot"] = snapshotWorkerCount(t, env, runID)
	report.Observed["diagnostics"] = acceptanceDiagnostics(t, env, runID, double)
	if peak < 4 {
		report.fail(1, "origin overlap", fmt.Sprintf("peak in-flight catalog requests=%d; completion count is not overlap", peak))
	}
	if peak > 4 {
		report.fail(1, "origin cap", fmt.Sprintf("peak %d exceeded the four authorized seeds", peak))
	}
	paths := map[string]int{}
	for _, row := range app.Requests() {
		paths[row.Path]++
		if paths[row.Path] > 1 {
			report.fail(1, "dedup", row.Path+" was in the log more than once while held")
		}
	}
	wb := acceptanceJSON(t, env, http.MethodGet, fmt.Sprintf("/api/runs/%s/workbench", runID), nil, nil)
	if wb.Code != 200 || !strings.Contains(wb.Body.String(), `"effective":4`) {
		report.fail(1, "pacing does not lower effective", fmt.Sprintf("workbench %d %s", wb.Code, wb.Body.String()))
	}
	factDone := make(chan int, 1)
	go func() {
		w := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/facts", runID), map[string]any{
			"summary": "control-plane write while origin request is held", "confidence": "observed",
		}, nil)
		factDone <- w.Code
	}()
	select {
	case code := <-factDone:
		if code >= 500 {
			report.fail(1, "control plane during fetch", fmt.Sprintf("fact status %d", code))
		}
	case <-time.After(800 * time.Millisecond):
		report.fail(1, "no network inside lock", "fact write did not return while a catalog request was held")
	}
	app.ReleaseRequests()
	stopRun(t, env, runID)
	unique := map[string]struct{}{}
	for _, row := range app.Requests() {
		unique[row.Path] = struct{}{}
	}
	if len(unique) > 2 {
		report.fail(1, "max_pages", fmt.Sprintf("fetched %d paths with max_pages=2", len(unique)))
	}
	assertNoLeaks(t, report, 1, "A10", env, runID, double)
	report.finish(t)
}

func TestMA_A11_ContainerIsolationAndTrustedCapture(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A11", Owner: "agent-4", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A11_ContainerIsolationAndTrustedCapture",
		Observed: map[string]any{},
	}
	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	double, err := modeldouble.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = double.Close() })
	configureLocalModel(t, double, "chat_completions")
	canary := filepath.Join(os.TempDir(), "ma7-a11-host-escape")
	_ = os.Remove(canary)
	double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
		switch classifyModelBody(body) {
		case "planner_initial":
			return planStep(body, namedPlan([]string{"dir-script"}), modeldouble.KindChatTool), true
		case "worker":
			return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "run_command", Arguments: fmt.Sprintf(`{"argv":["/bin/sh","-c","touch %s"]}`, canary)}, true
		default:
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "wait"}, true
		}
	})
	task := createSampleTask(t, env)
	runID, resp := startOrchestratedRun(t, env, task.ID, 1)
	if resp.Code != 201 {
		report.fail(1, "create", resp.Body.String())
		report.finish(t)
		return
	}
	time.Sleep(1200 * time.Millisecond)
	if _, err := os.Stat(canary); err == nil {
		report.fail(1, "host escape", canary+" was created on the host")
		_ = os.Remove(canary)
	}
	var jobsErr string
	var jobs int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM execution_jobs WHERE run_id=$1`, runID).Scan(&jobs); err != nil {
		jobsErr = err.Error()
		report.fail(1, "job row", jobsErr)
	} else if jobs == 0 {
		report.fail(1, "job row", "run_command produced no execution_jobs row")
	}
	label := "ma7.accept=" + runID.String()
	out, dockerErr := exec.Command("docker", "ps", "-aq", "--filter", "label="+label).CombinedOutput()
	report.Observed["docker"] = map[string]any{"label": label, "output": string(out), "error": errString(dockerErr)}
	if dockerErr != nil || strings.TrimSpace(string(out)) == "" {
		report.fail(1, "owned container", "no container with the run label was observed; cleanup will not prune")
	}
	var success int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND tool_name='run_command' AND status='success'`, runID).Scan(&success)
	if success > 0 && jobs == 0 {
		report.fail(1, "untrusted success", "run_command succeeded without a job record")
	}
	stopRun(t, env, runID)
	assertNoLeaks(t, report, 1, "A11", env, runID, double)
	report.finish(t)
}

func TestMA_A12_StopKillEpochNoLeak(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A12", Owner: "agent-1", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A12_StopKillEpochNoLeak",
		Observed: map[string]any{},
	}
	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	double, err := modeldouble.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = double.Close() })
	configureLocalModel(t, double, "chat_completions")
	double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
		text := string(body)
		switch classifyModelBody(body) {
		case "planner_initial":
			return planStep(body, namedPlan([]string{"dir-kill", "dir-keep"}), modeldouble.KindChatTool), true
		case "worker":
			if strings.Contains(text, "dir-keep") {
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "keep", Barrier: "keep"}, true
			}
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "kill", Barrier: "kill"}, true
		default:
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "wait"}, true
		}
	})
	task := createSampleTask(t, env)
	runID, resp := startOrchestratedRun(t, env, task.ID, 2)
	if resp.Code != 201 {
		report.fail(1, "create", resp.Body.String())
		report.finish(t)
		return
	}
	killArrived := double.Barrier("kill").WaitArrived(1, 3*time.Second)
	keepArrived := double.Barrier("keep").WaitArrived(1, 3*time.Second)
	report.Observed["diagnostics"] = acceptanceDiagnostics(t, env, runID, double)
	if !killArrived || !keepArrived {
		report.fail(1, "workers overlapped before kill", fmt.Sprintf("kill=%d keep=%d events=%d", double.Barrier("kill").Arrived(), double.Barrier("keep").Arrived(), len(double.Events())))
	}
	var killID uuid.UUID
	_ = env.pool.QueryRow(context.Background(), `SELECT id FROM work_items WHERE run_id=$1 AND key='dir-kill'`, runID).Scan(&killID)
	cmd := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/commands", runID), map[string]any{
		"type": "kill_work", "expected_epoch": 1, "work_item_id": killID, "payload": map[string]any{"reason": "stop this direction"},
	}, map[string]string{"Idempotency-Key": "ma7-a12-kill"})
	if cmd.Code == http.StatusNotFound {
		report.fail(1, "command route", "POST /commands returned 404")
	}
	replay := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/commands", runID), map[string]any{
		"type": "stop_run", "expected_epoch": 1,
	}, map[string]string{"Idempotency-Key": "ma7-a12-kill"})
	if cmd.Code != http.StatusNotFound && replay.Code != http.StatusConflict {
		report.fail(1, "idempotency 409", fmt.Sprintf("same key different body returned %d", replay.Code))
	}
	var callsBefore int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_calls WHERE run_id=$1`, runID).Scan(&callsBefore)
	if callsBefore == 0 {
		report.fail(1, "usage accounted while in flight", "model_calls was 0 while worker requests were held")
	}
	var stopBoundary time.Time
	if err := env.pool.QueryRow(context.Background(), `SELECT now()`).Scan(&stopBoundary); err != nil {
		t.Fatal(err)
	}
	stopRun(t, env, runID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && double.InFlight() > 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if double.InFlight() != 0 {
		report.fail(1, "cancel releases model wait", fmt.Sprintf("in_flight=%d after stop", double.InFlight()))
	}
	attempts, sessions, calls := runningChildCounts(t, env, runID)
	if attempts != 0 || sessions != 0 || calls != 0 {
		report.fail(1, "running child rows", fmt.Sprintf("attempts=%d sessions=%d model_calls=%d", attempts, sessions, calls))
	}
	var callsAfterStop int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_calls WHERE run_id=$1`, runID).Scan(&callsAfterStop)
	epoch := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/commands", runID), map[string]any{
		"type": "start_epoch", "expected_epoch": 1, "payload": map[string]any{"reason": "continue same run"},
	}, map[string]string{"Idempotency-Key": "ma7-a12-epoch"})
	if epoch.Code == http.StatusNotFound {
		legacy := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/epochs/new", runID), map[string]any{
			"reason": "continue same run", "expected_epoch": 1,
		}, nil)
		report.Observed["legacy_epoch"] = legacy.Code
		if legacy.Code != 201 && legacy.Code != 200 {
			report.fail(1, "same run epoch", fmt.Sprintf("commands %d legacy %d %s", epoch.Code, legacy.Code, legacy.Body.String()))
		}
	}
	var callsAfterEpoch int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM model_calls WHERE run_id=$1`, runID).Scan(&callsAfterEpoch)
	if callsAfterEpoch < callsAfterStop || callsAfterStop == 0 && callsBefore > 0 {
		report.fail(1, "usage not zeroed", fmt.Sprintf("before=%d after_stop=%d after_epoch=%d", callsBefore, callsAfterStop, callsAfterEpoch))
	}
	double.Barrier("kill").Release()
	double.Barrier("keep").Release()
	time.Sleep(200 * time.Millisecond)
	var lateSuccess int
	_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND epoch_number=1 AND status='success' AND finished_at > $2`, runID, stopBoundary).Scan(&lateSuccess)
	if lateSuccess > 0 {
		report.fail(1, "late epoch write", fmt.Sprintf("epoch 1 gained %d recent success tool calls", lateSuccess))
	}
	if n := descendantNames(); len(n) > 0 {
		report.fail(1, "child processes", strings.Join(n, ","))
	}
	report.finish(t)
}

func TestMA_A13_ProofRequiresTrustedTargetEvidence(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A13", Owner: "agent-3", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A13_ProofRequiresTrustedTargetEvidence",
		Observed: map[string]any{},
	}
	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	task := createSampleTask(t, env)
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var assetID uuid.UUID
	if err := env.pool.QueryRow(context.Background(), `SELECT asset_id FROM run_assets WHERE run_id=$1 LIMIT 1`, run.ID).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	goals := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/goals", run.ID), map[string]any{
		"goals": []map[string]any{{"text": "The synthetic mismatch is independently verified on the target"}},
	}, nil)
	var goalWrap struct {
		Goals []struct {
			ID uuid.UUID `json:"id"`
		} `json:"goals"`
	}
	_ = json.Unmarshal(goals.Body.Bytes(), &goalWrap)
	if goals.Code >= 400 || len(goalWrap.Goals) == 0 {
		report.fail(1, "goal", fmt.Sprintf("%d %s", goals.Code, goals.Body.String()))
		report.finish(t)
		return
	}
	fact := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/facts", run.ID), map[string]any{
		"summary": "command stdout said the counts matched", "confidence": "inferred",
	}, nil)
	var factWrap struct {
		Fact struct {
			ID uuid.UUID `json:"id"`
		} `json:"fact"`
	}
	_ = json.Unmarshal(fact.Body.Bytes(), &factWrap)
	proved := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/goals/%s/prove", run.ID, goalWrap.Goals[0].ID), map[string]any{
		"evidence_id": factWrap.Fact.ID, "reason": "reading the recorded fact is enough",
	}, nil)
	if proved.Code < 400 {
		report.fail(1, "knowledge is not empirical", proved.Body.String())
	}
	cmdID := uuid.New()
	_, insertErr := env.pool.Exec(context.Background(), `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status, epoch_number, finished_at)
		VALUES ($1, $2, 'call_stdout', 'run_command', '{"argv":["printf"]}', 'listed_count matches', 'success', 1, now())`, cmdID, run.ID)
	if insertErr != nil {
		report.fail(1, "stage stdout row", insertErr.Error())
	} else {
		finding := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings", run.ID), map[string]any{
			"asset_id": assetID, "title": "stdout proof", "category": "logic", "severity": "low",
			"description": "command stdout", "source_kind": "empirical", "source_tool_call_id": cmdID,
		}, nil)
		if finding.Code < 400 {
			report.fail(1, "stdout cannot be empirical", finding.Body.String())
		}
	}
	fakeHTTP := uuid.New()
	_, insertErr = env.pool.Exec(context.Background(), `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status, epoch_number, finished_at)
		VALUES ($1, $2, 'call_fake_http', 'get_endpoint', $3, '{"status":200,"body":"synthetic success"}', 'success', 1, now())`,
		fakeHTTP, run.ID, fmt.Sprintf(`{"asset_id":%q}`, assetID))
	if insertErr != nil {
		report.fail(1, "stage fake receipt", insertErr.Error())
	} else {
		finding := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings", run.ID), map[string]any{
			"asset_id": assetID, "title": "inserted receipt", "category": "logic", "severity": "low",
			"description": "no lab request", "source_kind": "empirical", "source_tool_call_id": fakeHTTP,
		}, nil)
		if finding.Code < 400 {
			report.fail(1, "inserted success is not a network receipt", finding.Body.String())
		}
	}
	spec := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings", run.ID), map[string]any{
		"asset_id": assetID, "title": "synthetic mismatch note", "category": "logic", "severity": "info",
		"description": "not yet verified", "source_kind": "speculative",
	}, nil)
	var finding model.Finding
	_ = json.Unmarshal(spec.Body.Bytes(), &finding)
	if spec.Code >= 400 || finding.ID == uuid.Nil {
		report.fail(1, "speculative finding", fmt.Sprintf("%d %s", spec.Code, spec.Body.String()))
		report.finish(t)
		return
	}
	retest := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings/%s/retests", run.ID, finding.ID), map[string]any{"notes": "independent check"}, nil)
	var retestWrap struct {
		Retest struct {
			ID uuid.UUID `json:"id"`
		} `json:"retest"`
	}
	_ = json.Unmarshal(retest.Body.Bytes(), &retestWrap)
	if retest.Code >= 400 || retestWrap.Retest.ID == uuid.Nil {
		report.fail(1, "retest", fmt.Sprintf("%d %s", retest.Code, retest.Body.String()))
	} else {
		finished := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings/%s/retests/%s/finish", run.ID, finding.ID, retestWrap.Retest.ID), map[string]any{
			"verdict": "fixed", "evidence": "command stdout said fixed", "summary": "prose only",
		}, nil)
		report.Observed["fixed_status"] = finished.Code
		if finished.Code < 400 {
			again := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/goals/%s/prove", run.ID, goalWrap.Goals[0].ID), map[string]any{
				"evidence_id": finding.ID, "reason": "fixed finding still proves the goal",
			}, nil)
			if again.Code < 400 {
				report.fail(1, "fixed invalidates proof", again.Body.String())
			}
		} else {
			report.fail(1, "fixed invalidation not observed", fmt.Sprintf("finish status %d body %s", finished.Code, finished.Body.String()))
		}
	}
	withdraw := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings/%s/withdraw", run.ID, finding.ID), map[string]any{"reason": "synthetic proof withdrawn"}, nil)
	report.Observed["withdraw_status"] = withdraw.Code
	if withdraw.Code == http.StatusNotFound {
		report.fail(1, "withdraw invalidates proof", "POST findings/{id}/withdraw returned 404")
	} else if withdraw.Code < 400 {
		again := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/goals/%s/prove", run.ID, goalWrap.Goals[0].ID), map[string]any{
			"evidence_id": finding.ID, "reason": "withdrawn finding still proves the goal",
		}, nil)
		if again.Code < 400 {
			report.fail(1, "withdraw invalidates proof", again.Body.String())
		}
	} else {
		report.fail(1, "withdraw invalidates proof", fmt.Sprintf("status %d body %s", withdraw.Code, withdraw.Body.String()))
	}
	report.finish(t)
}

func TestMA_A14_LabCollaborationIsMaterial(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A14", Owner: "agent-1", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A14_LabCollaborationIsMaterial",
		Observed: map[string]any{},
	}
	app, err := labapp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	app.HoldRequests()
	t.Cleanup(app.ReleaseRequests)
	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	double, err := modeldouble.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = double.Close() })
	configureLocalModel(t, double, "chat_completions")
	var once sync.Mutex
	used := map[string]int{}
	double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
		text := string(body)
		switch classifyModelBody(body) {
		case "planner_initial":
			return planStep(body, collaborationPlan(text), modeldouble.KindChatTool), true
		case "worker":
			if strings.Contains(text, "dir-script") {
				id := workerAssetPattern.FindStringSubmatch(text)
				asset := ""
				if len(id) == 2 {
					asset = id[1]
				}
				once.Lock()
				used["dir-script"]++
				n := used["dir-script"]
				once.Unlock()
				if n == 1 && asset != "" {
					return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":%q}`, asset)}, true
				}
				return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "run_command", Arguments: `{"argv":["/bin/sh","/opt/work/compare.sh"]}`}, true
			}
			if strings.Contains(text, "dir-analyze") {
				once.Lock()
				used["dir-analyze"]++
				n := used["dir-analyze"]
				once.Unlock()
				if n > 1 {
					return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "http_request", Arguments: `{"method":"POST","path":"/login","body":{"username":"dana","password":"clerk-pass"}}`}, true
				}
			}
			id := workerAssetPattern.FindStringSubmatch(text)
			asset := ""
			if len(id) == 2 {
				asset = id[1]
			}
			once.Lock()
			used[asset]++
			n := used[asset]
			once.Unlock()
			if n == 1 && asset != "" {
				return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":%q}`, asset)}, true
			}
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "done", Barrier: "workers"}, true
		default:
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "wait"}, true
		}
	})
	seeds := []string{
		app.HTTPBase() + app.EntryPath(),
		app.HTTPBase() + app.ScriptPath(),
		app.HTTPBase() + app.VerifyPath(),
	}
	task := createSeedTask(t, env, app, seeds, 4)
	runID, resp := startOrchestratedRun(t, env, task.ID, 3)
	if resp.Code != 201 {
		report.fail(1, "create", resp.Body.String())
		report.finish(t)
		return
	}
	_ = app.WaitInFlight(2, 4*time.Second)
	if app.PeakInFlight() < 2 {
		report.fail(1, "two workers overlap on the lab", fmt.Sprintf("peak=%d", app.PeakInFlight()))
	}
	app.ReleaseRequests()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(app.Requests()) < 2 {
		time.Sleep(40 * time.Millisecond)
	}
	idents := map[string]int{}
	paths := map[string]int{}
	for _, row := range app.Requests() {
		paths[row.Path]++
		if row.Identity != "" {
			idents[row.Identity]++
		}
	}
	report.Observed["paths"] = paths
	report.Observed["identities"] = idents
	if paths[app.EntryPath()] == 0 && paths["/"+app.Marker()+"/"] == 0 {
		report.fail(1, "entry discovered by request", "lab entry was not requested")
	}
	if idents["ravi"] == 0 || idents["dana"] == 0 {
		report.fail(1, "authorized analysis", fmt.Sprintf("identities=%v; workers did not keep materials separate", idents))
	}
	if paths[app.VerifyPath()] == 0 {
		report.fail(1, "independent verify request", "verify path was not requested")
	}
	scriptDeadline := time.Now().Add(4 * time.Second)
	var scriptCalls int
	for {
		_ = env.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND tool_name='run_command'`, runID).Scan(&scriptCalls)
		if scriptCalls > 0 || time.Now().After(scriptDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if scriptCalls == 0 {
		report.fail(1, "script direction", "no worker executed or attempted run_command")
	}
	var jobs int
	if err := env.pool.QueryRow(context.Background(), `SELECT count(*) FROM execution_jobs WHERE run_id=$1`, runID).Scan(&jobs); err != nil || jobs == 0 {
		report.fail(1, "script container", fmt.Sprintf("jobs=%d err=%v", jobs, err))
	}
	report.Observed["diagnostics"] = acceptanceDiagnostics(t, env, runID, double)
	double.Barrier("workers").Release()
	stopRun(t, env, runID)
	assertNoLeaks(t, report, 1, "A14", env, runID, double)
	report.finish(t)
}

func TestMA_A15_BrowserFlowNotExecutedHere(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A15", Owner: "agent-6", Rounds: 1,
		Command:  "frontend/tests/ma-acceptance/playwright.config.ts",
		Observed: map[string]any{"spec": "frontend/tests/ma-acceptance/ma-acceptance.spec.ts"},
	}
	if os.Getenv("MA_ACCEPTANCE_BROWSER_RAN") != "1" {
		report.fail(1, "browser flow", "Playwright did not run against an isolated stack; screenshots from another server are not evidence")
	}
	report.finish(t)
}

func TestMA_A16_HistoryAndReleaseGates(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A16", Owner: "agent-0", Rounds: 1,
		Command:  "go test ./test/ -count=1 -run TestMA_A16_HistoryAndReleaseGates",
		Observed: map[string]any{},
	}
	for _, n := range []int{contract.MigrationRuntimeConstraints, contract.MigrationModelDiagnostics, contract.MigrationHTTPCapture, contract.MigrationExecutionJobs, contract.MigrationMainControl} {
		matches, _ := filepath.Glob(filepath.Join(migrationsPath, fmt.Sprintf("%03d_*", n)))
		if len(matches) > 0 {
			report.fail(1, "reserved migration", strings.Join(matches, ","))
		}
	}
	env := setupTestEnv(t)
	t.Cleanup(env.teardown)
	task := createSampleTask(t, env)
	run, _, err := env.runService.Create(context.Background(), task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var assetID uuid.UUID
	if err := env.pool.QueryRow(context.Background(), `SELECT asset_id FROM run_assets WHERE run_id=$1 LIMIT 1`, run.ID).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	spec := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings", run.ID), map[string]any{
		"asset_id": assetID, "title": "historical synthetic note", "category": "logic", "severity": "info",
		"description": "must survive migration", "source_kind": "speculative",
	}, nil)
	var finding model.Finding
	_ = json.Unmarshal(spec.Body.Bytes(), &finding)
	review := acceptanceJSON(t, env, http.MethodPost, fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), map[string]any{
		"status": "confirmed", "comment": "ma7 historical review", "reviewer": "ma7-acceptance",
		"expected_version": 0, "request_id": uuid.NewString(),
	}, nil)
	if review.Code >= 400 {
		report.fail(1, "review create", fmt.Sprintf("%d %s", review.Code, review.Body.String()))
	}
	var comment, reviewer, created string
	_ = env.pool.QueryRow(context.Background(), `SELECT comment, reviewer, created_at::text FROM finding_reviews WHERE run_id=$1`, run.ID).Scan(&comment, &reviewer, &created)
	if err := db.RunMigrations(os.Getenv("TEST_DATABASE_URL"), migrationsPath); err != nil {
		report.fail(1, "incremental migration", err.Error())
	}
	var comment2, reviewer2, created2 string
	_ = env.pool.QueryRow(context.Background(), `SELECT comment, reviewer, created_at::text FROM finding_reviews WHERE run_id=$1`, run.ID).Scan(&comment2, &reviewer2, &created2)
	if comment2 != comment || reviewer2 != reviewer || created2 != created || comment != "ma7 historical review" {
		report.fail(1, "review bytes", fmt.Sprintf("before %s/%s/%s after %s/%s/%s", comment, reviewer, created, comment2, reviewer2, created2))
	}
	if err := (contract.ExecutionPorts{}).ProductionReady(); err == nil {
		report.fail(1, "production ports", "empty ports reported ready")
	}
	report.fail(1, "old database upgrade", "agent_workbench was not migrated or compared; development history was not copied or rewritten")
	report.fail(1, "release gates", "full go test -race, go vet, frontend build, Playwright, and Compose smoke were not run on an integration commit. Compose binds host ports 25432/28080/23000/29090 and was not started.")
	report.finish(t)
}

func createSeedTask(t *testing.T, env *TestEnv, app *labapp.Server, seeds []string, maxPages int) model.Task {
	t.Helper()
	rawSeeds, _ := json.Marshal(seeds)
	scope := fmt.Sprintf(`{"mode":"discovery","seeds":%s,"discovery_policy":{"origin":%q,"mode":"same_origin_html","allowed_methods":["GET"],"max_pages":%d,"max_depth":1,"request_interval_ms":1}}`,
		rawSeeds, app.HTTPBase(), maxPages)
	task, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{
		Name:      "ma7-seeds-" + app.Marker(),
		Objective: "Inspect only the authorized lab paths for this instance.",
		Scope:     json.RawMessage(scope),
		Config:    json.RawMessage(`{"executor":"orchestrator"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	return *task
}

func inspectPlan(prompt string, n int, preferPath string) string {
	return inspectPlanFrom(prompt, assetLineIDs(prompt), n, preferPath)
}

func collaborationPlan(prompt string) string {
	ids := assetLineIDs(prompt)
	if len(ids) == 0 {
		return namedPlan([]string{"dir-entry", "dir-analyze", "dir-script"})
	}
	var entry, script, verify string
	for _, id := range ids {
		window := windowAround(prompt, id)
		switch {
		case strings.Contains(window, "/script"):
			script = id
		case strings.Contains(window, "/verify"):
			verify = id
		default:
			if entry == "" {
				entry = id
			}
		}
	}
	if entry == "" {
		entry = ids[0]
	}
	if script == "" {
		script = ids[0]
	}
	if verify == "" {
		verify = ids[len(ids)-1]
	}
	analyze := script
	items := []map[string]any{
		{"key": "dir-entry", "title": "Find the lab entry", "objective": "Fetch the authorized entry once and record the response.", "work_type": "inspect_http_endpoint", "asset_ids": []string{entry}, "input_data": map[string]any{"asset_id": entry}},
		{"key": "dir-analyze", "title": "Read one identity catalog", "objective": "Authenticate as one scoped identity and read only that identity's catalog material.", "work_type": "inspect_http_endpoint", "asset_ids": []string{analyze}, "input_data": map[string]any{"asset_id": analyze}},
		{"key": "dir-script", "title": "Run the compare script", "objective": "Execute the authorized compare script and record its exit code. Verify path " + verify, "work_type": "intent", "asset_ids": []string{verify}, "input_data": map[string]any{"profile": "explore", "asset_id": verify}},
	}
	raw, _ := json.Marshal(map[string]any{"summary": "scripted collaboration plan", "items": items})
	return string(raw)
}

func acceptanceDiagnostics(t *testing.T, env *TestEnv, runID uuid.UUID, double *modeldouble.Server) map[string]any {
	t.Helper()
	statuses := map[string]int{}
	for _, ev := range double.Events() {
		statuses[fmt.Sprintf("%d", ev.Status)]++
	}
	var firstModel string
	_ = env.pool.QueryRow(context.Background(), `SELECT COALESCE(status,'') || ' ' || COALESCE(finish_reason,'') || ' ' || COALESCE(error_message,'') FROM model_calls WHERE run_id=$1 ORDER BY started_at LIMIT 1`, runID).Scan(&firstModel)
	rows, err := env.pool.Query(context.Background(), `SELECT tool_name, status, LEFT(COALESCE(error_message, output), 180) FROM agent_tool_calls WHERE run_id=$1 ORDER BY created_at`, runID)
	tools := []string{}
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var name, status, detail string
			if rows.Scan(&name, &status, &detail) == nil {
				tools = append(tools, name+":"+status+":"+detail)
			}
		}
	}
	var items string
	_ = env.pool.QueryRow(context.Background(), `SELECT COALESCE(string_agg(key || ':' || status, ','), '') FROM work_items WHERE run_id=$1`, runID).Scan(&items)
	return map[string]any{
		"model_events": len(double.Events()), "event_statuses": statuses,
		"first_model_call": firstModel, "tools": tools, "work_items": items,
	}
}

func inspectPlanFrom(prompt string, rawIDs []string, n int, preferPath string) string {
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(rawIDs))
	for _, id := range rawIDs {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	if preferPath != "" {
		ordered := make([]string, 0, len(unique))
		for _, id := range unique {
			if strings.Contains(prompt, preferPath) && strings.Contains(windowAround(prompt, id), preferPath) {
				ordered = append([]string{id}, ordered...)
				continue
			}
			ordered = append(ordered, id)
		}
		unique = ordered
	}
	if len(unique) == 0 {
		return planArguments(prompt, n, "aggregate_summary")
	}
	if n > len(unique) {
		n = len(unique)
	}
	items := make([]map[string]any, 0, n)
	for i := 0; i < n; i++ {
		items = append(items, map[string]any{
			"key": fmt.Sprintf("dir-%d", i), "title": fmt.Sprintf("Direction %d", i),
			"objective": "Fetch the authorized asset once and record the response.",
			"work_type": "inspect_http_endpoint", "asset_ids": []string{unique[i]},
			"input_data": map[string]any{"asset_id": unique[i]},
		})
	}
	raw, _ := json.Marshal(map[string]any{"summary": "scripted acceptance plan", "items": items})
	return string(raw)
}

func windowAround(text, id string) string {
	i := strings.Index(text, id)
	if i < 0 {
		return ""
	}
	start := i - 80
	if start < 0 {
		start = 0
	}
	end := i + len(id) + 80
	if end > len(text) {
		end = len(text)
	}
	return text[start:end]
}

func descendantNames() []string {
	self := os.Getpid()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return []string{"proc unreadable: " + err.Error()}
	}
	parents := map[int]int{}
	names := map[int]string{}
	for _, entry := range entries {
		pid := 0
		if _, err := fmt.Sscan(entry.Name(), &pid); err != nil || pid == self {
			continue
		}
		raw, err := os.ReadFile(filepath.Join("/proc", entry.Name(), "status"))
		if err != nil {
			continue
		}
		ppid := 0
		name := entry.Name()
		for _, line := range strings.Split(string(raw), "\n") {
			if strings.HasPrefix(line, "PPid:") {
				_, _ = fmt.Sscan(strings.TrimPrefix(line, "PPid:"), &ppid)
			}
			if strings.HasPrefix(line, "Name:") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "Name:"))
			}
		}
		parents[pid] = ppid
		names[pid] = name
	}
	var out []string
	for pid, ppid := range parents {
		for hop := 0; hop < 6 && ppid != 0; hop++ {
			if ppid == self {
				out = append(out, fmt.Sprintf("%d:%s", pid, names[pid]))
				break
			}
			ppid = parents[ppid]
		}
	}
	return out
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
