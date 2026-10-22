package test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRemaining_RetractedEvidenceAndChangedGoalInvalidateProofs(t *testing.T) {
	e, runID, assets, tc := review2ProbeSetup(t)
	ctx := context.Background()
	f, err := e.runService.RecordFact(ctx, runID, "Verified response", "status observed", "observed", tc.String(), nil, []uuid.UUID{assets[0].ID})
	require.NoError(t, err)
	require.Equal(t, "verified", f.State)
	before, err := e.runService.LatestExplorationChange(ctx, runID, 1)
	require.NoError(t, err)
	duplicate, err := e.runService.RecordFact(ctx, runID, "Verified response", "status observed", "observed", tc.String(), nil, []uuid.UUID{assets[0].ID})
	require.NoError(t, err)
	require.Equal(t, f.ID, duplicate.ID)
	require.False(t, duplicate.IsNew)
	after, err := e.runService.LatestExplorationChange(ctx, runID, 1)
	require.NoError(t, err)
	require.Equal(t, before, after, "identical facts must not manufacture progress")
	goals, err := e.runService.SetGoals(ctx, runID, []model.GoalSpec{{Text: "Observe the authorized response"}})
	require.NoError(t, err)
	_, err = e.runService.ProveGoal(ctx, runID, goals[0].ID, f.ID, "Response evidence matches this deliverable")
	require.NoError(t, err)
	cause, err := e.runService.CompletionCause(ctx, runID, "complete")
	require.NoError(t, err)
	require.Equal(t, model.CauseCompleted, cause)
	require.NoError(t, e.runService.ReviseExplorationNode(ctx, runID, f.ID, 1, nil, "retracted", "Source response is no longer reliable", "reviewer"))
	_, err = e.runService.ProveGoal(ctx, runID, goals[0].ID, f.ID, "Try using retracted evidence")
	require.Error(t, err)
	current, err := e.runService.ListGoals(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, "open", current[0].State)
	var valid bool
	var sourceVersion int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT valid,source_version FROM exploration_edges WHERE src_id=$1 AND dst_id=$2 AND rel='proves'`, f.ID, goals[0].ID).Scan(&valid, &sourceVersion))
	require.False(t, valid)
	require.Equal(t, 1, sourceVersion)
	require.ErrorIs(t, e.runService.ReviseExplorationNode(ctx, runID, f.ID, 1, nil, "retracted", "stale update", "reviewer"), service.ErrConflict)
	f2, err := e.runService.RecordFact(ctx, runID, "Replacement response evidence", "Reviewed original response", "observed", tc.String(), nil, []uuid.UUID{assets[0].ID})
	require.NoError(t, err)
	_, err = e.runService.ProveGoal(ctx, runID, goals[0].ID, f2.ID, "Current evidence reviewed")
	require.NoError(t, err)
	require.NoError(t, e.runService.ReviseExplorationNode(ctx, runID, goals[0].ID, 1, json.RawMessage(`{"text":"A different deliverable requires new review"}`), "open", "Operator changed the deliverable", "reviewer"))
	cause, err = e.runService.CompletionCause(ctx, runID, "complete")
	require.NoError(t, err)
	require.Equal(t, model.CauseNeedsInput, cause)
	var revisions int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM exploration_revisions WHERE run_id=$1`, runID).Scan(&revisions))
	require.Equal(t, 2, revisions)
}

