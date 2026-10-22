package test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/stretchr/testify/require"
)

func TestP5_PauseAndResumeRun(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P5 Task",
		Objective: "Test pause resume",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	require.NoError(t, err)

	// 1. Pause run via REST API
	req := controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/pause", 1, "", "")
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("pause failed (%d): %s", w.Code, w.Body.String())
	}

	var pauseRes map[string]string
	err = json.Unmarshal(w.Body.Bytes(), &pauseRes)
	require.NoError(t, err)
	require.Equal(t, "paused", pauseRes["status"])

	// Verify run status in DB is paused
	rRecord, err := env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusPaused, rRecord.Status)

	// Pausing again returns conflict
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/pause", 1, "", "")
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusConflict, w.Code)

	// Worker claim while paused returns nil (no items claimed)
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.Nil(t, wi)
	require.Nil(t, att)
	require.Nil(t, sess)

	// 2. Resume run via REST API
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/resume", 1, "", "")
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	rRecord, err = env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusRunning, rRecord.Status)

	// Events verify
	events, err := env.runService.GetEvents(ctx, run.ID)
	require.NoError(t, err)
	hasPauseEvent := false
	hasResumeEvent := false
	for _, ev := range events {
		if ev.Type == "state_change" {
			var p map[string]any
			_ = json.Unmarshal(ev.Payload, &p)
			if p["to"] == "paused" {
				hasPauseEvent = true
			}
			if p["to"] == "running" {
				hasResumeEvent = true
			}
		}
	}
	require.True(t, hasPauseEvent)
	require.True(t, hasResumeEvent)
}

