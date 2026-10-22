package test

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/maacceptance/labapp"
	"agent-workbench/backend/internal/maacceptance/modeldouble"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

// TestMA_A01_DiscoveryWorkerCapacityRequiresRealOverlap fails while discovery
// start rewrites the requested pool. It records barrier arrivals, not completions.
func TestMA_A01_DiscoveryWorkerCapacityRequiresRealOverlap(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A01", Owner: "agent-1", Rounds: acceptanceRounds(),
		Command:  "go test ./test/ -count=1 -run TestMA_A01_DiscoveryWorkerCapacityRequiresRealOverlap",
		Observed: map[string]any{},
	}
	app, err := labapp.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })

	for round := 1; round <= report.Rounds; round++ {
		requested := []int{1, 2, 4}[(round-1)%3]
		observeCapacity(t, report, round, app, true, requested)
		if requested != 4 {
			observeCapacity(t, report, round, app, true, 4)
		}
		observeCapacity(t, report, round, nil, false, 4)
	}
	report.finish(t)
}

func observeCapacity(t *testing.T, report *acceptanceReport, round int, app *labapp.Server, discovery bool, requested int) {
	t.Helper()
	label := fmt.Sprintf("round %d discovery=%t N=%d", round, discovery, requested)
	env := setupTestEnv(t)
	defer env.teardown()
	double, err := modeldouble.New()
	if err != nil {
		t.Fatal(err)
	}
	defer double.Close()
	configureLocalModel(t, double, "chat_completions")
	double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
		switch classifyModelBody(body) {
		case "worker":
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "held", Barrier: "workers"}, true
		case "planner_initial":
			kind := "aggregate_summary"
			if discovery {
				kind = "inspect_http_endpoint"
			}
			return planStep(body, planArguments(string(body), 4, kind), modeldouble.KindChatTool), true
		case "planner_followup":
			return modeldouble.Step{Kind: modeldouble.KindText, Text: "waiting"}, true
		default:
			return modeldouble.Step{}, false
		}
	})

	var task model.Task
	if discovery {
		task = createDiscoveryTask(t, env, app)
	} else {
		task = createSampleTask(t, env)
	}
	runID, resp := startOrchestratedRun(t, env, task.ID, requested)
	if resp.Code != 201 {
		report.fail(round, label+" create", fmt.Sprintf("status %d body %s", resp.Code, resp.Body.String()))
		return
	}
	snapshotN := snapshotWorkerCount(t, env, runID)
	if snapshotN != requested {
		report.fail(round, label+" snapshot", fmt.Sprintf("snapshot max_parallel_workers=%d requested=%d", snapshotN, requested))
	}
	eff := postJSON(t, env, "GET", fmt.Sprintf("/api/runs/%s/budget/effective", runID), nil, nil)
	if eff.Code != 200 || !strings.Contains(eff.Body.String(), fmt.Sprintf(`"max_parallel_workers":%d`, requested)) {
		report.fail(round, label+" effective", fmt.Sprintf("status %d body %s", eff.Code, eff.Body.String()))
	}

	window := 900 * time.Millisecond
	if requested >= 4 {
		window = 1500 * time.Millisecond
	}
	arrived := double.Barrier("workers").WaitArrived(requested, window)
	workerArrived := double.Barrier("workers").Arrived()
	peak := double.Peak()
	attempts := listAttempts(t, env, runID)
	running := 0
	for _, row := range attempts {
		if row.Status == "running" {
			running++
		}
	}
	report.Observed[label] = map[string]any{
		"snapshot": snapshotN, "worker_barrier": workerArrived, "server_peak": peak,
		"running_attempts": running, "attempt_count": len(attempts), "barrier_met": arrived,
	}
	if workerArrived != requested || running > requested {
		report.fail(round, label+" overlap", fmt.Sprintf("worker barrier=%d running_attempts=%d server_peak=%d requested=%d", workerArrived, running, peak, requested))
	}
	if requested > 1 && workerArrived < requested {
		report.fail(round, label+" not serial fallback", "completion count is not accepted; barrier did not show the requested overlap")
	}
	double.Barrier("workers").Release()
	stopRun(t, env, runID)
	assertNoLeaks(t, report, round, label, env, runID, double)
}

