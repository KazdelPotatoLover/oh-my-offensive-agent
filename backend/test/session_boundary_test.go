package test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

// TestSession_Isolation verifies that messages in different sessions under the same Run
// remain strictly isolated, each possessing monotonic seq numbers starting at 1.
func TestSession_Isolation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Session Isolation Task",
		Objective: "Verify session isolation",
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Create Session 1 (Planner)
	round1 := 1
	sess1, err := env.runService.CreateSession(ctx, run.ID, "planner", &round1, nil, nil, json.RawMessage(`{"prompt":"plan 1"}`))
	if err != nil {
		t.Fatalf("failed to create session 1: %v", err)
	}

	// Create Session 2 (Worker)
	sess2, err := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, json.RawMessage(`{"prompt":"worker 1"}`))
	if err != nil {
		t.Fatalf("failed to create session 2: %v", err)
	}

	// Insert messages into Session 1
	msg1_1, err := env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess1.ID, "system", "planner system", nil, nil)
	if err != nil {
		t.Fatalf("failed to insert msg1_1: %v", err)
	}
	msg1_2, err := env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess1.ID, "user", "plan objective", nil, nil)
	if err != nil {
		t.Fatalf("failed to insert msg1_2: %v", err)
	}

	// Insert messages into Session 2
	msg2_1, err := env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess2.ID, "system", "worker system", nil, nil)
	if err != nil {
		t.Fatalf("failed to insert msg2_1: %v", err)
	}
	msg2_2, err := env.runService.InsertAgentMessageWithSession(ctx, run.ID, &sess2.ID, "user", "work objective", nil, nil)
	if err != nil {
		t.Fatalf("failed to insert msg2_2: %v", err)
	}

	// Verify sequence numbers start at 1 in both sessions
	if msg1_1.Seq != 1 || msg1_2.Seq != 2 {
		t.Errorf("expected session 1 seq to be 1, 2; got %d, %d", msg1_1.Seq, msg1_2.Seq)
	}
	if msg2_1.Seq != 1 || msg2_2.Seq != 2 {
		t.Errorf("expected session 2 seq to be 1, 2; got %d, %d", msg2_1.Seq, msg2_2.Seq)
	}

	// Verify querying by session returns only that session's messages
	s1Msgs, err := env.runService.ListAgentMessagesBySession(ctx, sess1.ID)
	if err != nil {
		t.Fatalf("failed to list session 1 msgs: %v", err)
	}
	if len(s1Msgs) != 2 {
		t.Fatalf("expected 2 messages in session 1, got %d", len(s1Msgs))
	}
	if s1Msgs[0].Content != "planner system" || s1Msgs[1].Content != "plan objective" {
		t.Errorf("unexpected content in session 1: %+v", s1Msgs)
	}

	s2Msgs, err := env.runService.ListAgentMessagesBySession(ctx, sess2.ID)
	if err != nil {
		t.Fatalf("failed to list session 2 msgs: %v", err)
	}
	if len(s2Msgs) != 2 {
		t.Fatalf("expected 2 messages in session 2, got %d", len(s2Msgs))
	}
	if s2Msgs[0].Content != "worker system" || s2Msgs[1].Content != "work objective" {
		t.Errorf("unexpected content in session 2: %+v", s2Msgs)
	}
}