func TestP5_SteerAndKillWorkItem(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P5 Steer Task",
		Objective: "Test steer and kill",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	require.NoError(t, err)

	// Plan 2 items
	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item-1", Title: "Target A", Objective: "Analyze port 80", WorkType: "intent"},
		{Key: "item-2", Title: "Target B", Objective: "Analyze port 443", WorkType: "intent"},
	})
	require.NoError(t, err)
	require.Len(t, items, 2)

	// Claim item-1
	claimedWi, att, _, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, claimedWi)
	require.Equal(t, items[0].ID, claimedWi.ID)

	// 1. Steer item-1: online guidance
	steerBody, _ := json.Marshal(map[string]any{
		"instructions": "Skip port 80, focus directly on admin login path /wp-admin",
	})
	req := controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/work_items/"+claimedWi.ID.String()+"/steer", 1, "", string(steerBody))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// Guidance is delivered separately; the original objective/input remain immutable.
	steeredWi, err := env.runService.GetWorkItem(ctx, claimedWi.ID)
	require.NoError(t, err)
	require.Equal(t, claimedWi.Objective, steeredWi.Objective)
	require.JSONEq(t, string(claimedWi.InputData), string(steeredWi.InputData))

	// Verify hint was created in exploration_nodes
	var hintCount int
	err = env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM exploration_nodes WHERE run_id=$1 AND kind='hint' AND origin='human_steer'`, run.ID).Scan(&hintCount)
	require.NoError(t, err)
	require.Equal(t, 1, hintCount)

	// 2. Kill item-2 while pending/running
	killBody, _ := json.Marshal(map[string]string{
		"reason": "abandoned_subpath",
	})
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/work_items/"+items[1].ID.String()+"/kill", 1, "", string(killBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	killedWi, err := env.runService.GetWorkItem(ctx, items[1].ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkItemStatusCancelled, killedWi.Status)

	// Verify run remains in running state
	rRecord, err := env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusRunning, rRecord.Status)

	// Check kill event
	events, err := env.runService.GetEvents(ctx, run.ID)
	require.NoError(t, err)
	hasKillEvent := false
	for _, ev := range events {
		if ev.Type == "work_item_killed" {
			hasKillEvent = true
		}
	}
	require.True(t, hasKillEvent)

	// Complete item-1
	err = env.runService.FinishWorkItem(ctx, claimedWi.ID, att.ID, *att.SessionID, true, json.RawMessage(`{"status":"done"}`), "success")
	require.NoError(t, err)
}

func TestP5_BudgetGrantLedgerAndEffectiveBudget(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P5 Grants Task",
		Objective: "Test budget grants",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{
		Budget: &model.BudgetConfig{
			MaxModelCalls:     5,
			MaxToolCalls:      10,
			RunTimeoutSeconds: 60,
		},
	}, nil)
	require.NoError(t, err)

	// 1. Initial effective budget check
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID.String()+"/budget/effective", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var effective model.BudgetConfig
	err = json.Unmarshal(w.Body.Bytes(), &effective)
	require.NoError(t, err)
	require.Equal(t, 5, effective.MaxModelCalls)
	require.Equal(t, 10, effective.MaxToolCalls)
	require.Equal(t, 60, effective.RunTimeoutSeconds)

	// 2. Grant budget via REST API
	idempotencyKey := "grant-p5-001"
	grantBody, _ := json.Marshal(map[string]any{
		"additional_model_calls": 15,
		"additional_tool_calls":  25,
		"additional_seconds":     120,
		"reason":                 "Deepening reconnaissance authorized by operator",
		"idempotency_key":        idempotencyKey,
	})
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/grant_budget", 1, idempotencyKey, string(grantBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var grant model.BudgetGrant
	err = json.Unmarshal(w.Body.Bytes(), &grant)
	require.NoError(t, err)
	require.Equal(t, 15, grant.AdditionalModelCalls)
	require.Equal(t, 25, grant.AdditionalToolCalls)
	require.Equal(t, 120, grant.AdditionalSeconds)
	require.Equal(t, 1, grant.EpochNumber)

	// 3. Repeat grant with same idempotencyKey - should not duplicate amounts
	req = controlReq(http.MethodPost, "/api/runs/"+run.ID.String()+"/grant_budget", 1, idempotencyKey, string(grantBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// 4. Verify effective budget is sum = base + single grant
	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID.String()+"/budget/effective", nil)
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	err = json.Unmarshal(w.Body.Bytes(), &effective)
	require.NoError(t, err)
	require.Equal(t, 20, effective.MaxModelCalls)      // 5 + 15
	require.Equal(t, 35, effective.MaxToolCalls)       // 10 + 25
	require.Equal(t, 180, effective.RunTimeoutSeconds) // 60 + 120

	// 5. List grants
	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID.String()+"/budget_grants", nil)
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var listRes struct {
		Grants []model.BudgetGrant `json:"grants"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &listRes)
	require.NoError(t, err)
	require.Len(t, listRes.Grants, 1)
	require.Equal(t, idempotencyKey, *listRes.Grants[0].IdempotencyKey)
}