func assertNoLeaks(t *testing.T, report *acceptanceReport, round int, label string, env *TestEnv, runID uuid.UUID, double *modeldouble.Server) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var attempts, sessions, calls int
	for {
		attempts, sessions, calls = runningChildCounts(t, env, runID)
		if attempts == 0 && sessions == 0 && calls == 0 && double.InFlight() == 0 {
			return
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(40 * time.Millisecond)
	}
	report.fail(round, label+" leak", fmt.Sprintf("running_attempts=%d running_sessions=%d open_model_calls=%d model_inflight=%d", attempts, sessions, calls, double.InFlight()))
}

// TestMA_A02_ClaimOverlapAndDispatchDuringPlannerCall checks four distinct
// claims and whether the dispatcher can claim while a planner call is held.
func TestMA_A02_ClaimOverlapAndDispatchDuringPlannerCall(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{ID: "A02", Owner: "agent-1", Rounds: acceptanceRounds(), Command: "go test ./test/ -count=1 -run TestMA_A02_ClaimOverlapAndDispatchDuringPlannerCall"}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		double, err := modeldouble.New()
		if err != nil {
			t.Fatal(err)
		}
		configureLocalModel(t, double, "chat_completions")
		double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
			switch classifyModelBody(body) {
			case "worker":
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "held", Barrier: "workers"}, true
			case "planner_initial":
				return planStep(body, planArguments(string(body), 4, "aggregate_summary"), modeldouble.KindChatTool), true
			case "planner_followup":
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "planner held", Barrier: "planner"}, true
			default:
				return modeldouble.Step{}, false
			}
		})
		task := createSampleTask(t, env)
		runID, resp := startOrchestratedRun(t, env, task.ID, 2)
		if resp.Code != 201 {
			report.fail(round, "create", resp.Body.String())
			double.Close()
			env.teardown()
			continue
		}
		if !double.Barrier("workers").WaitArrived(2, 3*time.Second) {
			report.fail(round, "initial claim", fmt.Sprintf("workers arrived=%d", double.Barrier("workers").Arrived()))
		}
		if !double.Barrier("planner").WaitArrived(1, 3*time.Second) {
			report.fail(round, "planner follow-up", "follow-up planner call did not start while workers were held")
		}
		time.Sleep(400 * time.Millisecond)
		attempts := listAttempts(t, env, runID)
		seen := map[string]int{}
		running := 0
		for _, row := range attempts {
			seen[row.Key]++
			if row.Status == "running" {
				running++
			}
		}
		if running > 2 {
			report.fail(round, "cap", fmt.Sprintf("running attempts %d exceed N=2", running))
		}
		for key, n := range seen {
			if n != 1 {
				report.fail(round, "duplicate", fmt.Sprintf("%s has %d attempts", key, n))
			}
		}
		if len(seen) < 3 {
			report.fail(round, "dispatch during planner call", fmt.Sprintf("only %d directions claimed while planner call was in flight; pending directions were not claimed", len(seen)))
		}
		double.Barrier("planner").Release()
		double.Barrier("workers").Release()
		stopRun(t, env, runID)
		assertNoLeaks(t, report, round, "A02", env, runID, double)
		double.Close()
		env.teardown()
	}
	report.finish(t)
}

