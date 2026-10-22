package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestReview3_EmpiricalEvidenceRequiresTargetResponse(t *testing.T) {
	e, runID, assets, sourceID := review2ProbeSetup(t)
	for _, tc := range []struct{ name, tool, args, output string }{
		{"knowledge tool with forged asset", "list_facts", fmt.Sprintf(`{"asset_id":%q}`, assets[0].ID), `{"status_code":200}`},
		{"missing binding", "get_endpoint", `{}`, `{"status_code":200}`},
		{"malformed binding", "get_endpoint", `{"asset_id":"invalid"}`, `{"status_code":200}`},
		{"not a response", "get_endpoint", fmt.Sprintf(`{"asset_id":%q}`, assets[0].ID), `{}`},
		{"conflicting response", "get_endpoint", fmt.Sprintf(`{"asset_id":%q}`, assets[0].ID), fmt.Sprintf(`{"request":{"asset_id":%q},"response":{"status_code":200}}`, assets[1].ID)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.pool.Exec(context.Background(), `UPDATE agent_tool_calls SET tool_name=$2,arguments=$3,output=$4 WHERE id=$1`, sourceID, tc.tool, json.RawMessage(tc.args), tc.output)
			require.NoError(t, err)
			_, err = e.runService.ReportFinding(context.Background(), runID, service.ReportFindingInput{AssetID: assets[0].ID, Title: tc.name, SourceKind: "empirical", SourceToolCallID: &sourceID})
			require.Error(t, err)
		})
	}
	var count int
	require.NoError(t, e.pool.QueryRow(context.Background(), `SELECT count(*) FROM findings WHERE run_id=$1`, runID).Scan(&count))
	require.Zero(t, count, "failed validation must leave no finding")
}

func TestReview3_RetestEvidenceMatrix(t *testing.T) {
	e, runID, assets, sourceID := review2ProbeSetup(t)
	ctx := context.Background()
	_, err := e.pool.Exec(ctx, `UPDATE agent_tool_calls SET output='{"status_code":200,"body":"SECRET_EXPOSED"}' WHERE id=$1`, sourceID)
	require.NoError(t, err)
	f, err := e.runService.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assets[0].ID, Title: "Secret disclosure", SourceKind: "empirical", SourceToolCallID: &sourceID,
		Verification: &service.FindingVerification{
			Vulnerable: service.HTTPCondition{StatusCode: 200, BodyContains: "SECRET_EXPOSED"},
			Fixed:      service.HTTPCondition{StatusCode: 403, BodyContains: "SECRET_ACCESS_DENIED"},
		},
	})
	require.NoError(t, err)
	for _, tc := range []struct {
		name, output, verdict                                 string
		wrongSession, wrongAsset, oldEpoch, wrongTool, failed bool
	}{
		{name: "explicit fixed condition", output: `{"response":{"status_code":403,"body":"SECRET_ACCESS_DENIED"}}`, verdict: "fixed"},
		{name: "explicit reproduction", output: `{"status_code":200,"body":"SECRET_EXPOSED"}`, verdict: "still_reproducible"},
		{name: "generic error page", output: `{"status_code":403,"body":"Forbidden"}`, verdict: "inconclusive"},
		{name: "success alone", output: `{"status_code":200}`, verdict: "inconclusive"},
		{name: "truncated", output: `{"integrity":{"is_truncated":true},"response":{"status_code":403,"body":"SECRET_ACCESS_DENIED"}}`, verdict: "inconclusive"},
		{name: "other session", wrongSession: true, verdict: "inconclusive"},
		{name: "other asset", wrongAsset: true, verdict: "inconclusive"},
		{name: "other epoch", oldEpoch: true, verdict: "inconclusive"},
		{name: "knowledge tool", wrongTool: true, verdict: "inconclusive"},
		{name: "failed request", failed: true, verdict: "inconclusive"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt, sess, err := e.runService.CreateFindingRetest(ctx, runID, f.ID, tc.name)
			require.NoError(t, err)
			sessionID := sess.ID
			if tc.wrongSession {
				sessionID = uuid.Nil
			}
			assetID := assets[0].ID
			if tc.wrongAsset {
				assetID = assets[1].ID
			}
			epoch := 1
			if tc.oldEpoch {
				epoch = 0
			}
			name, status := "get_endpoint", "success"
			if tc.wrongTool {
				name = "list_facts"
			}
			if tc.failed {
				status = "failed"
			}
			output := tc.output
			if output == "" {
				output = `{"status_code":403,"body":"SECRET_ACCESS_DENIED"}`
			}
			var callID uuid.UUID
			require.NoError(t, e.pool.QueryRow(ctx, `INSERT INTO agent_tool_calls(run_id,session_id,call_id,tool_name,arguments,output,status,epoch_number)
				VALUES($1,NULLIF($2,'00000000-0000-0000-0000-000000000000'::uuid),$3,$4,jsonb_build_object('asset_id',$5::text),$6,$7,$8) RETURNING id`, runID, sessionID, uuid.NewString(), name, assetID, output, status, epoch).Scan(&callID))
			ref, _ := json.Marshal(map[string]any{"source_tool_call_id": callID})
			if tc.verdict != "fixed" {
				_, err = e.runService.FinishFindingRetest(ctx, runID, f.ID, rt.ID, "fixed", string(ref), "model claims fixed")
				require.Error(t, err, "unsupported manual verdict must be rejected")
			}
			done, err := e.runService.FinishFindingRetestFromEvidence(ctx, runID, f.ID, rt.ID, string(ref), "Evaluate response")
			require.NoError(t, err)
			require.Equal(t, tc.verdict, *done.Verdict)
		})
	}
}