// TestSession_ObservationVerification verifies that CreateObservation strictly validates
// source tool call existence, run affiliation, session affiliation, and success status.
func TestSession_ObservationVerification(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Obs Task",
		Objective: "Obs test",
		Scope:     json.RawMessage(`{"endpoints":["sample_home","sample_login","sample_health"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	var homeAssetID uuid.UUID
	for _, a := range runAssets {
		if a.ExternalKey == "sample_home" {
			homeAssetID = a.ID
			break
		}
	}

	// Create a WorkItem & Attempt
	planItems, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "item_1",
			Title:     "Item 1",
			Objective: "Inspect sample",
			WorkType:  "analyze_sample",
			AssetIDs:  []uuid.UUID{homeAssetID},
			InputData: map[string]interface{}{"sample_id": "sample_home"},
		},
	})
	if err != nil {
		t.Fatalf("failed to submit plan: %v", err)
	}

	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wi == nil || att == nil || sess == nil {
		t.Fatalf("failed to claim work item: %v (wi=%v)", err, wi)
	}

	// 1. Create a successful tool call in this session
	tcSuccess, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_ok", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`))
	if err != nil {
		t.Fatalf("failed to create tool call: %v", err)
	}
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcSuccess.ID, "success", `{"status_code": 200}`, nil, &now, &now)

	// Valid observation succeeds
	obs, err := env.runService.CreateObservation(
		ctx, run.ID, att.ID, "http_status", "status was 200", json.RawMessage(`{"status_code":200}`), tcSuccess.ID)
	if err != nil {
		t.Fatalf("expected observation creation to succeed, got: %v", err)
	}
	if obs.SourceToolCallID != tcSuccess.ID {
		t.Errorf("expected source tool call id %s, got %s", tcSuccess.ID, obs.SourceToolCallID)
	}

	// 2. Reject non-existent tool call
	fakeID := uuid.New()
	_, err = env.runService.CreateObservation(
		ctx, run.ID, att.ID, "http_status", "status was 200", nil, fakeID)
	if !errors.Is(err, service.ErrInvalidObservationSource) {
		t.Errorf("expected ErrInvalidObservationSource for non-existent tool call, got %v", err)
	}

	// 3. Reject failed tool call
	tcFailed, _ := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_fail", "read_sample", json.RawMessage(`{}`))
	_ = env.runService.UpdateAgentToolCall(ctx, tcFailed.ID, "failed", "", nil, &now, &now)

	_, err = env.runService.CreateObservation(
		ctx, run.ID, att.ID, "http_status", "failed output", nil, tcFailed.ID)
	if !errors.Is(err, service.ErrInvalidObservationSource) {
		t.Errorf("expected ErrInvalidObservationSource for failed tool call, got %v", err)
	}

	// 4. Reject tool call belonging to a different session
	otherSess, _ := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil)
	tcOther, _ := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &otherSess.ID, nil, "call_other", "read_sample", json.RawMessage(`{}`))
	_ = env.runService.UpdateAgentToolCall(ctx, tcOther.ID, "success", `{"ok":true}`, nil, &now, &now)

	_, err = env.runService.CreateObservation(
		ctx, run.ID, att.ID, "http_status", "cross session", nil, tcOther.ID)
	if !errors.Is(err, service.ErrInvalidObservationSource) {
		t.Errorf("expected ErrInvalidObservationSource for cross-session tool call, got %v", err)
	}
	_ = planItems
}