// TestMA_A03_InFlightFactCanDispatchSecondDirection requires B to start before A exits.
func TestMA_A03_InFlightFactCanDispatchSecondDirection(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{ID: "A03", Owner: "agent-1", Rounds: acceptanceRounds(), Command: "go test ./test/ -count=1 -run TestMA_A03_InFlightFactCanDispatchSecondDirection"}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		double, err := modeldouble.New()
		if err != nil {
			t.Fatal(err)
		}
		configureLocalModel(t, double, "chat_completions")
		plannerCalls := 0
		double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
			switch classifyModelBody(body) {
			case "worker":
				if strings.Contains(string(body), "dir-b") {
					return modeldouble.Step{Kind: modeldouble.KindText, Text: "b arrived", Barrier: "b"}, true
				}
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "a held", Barrier: "a"}, true
			case "planner_initial":
				return planStep(body, oneDirectionPlan("dir-a"), modeldouble.KindChatTool), true
			case "planner_followup":
				plannerCalls++
				if plannerCalls == 1 {
					return planStep(body, oneDirectionPlan("dir-b"), modeldouble.KindChatTool), true
				}
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "no further plan", Barrier: "planner-hold"}, true
			default:
				return modeldouble.Step{}, false
			}
		})
		task := createSampleTask(t, env)
		runID, resp := startOrchestratedRun(t, env, task.ID, 2)
		if resp.Code != 201 {
			report.fail(round, "create", resp.Body.String())
			double.Close()
			env.teardown()
			continue
		}
		if !double.Barrier("a").WaitArrived(1, 3*time.Second) {
			report.fail(round, "worker A", "A did not enter the barrier")
		}
		fact := postJSON(t, env, "POST", fmt.Sprintf("/api/runs/%s/facts", runID), map[string]any{
			"summary":    "A local catalog link was observed while A was still running.",
			"detail":     "synthetic fact for wake-up ordering",
			"confidence": "observed",
		}, nil)
		if fact.Code >= 400 {
			report.fail(round, "record fact", fmt.Sprintf("%d %s", fact.Code, fact.Body.String()))
		}
		if !double.Barrier("b").WaitArrived(1, 4*time.Second) {
			report.fail(round, "dispatch B while A in flight", fmt.Sprintf("B arrived=%d A still held=%d planner_calls=%d", double.Barrier("b").Arrived(), double.Barrier("a").Arrived(), plannerCalls))
		}
		followUps := 0
		for _, ev := range double.Events() {
			if ev.Rule == "dynamic" {
				followUps++
			}
		}
		if followUps > 12 {
			report.fail(round, "model storm", fmt.Sprintf("model events=%d", followUps))
		}
		double.Barrier("a").Release()
		double.Barrier("b").Release()
		double.Barrier("planner-hold").Release()
		stopRun(t, env, runID)
		assertNoLeaks(t, report, round, "A03", env, runID, double)
		double.Close()
		env.teardown()
	}
	report.finish(t)
}

func oneDirectionPlan(key string) string {
	return fmt.Sprintf(`{"summary":"one","items":[{"key":"%s","title":"%s","objective":"Hold this direction","work_type":"aggregate_summary","input_data":{}}]}`, key, key)
}