// The production epoch endpoint creates its provider from the immutable Run
// snapshot. An HTTP model stub exercises that path without any paid model call.
func TestReview3_EpochAPIStartsExecutor(t *testing.T) {
	e := setupTestEnv(t)
	defer e.teardown()
	ctx := context.Background()
	var restored atomic.Bool
	modelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []struct{ Role, Content string } `json:"messages"`
			Tools    []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		full := ""
		for _, m := range req.Messages {
			full += m.Content + "\n"
		}
		lastRole := ""
		if len(req.Messages) > 0 {
			lastRole = req.Messages[len(req.Messages)-1].Role
		}
		planner := false
		for _, tool := range req.Tools {
			if tool.Function.Name == "suggest_completion" {
				planner = true
			}
		}
		msg := map[string]any{"role": "assistant", "content": "acknowledged"}
		finish := "stop"
		toolName, args := "", ""
		if planner && lastRole != "tool" {
			if strings.Contains(full, "Available for Continuation") || strings.Contains(full, "is exhausted") {
				toolName, args = "continue_work", fmt.Sprintf(`{"work_item_id":%q,"instruction":"Continue from saved progress"}`, promptWorkItemID(full, "resume_me"))
			} else {
				toolName, args = "suggest_completion", `{"final_summary":"Resumed work completed","conclusion":"complete"}`
			}
		} else if !planner && lastRole != "tool" {
			if strings.Contains(full, "saved-worker-marker") {
				restored.Store(true)
			}
			toolName, args = "read_sample", `{"sample_id":"sample_home"}`
		}
		if toolName != "" {
			msg["tool_calls"] = []any{map[string]any{"id": uuid.NewString(), "type": "function", "function": map[string]any{"name": toolName, "arguments": args}}}
			finish = "tool_calls"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.NewString(), "choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}}})
	}))
	defer modelServer.Close()
	t.Setenv("LLM_API_KEY", "local-test-only")
	t.Setenv("LLM_API_FORMAT", "chat_completions")
	t.Setenv("LLM_BASE_URL", modelServer.URL)
	t.Setenv("LLM_MODEL", "local-test")
	config, _ := json.Marshal(map[string]any{"agent_config": map[string]any{"executor": "orchestrator", "provider": "openai"}})
	task, err := e.taskService.Create(ctx, model.CreateTaskRequest{Name: "Epoch execution", Scope: json.RawMessage(`{"endpoints":["sample_home"]}`), Config: config})
	require.NoError(t, err)
	run, _, err := e.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &model.BudgetConfig{MaxModelCalls: 30, MaxToolCalls: 30, RunTimeoutSeconds: 20, MaxParallelWorkers: 1}}, nil)
	require.NoError(t, err)
	_, err = e.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)
	_, err = e.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "resume_me", Title: "Resume saved work", WorkType: "analyze_sample", InputData: map[string]interface{}{"sample_id": "sample_home"}}})
	require.NoError(t, err)
	_, oldAttempt, oldSession, err := e.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	_, err = e.runService.InsertAgentMessageWithSession(ctx, run.ID, &oldSession.ID, "user", "saved-worker-marker", nil, nil)
	require.NoError(t, err)
	_, err = e.runService.CreatePlannerRound(ctx, run.ID, 1, "initial", nil)
	require.NoError(t, err)
	require.NoError(t, e.runService.FinalizeRunWrapup(ctx, run.ID, model.CauseToolBudgetExhausted, nil))
	// A long stopped interval must not consume the next epoch's execution time.
	_, err = e.pool.Exec(ctx, `UPDATE runs SET started_at=now()-interval '1 hour',finished_at=now()-interval '3598 seconds' WHERE id=$1`, run.ID)
	require.NoError(t, err)
	req := controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/epochs/new", 1, "", `{"reason":"operator approved continuation"}`)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code, w.Body.String())
	require.Eventually(t, func() bool { r, err := e.runService.Get(ctx, run.ID); return err == nil && r.Status.IsTerminal() }, 6*time.Second, 20*time.Millisecond)
	after, err := e.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, 2, after.CurrentEpoch)
	require.Equal(t, model.RunStatusCompleted, after.Status)
	require.True(t, restored.Load(), "Worker must receive its old conversation")
	var attempts, reads, rounds int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM attempts WHERE work_item_id=(SELECT work_item_id FROM attempts WHERE id=$1) AND epoch_number=2 AND status='completed'`, oldAttempt.ID).Scan(&attempts))
	require.Equal(t, 1, attempts)
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_tool_calls WHERE run_id=$1 AND epoch_number=2 AND tool_name='read_sample' AND status='success'`, run.ID).Scan(&reads))
	require.Equal(t, 1, reads)
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT min(round) FROM planner_rounds WHERE run_id=$1 AND round>1`, run.ID).Scan(&rounds))
	require.Equal(t, 2, rounds)
	require.ErrorIs(t, e.runService.FinalizeRunWrapupWithEpoch(ctx, run.ID, model.CauseInternalError, nil, 1), service.ErrStaleEpoch)
	_, err = e.runService.StartNewEpoch(ctx, run.ID, "retry completed task", "")
	require.True(t, errors.Is(err, service.ErrInvalidState))
}

func TestReview3_EpochStartFailureLeavesTerminalRun(t *testing.T) {
	e := setupTestEnv(t)
	defer e.teardown()
	ctx := context.Background()
	task, err := e.taskService.Create(ctx, model.CreateTaskRequest{Name: "Rejected epoch executor", Config: json.RawMessage(`{"agent_config":{"executor":"orchestrator","provider":"mock"}}`)})
	require.NoError(t, err)
	run, _, err := e.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	_, err = e.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)
	previousSummary := "Previous budget stop summary"
	require.NoError(t, e.runService.FinalizeRunWrapup(ctx, run.ID, model.CauseToolBudgetExhausted, &previousSummary))
	e.manager.Close()
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/epochs/new", 1, "", `{"reason":"resume after grant"}`))
	require.Equal(t, http.StatusServiceUnavailable, w.Code, w.Body.String())
	after, err := e.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusFailed, after.Status)
	require.NotNil(t, after.FinishedAt)
	epochs, err := e.runService.ListExecutionEpochs(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, epochs, 2)
	require.Equal(t, previousSummary, *epochs[0].Summary)
	require.Equal(t, "failed", epochs[1].Status)
}

type review3DrainingExecutor struct{ started, exited chan struct{} }

func (e *review3DrainingExecutor) Run(ctx context.Context) {
	close(e.started)
	<-ctx.Done()
	close(e.exited)
}

func TestReview3_QuiesceWaitsForExecutor(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	drainer := &review3DrainingExecutor{started: make(chan struct{}), exited: make(chan struct{})}
	require.NoError(t, e.manager.StartExecutor(runID, drainer))
	<-drainer.started
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	require.NoError(t, e.manager.QuiesceEpoch(ctx, runID))
	select {
	case <-drainer.exited:
	default:
		t.Fatal("old executor still running")
	}
	run, err := e.runService.Get(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusInterrupted, run.Status, "the previous executor must finish cleanup before returning")
}