func TestP5_ExecutionEpochsAndStaleWriteRejection(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P5 Epoch Task",
		Objective: "Test epochs",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)
	require.NoError(t, err)

	// Submit work item
	items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "epoch-item-1", Title: "Epoch Item", Objective: "Test stale rejection", WorkType: "intent"},
	})
	require.NoError(t, err)
	require.Len(t, items, 1)

	// Claim under Epoch 1
	claimedWi, attEpoch1, sessEpoch1, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, claimedWi)

	// Verify attEpoch1 has epoch_number = 1
	var attEpochNum, sessEpochNum int
	err = env.pool.QueryRow(ctx, `SELECT epoch_number FROM attempts WHERE id=$1`, attEpoch1.ID).Scan(&attEpochNum)
	require.NoError(t, err)
	require.Equal(t, 1, attEpochNum)

	err = env.pool.QueryRow(ctx, `SELECT epoch_number FROM agent_sessions WHERE id=$1`, sessEpoch1.ID).Scan(&sessEpochNum)
	require.NoError(t, err)
	require.Equal(t, 1, sessEpochNum)

	// Service-level fence test; the real HTTP epoch execution is covered by TestReview3_EpochAPIStartsExecutor.
	epoch, err := env.runService.StartNewEpoch(ctx, run.ID, "manual_operator_intervention", "Starting epoch 2")
	require.NoError(t, err)
	require.Equal(t, 2, epoch)

	// ValidateEpoch on RunService
	err = env.runService.ValidateEpoch(ctx, run.ID, 1)
	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrStaleEpoch))

	err = env.runService.ValidateEpoch(ctx, run.ID, 2)
	require.NoError(t, err)

	// 2. CONTRACT E6: Attempt from Epoch 1 tries to write tool call -> Rejected!
	_, err = env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessEpoch1.ID, nil, "tc-stale", "record_fact", json.RawMessage(`{"fact":"late"}`))
	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrStaleEpoch), "expected ErrStaleEpoch, got: %v", err)

	// 3. CONTRACT E6: Attempt from Epoch 1 tries to finish work item -> Rejected!
	err = env.runService.FinishWorkItem(ctx, claimedWi.ID, attEpoch1.ID, sessEpoch1.ID, true, json.RawMessage(`{"status":"stale"}`), "success")
	require.Error(t, err)
	require.True(t, errors.Is(err, service.ErrStaleEpoch), "expected ErrStaleEpoch, got: %v", err)

	// 4. In Epoch 2, we can retry or continue the work item under the new epoch
	// First mark attempt 1 stopped so item is retryable
	_, err = env.pool.Exec(ctx, `UPDATE attempts SET status='stopped', finish_reason='superseded_by_new_epoch' WHERE id=$1`, attEpoch1.ID)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `UPDATE work_items SET status='exhausted' WHERE id=$1`, claimedWi.ID)
	require.NoError(t, err)

	continuedWi, err := env.runService.ContinueWorkItem(ctx, run.ID, claimedWi.ID, attEpoch1.ID, "epoch 2 resumption")
	require.NoError(t, err)
	require.NotNil(t, continuedWi)
	require.Equal(t, model.WorkItemStatusPending, continuedWi.Status)

	claimedWi2, attEpoch2, sessEpoch2, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.NotNil(t, claimedWi2)
	require.NotNil(t, attEpoch2)
	require.NotNil(t, sessEpoch2)

	// Verify new attempt is under Epoch 2
	err = env.pool.QueryRow(ctx, `SELECT epoch_number FROM attempts WHERE id=$1`, attEpoch2.ID).Scan(&attEpochNum)
	require.NoError(t, err)
	require.Equal(t, 2, attEpochNum)

	err = env.pool.QueryRow(ctx, `SELECT epoch_number FROM agent_sessions WHERE id=$1`, sessEpoch2.ID).Scan(&sessEpochNum)
	require.NoError(t, err)
	require.Equal(t, 2, sessEpochNum)

	// In Epoch 2, tool call succeeds!
	tc, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessEpoch2.ID, nil, "tc-valid", "record_fact", json.RawMessage(`{"fact":"fresh"}`))
	require.NoError(t, err)
	require.NotNil(t, tc)

	// In Epoch 2, finish succeeds!
	err = env.runService.FinishWorkItem(ctx, claimedWi.ID, attEpoch2.ID, sessEpoch2.ID, true, json.RawMessage(`{"status":"valid"}`), "success")
	require.NoError(t, err)

	// 5. Verify epoch history
	req := httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID.String()+"/epochs", nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var epochsRes struct {
		Epochs []model.ExecutionEpoch `json:"epochs"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &epochsRes)
	require.NoError(t, err)
	require.Len(t, epochsRes.Epochs, 2)
	require.Equal(t, 1, epochsRes.Epochs[0].EpochNumber)
	require.Equal(t, "stopped", epochsRes.Epochs[0].Status)
	require.Equal(t, 2, epochsRes.Epochs[1].EpochNumber)
	require.Equal(t, "running", epochsRes.Epochs[1].Status)
}