// TestMA_A04_LocalProtocolAndExhaustionDoNotStopPeers keeps one bad response
// and one exhausted worker from ending the other directions.
// Peer progress is a dir-ok or dir-slow worker model call, or the slow barrier.
// A planner HTTP 200 that overlaps another call is not that proof.
func TestMA_A04_LocalProtocolAndExhaustionDoNotStopPeers(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{
		ID: "A04", Owner: "agent-1", Rounds: acceptanceRounds(),
		Command:  "go test ./test/ -count=1 -run TestMA_A04_LocalProtocolAndExhaustionDoNotStopPeers",
		Observed: map[string]any{},
	}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		double, err := modeldouble.New()
		if err != nil {
			t.Fatal(err)
		}
		configureLocalModel(t, double, "responses")
		var badCalls, exhaustCalls, peerOKCalls, peerSlowCalls atomic.Int32
		double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
			text := string(body)
			switch classifyModelBody(body) {
			case "planner_initial":
				return planStep(body, namedPlan([]string{"dir-bad", "dir-exhaust", "dir-ok", "dir-slow"}), modeldouble.KindResponsesFunction), true
			case "worker":
				switch {
				case strings.Contains(text, "dir-bad"):
					badCalls.Add(1)
					return modeldouble.Step{Kind: modeldouble.KindIncompleteArgs, Name: "write_artifact", Arguments: `{"title":`, CallID: "call_bad"}, true
				case strings.Contains(text, "dir-exhaust"):
					n := exhaustCalls.Add(1)
					return modeldouble.Step{
						Kind: modeldouble.KindResponsesFunction, Name: "write_artifact",
						Arguments: `{"name":"loop.txt","content":"x"}`,
						CallID:    fmt.Sprintf("call_loop_%d", n),
					}, true
				case strings.Contains(text, "dir-slow"):
					peerSlowCalls.Add(1)
					return modeldouble.Step{Kind: modeldouble.KindText, Text: "slow done", Barrier: "slow"}, true
				case strings.Contains(text, "dir-ok"):
					peerOKCalls.Add(1)
					return modeldouble.Step{Kind: modeldouble.KindText, Text: "peer done"}, true
				default:
					return modeldouble.Step{Kind: modeldouble.KindText, Text: "unclassified worker"}, true
				}
			case "planner_followup":
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "reviewing", Barrier: "review"}, true
			default:
				return modeldouble.Step{}, false
			}
		})
		task := createSampleTask(t, env)
		runID, resp := startOrchestratedRun(t, env, task.ID, 4)
		if resp.Code != 201 {
			report.fail(round, "create", resp.Body.String())
			double.Close()
			env.teardown()
			continue
		}
		peerDeadline := time.Now().Add(4 * time.Second)
		for time.Now().Before(peerDeadline) {
			if (peerOKCalls.Load() > 0 || peerSlowCalls.Load() > 0 || double.Barrier("slow").Arrived() > 0) && badCalls.Load() > 0 && exhaustCalls.Load() > 0 {
				break
			}
			time.Sleep(30 * time.Millisecond)
		}
		exhaustDeadline := time.Now().Add(6 * time.Second)
		var exhaustStatus, exhaustReason string
		for time.Now().Before(exhaustDeadline) {
			exhaustStatus, exhaustReason, _ = attemptOutcome(t, env, runID, "dir-exhaust")
			if localExhaustion(exhaustStatus, exhaustReason) {
				break
			}
			if exhaustCalls.Load() == 0 && peerOKCalls.Load() == 0 && peerSlowCalls.Load() == 0 && double.Barrier("slow").Arrived() == 0 && time.Until(exhaustDeadline) < 4*time.Second {
				break
			}
			time.Sleep(40 * time.Millisecond)
		}
		badStatus, badReason, _ := attemptOutcome(t, env, runID, "dir-bad")
		var badTools int
		_ = env.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND call_id='call_bad' AND status='success'`, runID).Scan(&badTools)
		if badTools != 0 {
			report.fail(round, "bad tool executed", fmt.Sprintf("success tool calls for incomplete call_id=%d", badTools))
		}
		run, _ := env.runService.Get(t.Context(), runID)
		if run != nil && run.FinishReason != nil && *run.FinishReason == "protocol_error" {
			report.fail(round, "run-wide protocol_error", "one incomplete worker response failed the run")
		}
		if run != nil && (run.Status == model.RunStatusFailed || run.Status == model.RunStatusStopped || run.Status == model.RunStatusCompleted) {
			finish := ""
			if run.FinishReason != nil {
				finish = *run.FinishReason
			}
			report.fail(round, "run still active", fmt.Sprintf("status=%s finish_reason=%s", run.Status, finish))
		}
		if badCalls.Load() == 0 {
			report.fail(round, "local protocol cause", "dir-bad worker model call was not observed")
		} else if badReason != "protocol_error" {
			report.fail(round, "local protocol cause", fmt.Sprintf("dir-bad status=%s finish_reason=%q model_calls=%d", badStatus, badReason, badCalls.Load()))
		}
		if badCalls.Load() > 4 {
			report.fail(round, "bounded recovery", fmt.Sprintf("dir-bad replayed %d model calls", badCalls.Load()))
		}
		if exhaustCalls.Load() == 0 {
			report.fail(round, "local exhaustion", "dir-exhaust worker model call was not observed")
		} else if !localExhaustion(exhaustStatus, exhaustReason) {
			report.fail(round, "local exhaustion", fmt.Sprintf("dir-exhaust status=%s finish_reason=%q model_calls=%d", exhaustStatus, exhaustReason, exhaustCalls.Load()))
		}
		grantedWorkerModels := snapshotWorkerModelCalls(t, env, runID)
		// Create asks for 3. One settlement call may follow the granted exploration calls.
		if grantedWorkerModels != 3 || exhaustCalls.Load() > int32(grantedWorkerModels+1) {
			report.fail(round, "bounded recovery", fmt.Sprintf("dir-exhaust model calls=%d snapshot worker_max_model_calls=%d; requested grant is 3 plus at most one settlement call", exhaustCalls.Load(), grantedWorkerModels))
		}
		if peerOKCalls.Load() == 0 || (peerSlowCalls.Load() == 0 && double.Barrier("slow").Arrived() == 0) {
			report.fail(round, "peer progress", fmt.Sprintf("dir-ok model calls=%d dir-slow model calls=%d slow_barrier=%d; a planner HTTP 200 is not peer progress", peerOKCalls.Load(), peerSlowCalls.Load(), double.Barrier("slow").Arrived()))
		}
		report.Observed[fmt.Sprintf("round-%d", round)] = map[string]any{
			"bad_model_calls":            badCalls.Load(),
			"bad_status":                 badStatus,
			"bad_finish_reason":          badReason,
			"exhaust_model_calls":        exhaustCalls.Load(),
			"exhaust_status":             exhaustStatus,
			"exhaust_finish_reason":      exhaustReason,
			"granted_worker_model_calls": grantedWorkerModels,
			"dir_ok_model_calls":         peerOKCalls.Load(),
			"dir_slow_model_calls":       peerSlowCalls.Load(),
			"slow_barrier":               double.Barrier("slow").Arrived(),
		}
		double.Barrier("slow").Release()
		double.Barrier("review").Release()
		stopRun(t, env, runID)
		assertNoLeaks(t, report, round, "A04", env, runID, double)
		double.Close()
		env.teardown()
	}
	report.finish(t)
}

func attemptOutcome(t *testing.T, env *TestEnv, runID uuid.UUID, key string) (status, reason string, ok bool) {
	t.Helper()
	err := env.pool.QueryRow(t.Context(), `
		SELECT a.status, COALESCE(a.finish_reason, '')
		FROM attempts a JOIN work_items w ON w.id = a.work_item_id
		WHERE w.run_id = $1 AND w.key = $2
		ORDER BY a.started_at DESC LIMIT 1`, runID, key).Scan(&status, &reason)
	return status, reason, err == nil
}

func localExhaustion(status, reason string) bool {
	switch reason {
	case "worker_tool_budget_exhausted", "worker_model_budget_exhausted", "tool_calls_exhausted", "model_calls_exhausted", "worker_exhausted", "worker_settlement_complete", "max_tool_calls_exceeded", "max_model_calls_exceeded":
		return status == "exhausted" || status == "failed"
	default:
		return false
	}
}

func snapshotWorkerModelCalls(t *testing.T, env *TestEnv, runID uuid.UUID) int {
	t.Helper()
	var raw []byte
	if err := env.pool.QueryRow(t.Context(), `SELECT execution_input FROM runs WHERE id=$1`, runID).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Budget struct {
			WorkerMaxModelCalls int `json:"worker_max_model_calls"`
		} `json:"budget"`
	}
	_ = json.Unmarshal(raw, &parsed)
	return parsed.Budget.WorkerMaxModelCalls
}

func namedPlan(keys []string) string {
	items := make([]string, 0, len(keys))
	for _, key := range keys {
		items = append(items, fmt.Sprintf(`{"key":"%s","title":"%s","objective":"Local %s","work_type":"aggregate_summary","input_data":{}}`, key, key, key))
	}
	return `{"summary":"mixed","items":[` + strings.Join(items, ",") + `]}`
}

// TestMA_A05_BadResponsesDoNotExecuteOrReplayForever drives incomplete, truncated, 429, and 5xx output.
func TestMA_A05_BadResponsesDoNotExecuteOrReplayForever(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{ID: "A05", Owner: "agent-2", Rounds: acceptanceRounds(), Command: "go test ./test/ -count=1 -run TestMA_A05_BadResponsesDoNotExecuteOrReplayForever", Observed: map[string]any{}}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		double, err := modeldouble.New()
		if err != nil {
			t.Fatal(err)
		}
		configureLocalModel(t, double, "responses")
		var plannerCalls, workerCalls, followCalls int
		double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
			switch classifyModelBody(body) {
			case "planner_initial":
				if strings.Contains(string(body), "Plan submitted successfully") || strings.Contains(string(body), `"created_count"`) {
					return modeldouble.Step{Kind: modeldouble.KindText, Text: "Plan submitted."}, true
				}
				plannerCalls++
				switch plannerCalls {
				case 1:
					return modeldouble.Step{Kind: modeldouble.KindStatus, StatusCode: 429}, true
				case 2:
					return modeldouble.Step{Kind: modeldouble.KindStatus, StatusCode: 500}, true
				case 3:
					return planStep(body, namedPlan([]string{"dir-a"}), modeldouble.KindResponsesFunction), true
				default:
					return modeldouble.Step{Kind: modeldouble.KindStatus, StatusCode: 429}, true
				}
			case "worker":
				workerCalls++
				return modeldouble.Step{Kind: modeldouble.KindIncompleteArgs, Name: "write_artifact", Arguments: `{"title":`, CallID: "call_same"}, true
			case "planner_followup":
				followCalls++
				return modeldouble.Step{Kind: modeldouble.KindTruncated, Name: "suggest_completion", Arguments: `{"final_summary":`}, true
			default:
				return modeldouble.Step{}, false
			}
		})
		task := createSampleTask(t, env)
		runID, resp := startOrchestratedRun(t, env, task.ID, 1)
		if resp.Code != 201 {
			report.fail(round, "create", resp.Body.String())
			double.Close()
			env.teardown()
			continue
		}
		time.Sleep(1500 * time.Millisecond)
		var badSuccess int
		_ = env.pool.QueryRow(t.Context(), `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND status='success' AND call_id='call_same'`, runID).Scan(&badSuccess)
		if badSuccess != 0 {
			report.fail(round, "executed incomplete call", fmt.Sprintf("success=%d", badSuccess))
		}
		events := double.Events()
		retries, serverErrors := 0, 0
		for _, ev := range events {
			if ev.Status == 429 {
				retries++
			}
			if ev.Status >= 500 {
				serverErrors++
			}
		}
		var modelCallCount int
		_ = env.pool.QueryRow(t.Context(), `SELECT count(*) FROM model_calls WHERE run_id=$1`, runID).Scan(&modelCallCount)
		report.Observed[fmt.Sprintf("round-%d", round)] = map[string]any{
			"events": len(events), "status_429": retries, "status_5xx": serverErrors,
			"planner_calls": plannerCalls, "worker_calls": workerCalls, "follow_calls": followCalls,
			"model_calls": modelCallCount,
		}
		if retries < 1 || retries > 3 || plannerCalls < 2 || serverErrors < 1 {
			report.fail(round, "bounded 429 and 5xx recovery", fmt.Sprintf("429=%d 5xx=%d planner_calls=%d model_calls=%d events=%d", retries, serverErrors, plannerCalls, modelCallCount, len(events)))
		}
		if workerCalls < 1 {
			report.fail(round, "incomplete arguments case reached", fmt.Sprintf("worker_calls=%d", workerCalls))
		}
		if followCalls < 1 {
			report.fail(round, "truncated follow-up reached", fmt.Sprintf("follow_calls=%d", followCalls))
		}
		if len(events) > 8 {
			report.fail(round, "infinite replay", fmt.Sprintf("events=%d", len(events)))
		}
		if modelCallCount < len(events) {
			report.fail(round, "real accounting", fmt.Sprintf("model_calls=%d double_events=%d", modelCallCount, len(events)))
		}
		var shared int
		shareErr := env.pool.QueryRow(t.Context(), `
			SELECT count(*) FROM agent_messages m
			JOIN agent_sessions s ON s.id = m.session_id
			WHERE m.run_id=$1 AND m.provider_payload IS NOT NULL
			  AND EXISTS (
			    SELECT 1 FROM agent_messages other
			    JOIN agent_sessions os ON os.id = other.session_id
			    WHERE other.run_id=m.run_id AND os.id <> s.id AND other.provider_payload = m.provider_payload
			  )`, runID).Scan(&shared)
		if shareErr != nil {
			report.fail(round, "shared native history", shareErr.Error())
		} else if shared > 0 {
			report.fail(round, "shared native history", fmt.Sprintf("copied provider payloads=%d", shared))
		}
		stopRun(t, env, runID)
		assertNoLeaks(t, report, round, "A05", env, runID, double)
		double.Close()
		env.teardown()
	}
	report.finish(t)
}

// TestMA_A06_SharedModelLimitQueuesAndCancel expects two in-flight model
// requests while four workers are active. The contract has no create-time field
// for that limit; the required behavior is still the acceptance scenario.
func TestMA_A06_SharedModelLimitQueuesAndCancel(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{ID: "A06", Owner: "agent-2", Rounds: acceptanceRounds(), Command: "go test ./test/ -count=1 -run TestMA_A06_SharedModelLimitQueuesAndCancel", Observed: map[string]any{"contract_gap": "ModelCapacity is a read model; no frozen create field names the shared limit"}}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		double, err := modeldouble.New()
		if err != nil {
			t.Fatal(err)
		}
		configureLocalModel(t, double, "chat_completions")
		double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
			switch classifyModelBody(body) {
			case "worker":
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "queued", Barrier: "workers"}, true
			case "planner_initial":
				return planStep(body, planArguments(string(body), 4, "aggregate_summary"), modeldouble.KindChatTool), true
			default:
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "wait", Barrier: "planner"}, true
			}
		})
		task := createSampleTask(t, env)
		runID, resp := startOrchestratedRun(t, env, task.ID, 4)
		if resp.Code != 201 {
			report.fail(round, "create", resp.Body.String())
			double.Close()
			env.teardown()
			continue
		}
		_ = double.Barrier("workers").WaitArrived(4, 3*time.Second)
		if double.Peak() > 2 {
			report.fail(round, "shared limit 2", fmt.Sprintf("peak in-flight model requests=%d while N=4", double.Peak()))
		}
		if double.Barrier("workers").Arrived() < 4 {
			report.fail(round, "workers remained active", fmt.Sprintf("arrived=%d", double.Barrier("workers").Arrived()))
		}
		stopRun(t, env, runID)
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) && double.InFlight() > 0 {
			time.Sleep(20 * time.Millisecond)
		}
		if double.InFlight() != 0 {
			report.fail(round, "cancel queued model calls", fmt.Sprintf("in_flight=%d after stop", double.InFlight()))
		}
		assertNoLeaks(t, report, round, "A06", env, runID, double)
		double.Close()
		env.teardown()
	}
	report.finish(t)
}

// TestMA_A07_PlannerSteerAndKillAreRealCommands requires the planner tool list
// to include steer_work and kill_work and to affect only the named direction.
func TestMA_A07_PlannerSteerAndKillAreRealCommands(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{ID: "A07", Owner: "agent-5", Rounds: acceptanceRounds(), Command: "go test ./test/ -count=1 -run TestMA_A07_PlannerSteerAndKillAreRealCommands"}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		double, err := modeldouble.New()
		if err != nil {
			t.Fatal(err)
		}
		configureLocalModel(t, double, "chat_completions")
		var sawSteer, sawKill bool
		double.SetDynamic(func(body []byte) (modeldouble.Step, bool) {
			text := string(body)
			if strings.Contains(text, `"name":"steer_work"`) || strings.Contains(text, `"name": "steer_work"`) {
				sawSteer = true
			}
			if strings.Contains(text, `"name":"kill_work"`) || strings.Contains(text, `"name": "kill_work"`) {
				sawKill = true
			}
			switch classifyModelBody(body) {
			case "worker":
				return modeldouble.Step{Kind: modeldouble.KindText, Text: "working", Barrier: "workers"}, true
			case "planner_initial":
				return planStep(body, namedPlan([]string{"dir-steer", "dir-kill", "dir-keep"}), modeldouble.KindChatTool), true
			case "planner_followup":
				return modeldouble.Step{Kind: modeldouble.KindChatTool, Name: "kill_work", Arguments: `{"work_item_key":"dir-kill","reason":"stop this direction"}`}, true
			default:
				return modeldouble.Step{}, false
			}
		})
		task := createSampleTask(t, env)
		runID, resp := startOrchestratedRun(t, env, task.ID, 3)
		if resp.Code != 201 {
			report.fail(round, "create", resp.Body.String())
			double.Close()
			env.teardown()
			continue
		}
		_ = double.Barrier("workers").WaitArrived(1, 3*time.Second)
		time.Sleep(800 * time.Millisecond)
		if !sawSteer || !sawKill {
			report.fail(round, "planner tools", fmt.Sprintf("steer_work present=%t kill_work present=%t", sawSteer, sawKill))
		}
		var killed, kept string
		_ = env.pool.QueryRow(t.Context(), `SELECT status FROM work_items WHERE run_id=$1 AND key='dir-kill'`, runID).Scan(&killed)
		_ = env.pool.QueryRow(t.Context(), `SELECT status FROM work_items WHERE run_id=$1 AND key='dir-keep'`, runID).Scan(&kept)
		if killed != "cancelled" && killed != "stopped" && killed != "failed" {
			report.fail(round, "kill target", fmt.Sprintf("dir-kill status=%q dir-keep status=%q", killed, kept))
		}
		if kept == "cancelled" {
			report.fail(round, "kill isolation", "dir-keep was cancelled with dir-kill")
		}
		double.Barrier("workers").Release()
		stopRun(t, env, runID)
		assertNoLeaks(t, report, round, "A07", env, runID, double)
		double.Close()
		env.teardown()
	}
	report.finish(t)
}

// TestMA_A08_MainAgentIdempotentBoundary calls the frozen main-message route.
func TestMA_A08_MainAgentIdempotentBoundary(t *testing.T) {
	requireAcceptanceDB(t)
	report := &acceptanceReport{ID: "A08", Owner: "agent-5", Rounds: acceptanceRounds(), Command: "go test ./test/ -count=1 -run TestMA_A08_MainAgentIdempotentBoundary"}
	for round := 1; round <= report.Rounds; round++ {
		env := setupTestEnv(t)
		task := createSampleTask(t, env)
		run, _, err := env.runService.Create(t.Context(), task.ID, model.CreateRunRequest{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		path := fmt.Sprintf("/api/runs/%s/main/messages", run.ID)
		body := map[string]any{"message": "pause the run", "expected_epoch": run.CurrentEpoch}
		headers := map[string]string{"Idempotency-Key": fmt.Sprintf("ma7-a08-%d", round)}
		first := postJSON(t, env, "POST", path, body, headers)
		if first.Code != 202 {
			report.fail(round, "submit", fmt.Sprintf("status %d body %s", first.Code, first.Body.String()))
		} else if !strings.Contains(first.Body.String(), "message_id") || !strings.Contains(first.Body.String(), "turn_id") {
			report.fail(round, "refs", first.Body.String())
		}
		second := postJSON(t, env, "POST", path, body, headers)
		if second.Code != first.Code || second.Body.String() != first.Body.String() {
			report.fail(round, "idempotent replay", fmt.Sprintf("first %d second %d", first.Code, second.Code))
		}
		var grants int
		_ = env.pool.QueryRow(t.Context(), `SELECT count(*) FROM budget_grants WHERE run_id=$1`, run.ID).Scan(&grants)
		if grants != 0 {
			report.fail(round, "no implicit grant", fmt.Sprintf("grants=%d", grants))
		}
		grant := postJSON(t, env, "POST", fmt.Sprintf("/api/runs/%s/commands", run.ID), map[string]any{
			"type": "grant_budget", "expected_epoch": run.CurrentEpoch,
			"payload": map[string]any{"additional_model_calls": 10},
		}, map[string]string{"Idempotency-Key": "ma7-grant"})
		if grant.Code == 200 || grant.Code == 202 {
			report.fail(round, "main cannot grant", fmt.Sprintf("grant returned %d", grant.Code))
		}
		env.teardown()
	}
	report.finish(t)
}