func TestRemaining_FindingIDProofRejectsFixedAndPreservesEpoch(t *testing.T) {
	e, runID, assets, tc := review2ProbeSetup(t)
	ctx := context.Background()
	finding, err := e.runService.ReportFinding(ctx, runID, service.ReportFindingInput{AssetID: assets[0].ID, Title: "Scoped finding", Category: "test", Severity: "low", Description: "Real scoped response", SourceKind: "empirical", SourceToolCallID: &tc})
	require.NoError(t, err)
	goals, err := e.runService.SetGoals(ctx, runID, []model.GoalSpec{{Text: "Prove current evidence"}})
	require.NoError(t, err)
	// Exercise the public finding-ID fallback, including a migrated finding
	// without its exploration projection. Fixed evidence must not be recreated
	// as a valid proof merely by using its finding UUID.
	_, err = e.pool.Exec(ctx, `DELETE FROM exploration_nodes WHERE finding_id=$1`, finding.ID)
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `UPDATE findings SET retest_verdict='fixed' WHERE id=$1`, finding.ID)
	require.NoError(t, err)
	_, err = e.runService.ProveGoal(ctx, runID, goals[0].ID, finding.ID, "Attempt to reuse invalid evidence")
	require.ErrorIs(t, err, service.ErrInvalidEvidence)
	_, err = e.pool.Exec(ctx, `UPDATE findings SET retest_verdict=NULL WHERE id=$1`, finding.ID)
	require.NoError(t, err)
	epoch, err := e.runService.StartNewEpoch(ctx, runID, "Resume with retained evidence", "")
	require.NoError(t, err)
	_, err = e.runService.ProveGoal(service.WithExecutionEpoch(ctx, epoch), runID, goals[0].ID, finding.ID, "Review retained evidence in the current epoch")
	require.NoError(t, err)
	var nodeEpoch int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT epoch_number FROM exploration_nodes WHERE finding_id=$1`, finding.ID).Scan(&nodeEpoch))
	require.Equal(t, epoch, nodeEpoch)
}

func TestRemaining_SteerIsDurableIdempotentAndCannotChangeInputs(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	_, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "steered", Title: "Direction", Objective: "Original objective", WorkType: "intent"}})
	require.NoError(t, err)
	wi, _, session, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	url := "/api/runs/" + runID.String() + "/work_items/" + wi.ID.String() + "/steer"
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		r := controlReq(http.MethodPost, url, 1, "same-guidance-command", `{"instructions":"Inspect saved evidence before sending requests"}`)
		e.router.ServeHTTP(w, r)
		require.Equal(t, 200, w.Code, w.Body.String())
	}
	// Reconstruct the service to ensure delivery does not depend on the old Engine's map.
	restarted := service.NewRunService(e.pool)
	messages, err := restarted.ConsumeSteerMessages(ctx, runID, wi.ID, session.ID)
	require.NoError(t, err)
	require.Len(t, messages, 1)
	again, err := restarted.ConsumeSteerMessages(ctx, runID, wi.ID, session.ID)
	require.NoError(t, err)
	require.Empty(t, again)
	raw, err := e.runService.ListAgentMessagesBySession(ctx, session.ID)
	require.NoError(t, err)
	count := 0
	for _, m := range raw {
		if strings.Contains(m.Content, "Inspect saved evidence") {
			count++
		}
	}
	require.Equal(t, 1, count)
	kept, err := e.runService.GetWorkItem(ctx, wi.ID)
	require.NoError(t, err)
	require.Equal(t, "Original objective", kept.Objective)
	w := httptest.NewRecorder()
	e.router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, url, bytes.NewBufferString(`{"instructions":"Change capability","input_data_override":{"profile":"invented_shell"}}`)))
	require.Equal(t, 409, w.Code)
}

func TestRemaining_EpochCommandDoesNotDuplicateAfterResponseLoss(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	opt := service.EpochStartOptions{ExpectedEpoch: 1, IdempotencyKey: "lost-response"}
	first, err := e.runService.StartNewEpoch(ctx, runID, "Resume approved work", "Keep evidence", opt)
	require.NoError(t, err)
	again, err := e.runService.StartNewEpoch(ctx, runID, "Resume approved work", "Keep evidence", opt)
	require.NoError(t, err)
	require.Equal(t, first, again)
	_, err = e.runService.StartNewEpoch(ctx, runID, "Different command", "Keep evidence", opt)
	require.ErrorIs(t, err, service.ErrConflict)
	_, err = e.runService.StartNewEpoch(ctx, runID, "Resume approved work", "", service.EpochStartOptions{ExpectedEpoch: 1, IdempotencyKey: "new-command-old-version"})
	require.ErrorIs(t, err, service.ErrStaleEpoch)
	epochs, err := e.runService.ListExecutionEpochs(ctx, runID)
	require.NoError(t, err)
	require.Len(t, epochs, 2)
}

func TestRemaining_NewEpochCanContinueStoppedWorkButKeepsExplicitKill(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{
		{Key: "resume_after_stop", Title: "Interrupted direction", Objective: "Keep the saved direction", WorkType: "intent"},
		{Key: "explicit_kill", Title: "Killed direction", Objective: "Operator revoked this direction", WorkType: "intent"},
	})
	require.NoError(t, err)
	wi, attempt, session, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, items[0].ID, wi.ID)
	_, err = e.runService.InsertAgentMessageWithSession(ctx, runID, &session.ID, "user", "saved-before-run-stop", nil, nil)
	require.NoError(t, err)
	require.NoError(t, e.runService.KillWorkItem(ctx, runID, items[1].ID, "operator revoked this direction"))
	require.NoError(t, e.runService.FinalizeRunWrapup(ctx, runID, model.CauseUserStop, nil))
	epochs, err := e.runService.ListExecutionEpochs(ctx, runID)
	require.NoError(t, err)
	require.Len(t, epochs, 1)
	require.Equal(t, "stopped", epochs[0].Status)
	_, err = e.runService.StartNewEpoch(ctx, runID, "Operator approved continuation", "Keep the original audit history")
	require.NoError(t, err)
	reopened, err := e.runService.GetWorkItem(ctx, wi.ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkItemStatusExhausted, reopened.Status, "Planner must still explicitly choose continuation")
	killed, err := e.runService.GetWorkItem(ctx, items[1].ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkItemStatusCancelled, killed.Status)
	_, err = e.runService.ContinueWorkItem(ctx, runID, wi.ID, attempt.ID, "Continue the interrupted transcript")
	require.NoError(t, err)
	_, nextAttempt, nextSession, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	require.NotEqual(t, attempt.ID, nextAttempt.ID)
	require.NotEqual(t, session.ID, nextSession.ID)
	history, err := e.runService.LoadWorkItemContext(ctx, wi.ID)
	require.NoError(t, err)
	require.Contains(t, history[0].Content, "saved-before-run-stop")
	var oldStatus string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT status FROM attempts WHERE id=$1`, attempt.ID).Scan(&oldStatus))
	require.Equal(t, "stopped", oldStatus)
}