// TestObservationBatch_IsAtomicAndTraceable verifies that a page's structured
// observations can be written together without allowing an invalid source to
// produce a partial batch.
func TestObservationBatch_IsAtomicAndTraceable(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Observation batch", Objective: "batch", Scope: json.RawMessage(`{"endpoints":["sample_home"]}`)})
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	assets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	var home uuid.UUID
	for _, a := range assets {
		if a.ExternalKey == "sample_home" {
			home = a.ID
		}
	}
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "page", Title: "page", Objective: "page", WorkType: "analyze_sample", InputData: map[string]any{"sample_id": "sample_home"}, AssetIDs: []uuid.UUID{home}}})
	if err != nil {
		t.Fatal(err)
	}
	_, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	call := func(id string) *model.AgentToolCall {
		tc, e := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, id, "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`))
		if e != nil {
			t.Fatal(e)
		}
		if e = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", `{"status_code":200,"title":"Home"}`, nil, &now, &now); e != nil {
			t.Fatal(e)
		}
		return tc
	}
	first, second := call("page-one"), call("page-two")
	obs, err := env.runService.CreateObservationsBatchWithAssets(ctx, run.ID, att.ID, []service.ObservationInput{
		{ObsType: "page_observation", Content: "status 200 title Home", Data: json.RawMessage(`{"status_code":200,"title":"Home"}`), SourceToolCallID: first.ID, AssetID: &home},
		{ObsType: "content_summary", Content: "home page summary", Data: json.RawMessage(`{"summary":"welcome"}`), SourceToolCallID: second.ID, AssetID: &home},
	})
	if err != nil || len(obs) != 2 {
		t.Fatalf("batch err=%v count=%d", err, len(obs))
	}
	stored, err := env.runService.ListObservationsByRun(ctx, run.ID)
	if err != nil || len(stored) != 2 {
		t.Fatalf("stored observations=%d err=%v", len(stored), err)
	}
	bad := uuid.New()
	_, err = env.runService.CreateObservationsBatchWithAssets(ctx, run.ID, att.ID, []service.ObservationInput{
		{ObsType: "page_observation", Content: "valid", SourceToolCallID: first.ID, AssetID: &home},
		{ObsType: "page_observation", Content: "invalid", SourceToolCallID: bad, AssetID: &home},
	})
	if !errors.Is(err, service.ErrInvalidObservationSource) {
		t.Fatalf("expected invalid source, got %v", err)
	}
	stored, _ = env.runService.ListObservationsByRun(ctx, run.ID)
	if len(stored) != 2 {
		t.Fatalf("invalid batch partially committed: %d", len(stored))
	}
	_, err = env.runService.CreateObservationsBatchWithSession(ctx, run.ID, uuid.New(), att.ID, []service.ObservationInput{
		{ObsType: "page_observation", Content: "wrong session", SourceToolCallID: first.ID, AssetID: &home},
	})
	if !errors.Is(err, service.ErrInvalidObservationSource) {
		t.Fatalf("expected session mismatch to be rejected, got %v", err)
	}
	stored, _ = env.runService.ListObservationsByRun(ctx, run.ID)
	if len(stored) != 2 {
		t.Fatalf("session mismatch changed observation count: %d", len(stored))
	}
	otherSession, err := env.runService.CreateSession(ctx, run.ID, "worker", nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	foreign, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &otherSession.ID, nil, "foreign", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err = env.runService.UpdateAgentToolCall(ctx, foreign.ID, "success", `{}`, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	wrongAsset := uuid.New()
	for _, invalid := range []struct {
		name                 string
		run, attempt, source uuid.UUID
		asset                *uuid.UUID
	}{
		{"source_session", run.ID, att.ID, foreign.ID, &home},
		{"wrong_attempt", run.ID, uuid.New(), first.ID, &home},
		{"wrong_run", uuid.New(), att.ID, first.ID, &home},
		{"asset_mismatch", run.ID, att.ID, first.ID, &wrongAsset},
	} {
		t.Run(invalid.name, func(t *testing.T) {
			var eventsBefore, eventsAfter int
			if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id=$1 AND type='observation'`, run.ID).Scan(&eventsBefore); err != nil {
				t.Fatal(err)
			}
			_, err := env.runService.CreateObservationsBatchWithSession(ctx, invalid.run, sess.ID, invalid.attempt, []service.ObservationInput{
				{ObsType: "page_observation", Content: "valid first", SourceToolCallID: first.ID, AssetID: &home},
				{ObsType: "separate_conclusion", Content: "invalid second", SourceToolCallID: invalid.source, AssetID: invalid.asset},
			})
			if err == nil {
				t.Fatal("invalid batch accepted")
			}
			stored, err := env.runService.ListObservationsByRun(ctx, run.ID)
			if err != nil || len(stored) != 2 {
				t.Fatalf("partial observation commit: %d %v", len(stored), err)
			}
			if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id=$1 AND type='observation'`, run.ID).Scan(&eventsAfter); err != nil {
				t.Fatal(err)
			}
			if eventsBefore != eventsAfter {
				t.Fatal("invalid batch partially emitted events")
			}
		})
	}
	tooMany := make([]service.ObservationInput, service.MaxObservationBatch+1)
	if _, err := env.runService.CreateObservationsBatchWithAssets(ctx, run.ID, att.ID, tooMany); err == nil {
		t.Fatal("oversized batch accepted")
	}
	if _, err := env.runService.CreateObservationsBatchWithAssets(ctx, run.ID, att.ID, nil); err == nil {
		t.Fatal("empty batch accepted")
	}
	// This fails during insertion after the first row/event have been written,
	// verifying transactional rollback as well as upfront source validation.
	_, err = env.runService.CreateObservationsBatchWithAssets(ctx, run.ID, att.ID, []service.ObservationInput{
		{ObsType: "page_observation", Content: "valid", SourceToolCallID: first.ID},
		{ObsType: "page_observation", Content: "invalid JSON data", Data: json.RawMessage(`{`), SourceToolCallID: second.ID},
	})
	if err == nil {
		t.Fatal("invalid data insertion accepted")
	}
	stored, _ = env.runService.ListObservationsByRun(ctx, run.ID)
	if len(stored) != 2 {
		t.Fatalf("insert error partially committed: %d", len(stored))
	}
	var events int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM run_events WHERE run_id=$1 AND type='observation'`, run.ID).Scan(&events); err != nil || events != 2 {
		t.Fatalf("insert error partially emitted events: %d %v", events, err)
	}
}

// TestSession_DependencyCycleRejection verifies that SubmitPlan detects cycles, self-dependencies,
// and key conflicts.
func TestSession_DependencyCycleRejection(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Dep Task", Objective: "Dep test"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// 1. Self dependency rejected
	_, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "item_self",
			Title:        "Self",
			Objective:    "Self dep",
			Dependencies: []string{"item_self"},
		},
	})
	if err == nil {
		t.Fatal("expected error on self dependency, got nil")
	}

	// 2. Direct cycle (A -> B -> A) rejected
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "item_a",
			Title:        "A",
			Objective:    "A dep",
			Dependencies: []string{"item_b"},
		},
		{
			Key:          "item_b",
			Title:        "B",
			Objective:    "B dep",
			Dependencies: []string{"item_a"},
		},
	})
	if !errors.Is(err, service.ErrDependencyCycle) {
		t.Fatalf("expected ErrDependencyCycle, got %v", err)
	}

	// 3. Valid DAG succeeds
	validPlan, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "step_1",
			Title:     "Step 1",
			Objective: "Initial step",
		},
		{
			Key:          "step_2",
			Title:        "Step 2",
			Objective:    "Second step",
			Dependencies: []string{"step_1"},
		},
	})
	if err != nil {
		t.Fatalf("expected valid plan to succeed, got: %v", err)
	}
	if len(validPlan) != 2 {
		t.Fatalf("expected 2 work items created, got %d", len(validPlan))
	}

	// 4. Same key conflict with different objective is rejected
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "step_1",
			Title:     "Step 1",
			Objective: "DIFFERENT objective for existing key",
		},
	})
	if !errors.Is(err, service.ErrWorkItemConflict) {
		t.Fatalf("expected ErrWorkItemConflict, got %v", err)
	}
}

// TestSession_RestartInterruptedRecovery verifies that RecoverActiveRuns
// terminates active sessions, attempts, and pending work items with consistent status.
func TestSession_RestartInterruptedRecovery(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Restart Task", Objective: "Restart test"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)

	// Transition run to running
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// Create WorkItem, Attempt, Session
	_, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "work_1", Title: "Work 1", Objective: "Test recovery"},
	})
	if err != nil {
		t.Fatalf("submit plan failed: %v", err)
	}

	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wi == nil {
		t.Fatalf("claim work item failed: %v", err)
	}

	// Simulate sudden server restart
	_, err = env.runService.RecoverActiveRuns(ctx)
	if err != nil {
		t.Fatalf("RecoverActiveRuns failed: %v", err)
	}

	// Verify Run is interrupted
	updatedRun, _ := env.runService.Get(ctx, run.ID)
	if updatedRun.Status != model.RunStatusInterrupted {
		t.Errorf("expected run status interrupted, got %s", updatedRun.Status)
	}

	// Verify Session is interrupted
	updatedSess, _ := env.runService.GetSession(ctx, sess.ID)
	if updatedSess.Status != "interrupted" {
		t.Errorf("expected session status interrupted, got %s", updatedSess.Status)
	}

	// Verify Attempt is interrupted
	updatedAtt, _ := env.runService.GetAttempt(ctx, att.ID)
	if updatedAtt.Status != "interrupted" {
		t.Errorf("expected attempt status interrupted, got %s", updatedAtt.Status)
	}

	// Verify WorkItem is failed
	updatedWi, _ := env.runService.GetWorkItem(ctx, wi.ID)
	if updatedWi.Status != model.WorkItemStatusFailed {
		t.Errorf("expected work item status failed, got %s", updatedWi.Status)
	}
}

// TestSession_PersistenceFailureStopsExecution verifies that when database operations fail,
// SessionRunner aborts and returns an error without calling the model further.
func TestSession_PersistenceFailureStopsExecution(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()
	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Fail Task", Objective: "Fail test"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)

	// Create a session with a non-existent run ID to trigger a foreign key persistence failure
	fakeRunID := uuid.New()
	fakeSessID := uuid.New()
	mockP := provider.NewMockProvider("normal")
	budget := model.DefaultBudgetConfig()

	sr := agent.NewSessionRunner(fakeSessID, fakeRunID, "worker", env.runService, mockP, nil, budget)
	res, err := sr.Run(ctx, []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "Hello"},
	})

	if err == nil {
		t.Fatal("expected persistence failure error, got nil")
	}
	if !errors.Is(err, agent.ErrPersistenceFailure) {
		t.Errorf("expected ErrPersistenceFailure, got: %v", err)
	}
	if res != nil {
		t.Errorf("expected nil result on persistence error, got: %+v", res)
	}
	_ = run
}