func TestRemaining_DatabaseRejectsCrossRunGraphEdge(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	goals, err := e.runService.SetGoals(ctx, runID, []model.GoalSpec{{Text: "First Run"}})
	require.NoError(t, err)
	task, err := e.taskService.Create(ctx, model.CreateTaskRequest{Name: "Other graph"})
	require.NoError(t, err)
	other, _, err := e.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	foreign, err := e.runService.SetGoals(ctx, other.ID, []model.GoalSpec{{Text: "Other Run"}})
	require.NoError(t, err)
	_, err = e.pool.Exec(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel) VALUES($1,$2,$3,'proves')`, runID, goals[0].ID, foreign[0].ID)
	require.Error(t, err, "database must enforce scope even when bypassing a handler")
}

func TestRemaining_DurableHintWakesPlannerWithoutMemoryNotification(t *testing.T) {
	e := setupTestEnv(t)
	defer e.teardown()
	ctx := context.Background()
	task, err := e.taskService.Create(ctx, model.CreateTaskRequest{Name: "Durable changefeed"})
	require.NoError(t, err)
	budget := model.DefaultBudgetConfig()
	budget.MaxParallelWorkers = 2
	budget.MaxModelCalls = 50
	budget.MaxPlannerRounds = 12
	run, _, err := e.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	require.NoError(t, err)
	aStarted, bStarted, releaseA := make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{})
	p := &testFakeProvider{callFunc: func(callCtx context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
		last := msgs[len(msgs)-1]
		full := ""
		for _, m := range msgs {
			full += m.Content
		}
		call := func(name, args string) (*provider.ModelResponse, error) {
			return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: uuid.NewString(), Type: "function", Function: provider.FunctionCall{Name: name, Arguments: args}}}}, nil
		}
		planner := false
		for _, d := range defs {
			if d.Name == "submit_plan" {
				planner = true
			}
		}
		if planner {
			if last.Role == provider.RoleTool {
				return &provider.ModelResponse{Content: "Planner acknowledged", FinishReason: "stop"}, nil
			}
			if strings.Contains(last.Content, "Break down") {
				return call("submit_plan", `{"items":[{"key":"durable_a","title":"Wait for clue","objective":"Keep investigating A","work_type":"intent"}]}`)
			}
			if strings.Contains(full, "new-material-clue") && !strings.Contains(full, "durable_b") {
				return call("submit_plan", `{"items":[{"key":"durable_b","title":"Investigate clue","objective":"Follow the operator clue","work_type":"intent"}]}`)
			}
			if strings.Contains(full, "durable_b") {
				return call("suggest_completion", `{"final_summary":"Work retained; goals still need review","conclusion":"partial"}`)
			}
			return &provider.ModelResponse{Content: "Wait for material evidence", FinishReason: "stop"}, nil
		}
		if strings.Contains(full, "durable_a") {
			select {
			case aStarted <- struct{}{}:
			default:
			}
			select {
			case <-releaseA:
			case <-callCtx.Done():
				return nil, callCtx.Err()
			}
			return &provider.ModelResponse{Content: "A finished", FinishReason: "stop"}, nil
		}
		select {
		case bStarted <- struct{}{}:
		default:
		}
		return &provider.ModelResponse{Content: "B finished", FinishReason: "stop"}, nil
	}}
	engine := orchestration.NewEngine(run.ID, e.runService, p, budget)
	require.NoError(t, e.manager.StartExecutor(run.ID, engine))
	select {
	case <-aStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("A did not start")
	}
	// This direct service call deliberately sends no Engine.Nudge callback.
	_, err = e.runService.RecordHint(ctx, run.ID, "new-material-clue", nil)
	require.NoError(t, err)
	select {
	case <-bStarted:
		close(releaseA)
	case <-time.After(3 * time.Second):
		close(releaseA)
		t.Fatal("persisted hint did not wake Planner while A was still running")
	}
	require.Eventually(t, func() bool { r, err := e.runService.Get(ctx, run.ID); return err == nil && r.Status.IsTerminal() }, 4*time.Second, 20*time.Millisecond)
	var cursor int64
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT change_seq FROM planner_cursors WHERE run_id=$1 AND epoch_number=1`, run.ID).Scan(&cursor))
	require.Positive(t, cursor)
}
