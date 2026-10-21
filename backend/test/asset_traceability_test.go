package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// TestAsset_IdentityAcrossRuns verifies that running the same or new tasks with the same endpoints
// reuses the exact same Asset records in the database, while creating distinct RunAsset, WorkItem, and Observation records.
func TestAsset_IdentityAcrossRuns(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	// Task with 3 sample endpoints
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Asset Identity Task",
		Objective: "Verify asset identity persistence across runs",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home", "sample_login", "sample_health"]}`),
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "orchestrator"
	provType := "mock"
	scenario := string(provider.ScenarioTraceability)
	runReq := model.CreateRunRequest{
		Executor:     &executor,
		Provider:     &provType,
		MockScenario: &scenario,
		Budget: &model.BudgetConfig{
			MaxModelCalls:      30,
			MaxToolCalls:       30,
			MaxParallelWorkers: 2,
			RunTimeoutSeconds:  15,
		},
	}

	// First Run
	run1, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run 1: %v", err)
	}

	// Verify 3 assets created
	assets1, err := env.runService.ListAssets(ctx)
	if err != nil {
		t.Fatalf("failed to list assets after run 1: %v", err)
	}
	if len(assets1) != 3 {
		t.Fatalf("expected 3 assets after run 1, got %d", len(assets1))
	}

	assetMap1 := make(map[string]uuid.UUID)
	for _, a := range assets1 {
		assetMap1[a.ExternalKey] = a.ID
	}

	// Verify run_assets for Run 1
	runAssets1, err := env.runService.ListAssetsByRun(ctx, run1.ID)
	if err != nil {
		t.Fatalf("failed to list assets for run 1: %v", err)
	}
	if len(runAssets1) != 3 {
		t.Fatalf("expected 3 run_assets for run 1, got %d", len(runAssets1))
	}

	// Complete run 1 manually
	_, _ = env.runService.TransitionState(ctx, run1.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	reason := "done"
	_, _ = env.runService.TransitionState(ctx, run1.ID, model.RunStatusRunning, model.RunStatusCompleted, &reason)

	// Second Run for the same task
	run2, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run 2: %v", err)
	}

	// Total assets in DB must STILL be exactly 3!
	assets2, err := env.runService.ListAssets(ctx)
	if err != nil {
		t.Fatalf("failed to list assets after run 2: %v", err)
	}
	if len(assets2) != 3 {
		t.Fatalf("expected exactly 3 assets after run 2, got %d", len(assets2))
	}

	// Verify asset IDs match run 1 exactly (identity preserved!)
	for _, a := range assets2 {
		origID, exists := assetMap1[a.ExternalKey]
		if !exists {
			t.Fatalf("unexpected asset key %s in run 2", a.ExternalKey)
		}
		if a.ID != origID {
			t.Fatalf("asset %s ID changed across runs: was %s, now %s", a.ExternalKey, origID, a.ID)
		}
	}

	// Verify run 2 has its own run_assets links
	runAssets2, err := env.runService.ListAssetsByRun(ctx, run2.ID)
	if err != nil {
		t.Fatalf("failed to list assets for run 2: %v", err)
	}
	if len(runAssets2) != 3 {
		t.Fatalf("expected 3 run_assets for run 2, got %d", len(runAssets2))
	}
}

// TestAsset_ConcurrentImportIdempotency tests concurrent creation of runs targeting the same assets.
// Confirms zero unique constraint violations and idempotent asset creation.
func TestAsset_ConcurrentImportIdempotency(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	const numTasks = 10
	taskIDs := make([]uuid.UUID, numTasks)
	for i := 0; i < numTasks; i++ {
		task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
			Name:      fmt.Sprintf("Concurrent Task %d", i),
			Objective: "Test concurrency",
			Scope:     json.RawMessage(`{"endpoints": ["sample_home", "sample_login", "sample_health"]}`),
		})
		if err != nil {
			t.Fatalf("failed to create task %d: %v", i, err)
		}
		taskIDs[i] = task.ID
	}

	var wg sync.WaitGroup
	errCh := make(chan error, numTasks)

	executor := "fake"
	for i := 0; i < numTasks; i++ {
		wg.Add(1)
		go func(tid uuid.UUID) {
			defer wg.Done()
			_, _, err := env.runService.Create(ctx, tid, model.CreateRunRequest{Executor: &executor}, nil)
			if err != nil {
				errCh <- err
			}
		}(taskIDs[i])
	}

	wg.Wait()
	close(errCh)

	for err := range errCh {
		t.Fatalf("concurrent run creation failed: %v", err)
	}

	// Verify database has exactly 3 assets despite 10 concurrent run creations
	assets, err := env.runService.ListAssets(ctx)
	if err != nil {
		t.Fatalf("failed to list assets: %v", err)
	}
	if len(assets) != 3 {
		t.Fatalf("expected exactly 3 unique assets, got %d", len(assets))
	}
}

// TestObservation_StrictValidation verifies strict validation rules for observation creation:
// - Tool call must be read_sample
// - Tool call must be success
// - sample_id must match asset key
// - asset must be in run_assets
// - asset must be in work_item_assets
func TestObservation_StrictValidation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	// Task with scope home and login only
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Observation Validation Task",
		Objective: "Test observation checks",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home", "sample_login"]}`),
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "fake"
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Executor: &executor}, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Transition to running so plan submission is allowed
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatalf("failed to transition run to running: %v", err)
	}

	// Get assets
	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil || len(runAssets) != 2 {
		t.Fatalf("expected 2 run assets, got %d (err: %v)", len(runAssets), err)
	}
	var homeAsset, loginAsset model.Asset
	for _, a := range runAssets {
		if a.ExternalKey == "sample_home" {
			homeAsset = a
		} else if a.ExternalKey == "sample_login" {
			loginAsset = a
		}
	}

	// Create Planner Round 1 and Work Item targeting only homeAsset
	roundNum := 1
	pr, _ := env.runService.CreatePlannerRound(ctx, run.ID, roundNum, "initial", nil)
	_, err = env.runService.SubmitPlanWithRound(ctx, run.ID, &pr.ID, []service.PlanItemSpec{
		{
			Key:       "check_home",
			Title:     "Check Home",
			Objective: "Inspect home",
			WorkType:  "analyze_sample",
			InputData: map[string]interface{}{"sample_id": "sample_home"},
			AssetIDs:  []uuid.UUID{homeAsset.ID},
		},
	})
	if err != nil {
		t.Fatalf("failed to submit plan: %v", err)
	}

	// Claim work item
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wi == nil || att == nil || sess == nil {
		t.Fatalf("failed to claim work item: %v (wi: %v, att: %v, sess: %v)", err, wi, att, sess)
	}

	// 1. Tool call is list_samples (not read_sample) -> rejected
	tcBadTool, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_list", "list_samples", json.RawMessage(`{}`),
	)
	if err != nil {
		t.Fatalf("failed to create tool call: %v", err)
	}
	_ = env.runService.UpdateAgentToolCall(ctx, tcBadTool.ID, "success", "{}", nil, nil, nil)

	_, err = env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "Test", json.RawMessage(`{"status":200}`), tcBadTool.ID, &homeAsset.ID,
	)
	if err == nil {
		t.Fatal("expected rejection when tool call is list_samples, but succeeded")
	}

	// 2. Tool call is read_sample but failed -> rejected
	tcFailed, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_read_fail", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`),
	)
	if err != nil {
		t.Fatalf("failed to create tool call: %v", err)
	}
	errMsg := "connection refused"
	_ = env.runService.UpdateAgentToolCall(ctx, tcFailed.ID, "failed", "", &errMsg, nil, nil)

	_, err = env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "Test", json.RawMessage(`{"status":200}`), tcFailed.ID, &homeAsset.ID,
	)
	if err == nil {
		t.Fatal("expected rejection when tool call failed, but succeeded")
	}

	// 3. Valid tool call for sample_home
	tcHome, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_read_home", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`),
	)
	if err != nil {
		t.Fatalf("failed to create tool call: %v", err)
	}
	_ = env.runService.UpdateAgentToolCall(ctx, tcHome.ID, "success", `{"status_code":200}`, nil, nil, nil)

	// 3a. Mismatched sample_id: Tool called sample_home, but observation specifies loginAsset -> rejected
	_, err = env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "Test", json.RawMessage(`{"status":200}`), tcHome.ID, &loginAsset.ID,
	)
	if !errors.Is(err, service.ErrAssetSampleMismatch) {
		t.Fatalf("expected ErrAssetSampleMismatch, got %v", err)
	}

	// 3b. Work item asset mismatch: Even if tool called sample_login, check_home only links homeAsset
	tcLogin, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_read_login", "read_sample", json.RawMessage(`{"sample_id":"sample_login"}`),
	)
	if err != nil {
		t.Fatalf("failed to create tool call: %v", err)
	}
	_ = env.runService.UpdateAgentToolCall(ctx, tcLogin.ID, "success", `{"status_code":200}`, nil, nil, nil)

	_, err = env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "Test", json.RawMessage(`{"status":200}`), tcLogin.ID, &loginAsset.ID,
	)
	if !errors.Is(err, service.ErrAssetOutOfScope) {
		t.Fatalf("expected ErrAssetOutOfScope (not in work item assets), got %v", err)
	}

	// 3c. Success case: Tool call matches sample_home, asset is homeAsset -> succeeds!
	obs, err := env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "Home endpoint 200 OK", json.RawMessage(`{"status":200}`), tcHome.ID, &homeAsset.ID,
	)
	if err != nil {
		t.Fatalf("expected success, got error: %v", err)
	}
	if obs.AssetID == nil || *obs.AssetID != homeAsset.ID {
		t.Fatalf("expected observation to bind asset ID %s, got %v", homeAsset.ID, obs.AssetID)
	}
}

// TestObservation_SessionCollisionIsolation tests that two sessions sharing identical model call_ids
// do NOT resolve the other session's tool call when recording observations.
func TestObservation_SessionCollisionIsolation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Session Collision Task",
		Objective: "Test call_id resolution isolation",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "fake"
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Executor: &executor}, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// Create 2 work items
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "item_a", Title: "A", Objective: "A", WorkType: "analyze_sample", AssetIDs: []uuid.UUID{homeAsset.ID}},
		{Key: "item_b", Title: "B", Objective: "B", WorkType: "analyze_sample", AssetIDs: []uuid.UUID{homeAsset.ID}},
	})
	if err != nil {
		t.Fatalf("failed to submit plan: %v", err)
	}

	// Claim Session A
	wiA, attA, sessA, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wiA == nil {
		t.Fatalf("failed to claim item A: %v", err)
	}

	// Claim Session B
	wiB, attB, sessB, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil || wiB == nil {
		t.Fatalf("failed to claim item B: %v", err)
	}

	// Tool call with call_id = "call_colliding_id" in Session A
	tcA, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sessA.ID, nil, "call_colliding_id", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`),
	)
	if err != nil {
		t.Fatalf("failed create tool call A: %v", err)
	}
	_ = env.runService.UpdateAgentToolCall(ctx, tcA.ID, "success", `{"body":"Content A"}`, nil, nil, nil)

	// Tool call with IDENTICAL call_id = "call_colliding_id" in Session B!
	tcB, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sessB.ID, nil, "call_colliding_id", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`),
	)
	if err != nil {
		t.Fatalf("failed create tool call B: %v", err)
	}
	_ = env.runService.UpdateAgentToolCall(ctx, tcB.ID, "success", `{"body":"Content B"}`, nil, nil, nil)

	// Record observation using RecordObservationToolWithSession for Session A
	toolA := tools.NewRecordObservationToolWithSession(run.ID, attA.ID, sessA.ID, env.runService)
	toolB := tools.NewRecordObservationToolWithSession(run.ID, attB.ID, sessB.ID, env.runService)

	argsA := fmt.Sprintf(`{
		"obs_type": "http_status",
		"content": "Obs from A",
		"source_tool_call_id": "call_colliding_id",
		"asset_id": "%s"
	}`, homeAsset.ID)

	resA, err := toolA.Execute(ctx, []byte(argsA))
	if err != nil {
		t.Fatalf("failed tool A execute: %v", err)
	}

	argsB := fmt.Sprintf(`{
		"obs_type": "http_status",
		"content": "Obs from B",
		"source_tool_call_id": "call_colliding_id",
		"asset_id": "%s"
	}`, homeAsset.ID)

	resB, err := toolB.Execute(ctx, []byte(argsB))
	if err != nil {
		t.Fatalf("failed tool B execute: %v", err)
	}

	// Parse observation IDs
	var outA, outB struct {
		ObservationID uuid.UUID `json:"observation_id"`
	}
	json.Unmarshal([]byte(resA), &outA)
	json.Unmarshal([]byte(resB), &outB)

	// Query observation records
	obsList, err := env.runService.ListObservationsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list observations: %v", err)
	}
	for _, obs := range obsList {
		if obs.ID == outA.ObservationID {
			if obs.SourceToolCallID != tcA.ID {
				t.Fatalf("Observation A linked to wrong tool call: expected %s, got %s", tcA.ID, obs.SourceToolCallID)
			}
		}
		if obs.ID == outB.ObservationID {
			if obs.SourceToolCallID != tcB.ID {
				t.Fatalf("Observation B linked to wrong tool call: expected %s, got %s", tcB.ID, obs.SourceToolCallID)
			}
		}
	}
}

// TestAsset_ScopeEnforcement tests that out-of-scope sample endpoints are blocked by ReadSampleToolWithScope
// and plan submission.
func TestAsset_ScopeEnforcement(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	// Scope only contains sample_home
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Scope Enforcement Task",
		Objective: "Test out of scope blocking",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "fake"
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Executor: &executor}, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	// Scope checks
	inScopeHome, _ := env.runService.IsSampleInRunScope(ctx, run.ID, "sample_home")
	if !inScopeHome {
		t.Fatal("expected sample_home to be in scope")
	}
	inScopeLogin, _ := env.runService.IsSampleInRunScope(ctx, run.ID, "sample_login")
	if inScopeLogin {
		t.Fatal("expected sample_login NOT to be in scope")
	}

	// Tool scope check
	tool := tools.NewReadSampleToolWithScope(run.ID, func(ctx context.Context, sampleID string) (bool, error) {
		return env.runService.IsSampleInRunScope(ctx, run.ID, sampleID)
	})

	// Reading in-scope sample_home succeeds
	resHome, err := tool.Execute(ctx, []byte(`{"sample_id":"sample_home"}`))
	if err != nil || resHome == "" {
		t.Fatalf("expected sample_home read to succeed, got %v", err)
	}

	// Reading out-of-scope sample_login fails
	_, err = tool.Execute(ctx, []byte(`{"sample_id":"sample_login"}`))
	if err == nil {
		t.Fatal("expected error reading sample_login out of scope, but got nil")
	}

	// Plan submission with fake asset ID not in run_assets fails
	fakeAssetID := uuid.New()
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:      "bad_item",
			Title:    "Bad",
			WorkType: "analyze_sample",
			AssetIDs: []uuid.UUID{fakeAssetID},
		},
	})
	if !errors.Is(err, service.ErrAssetOutOfScope) {
		t.Fatalf("expected ErrAssetOutOfScope, got %v", err)
	}
}

// TestAsset_FullScenarioTraceability_Mock tests the complete Section 9 demonstration workflow:
// - Round 1: Planner creates 4 initial items (3 samples + 1 aggregate summary) with asset_ids.
// - Dual workers execute items; observations recorded for sample_home, sample_login, sample_health with asset_id linked.
// - Round 2: Planner references login observation, creates follow-up recheck_login_title with source_observation_ids.
// - Worker executes recheck_login_title and records page_title observation.
// - Round 3: Planner reviews all observations and suggests completion.
// - DB verification: 5 work items, 3 planner rounds, source observations linked, assets linked.
func TestAsset_FullScenarioTraceability_Mock(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.StopAll()

	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Phase 4 Traceability Acceptance Task",
		Objective: "Audit sample services, record observations, and recheck login title",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home", "sample_login", "sample_health"]}`),
	})
	if err != nil {
		t.Fatalf("failed to create task: %v", err)
	}

	executor := "orchestrator"
	provType := "mock"
	scenario := string(provider.ScenarioTraceability)
	runReq := model.CreateRunRequest{
		Executor:     &executor,
		Provider:     &provType,
		MockScenario: &scenario,
		Budget: &model.BudgetConfig{
			MaxModelCalls:      40,
			MaxToolCalls:       40,
			MaxParallelWorkers: 2,
			RunTimeoutSeconds:  20,
		},
	}

	run, _, err := env.runService.Create(ctx, task.ID, runReq, nil)
	if err != nil {
		t.Fatalf("failed to create run: %v", err)
	}

	// Start execution via Engine
	p := provider.NewMockProvider(provider.ScenarioTraceability)

	budget := *runReq.Budget
	eng := orchestration.NewEngine(run.ID, env.runService, p, budget)

	engineDone := make(chan struct{})
	go func() {
		eng.Run(context.Background())
		close(engineDone)
	}()

	select {
	case <-engineDone:
	case <-time.After(15 * time.Second):
		t.Fatal("engine run timed out after 15 seconds")
	}

	// 1. Verify Run reached terminal completed state
	finalRun, err := env.runService.Get(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to get final run: %v", err)
	}
	if finalRun.Status != model.RunStatusCompleted {
		t.Fatalf("expected run status 'completed', got '%s' (reason: %v)", finalRun.Status, finalRun.FinishReason)
	}

	// 2. Event-driven planning may add bounded review rounds while workers run.
	// Round 1: Initial plan
	// Round 2: Follow-up recheck
	// Round 3: Suggest completion
	rounds, err := env.runService.ListPlannerRounds(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list planner rounds: %v", err)
	}
	if len(rounds) < 3 || len(rounds) > model.DefaultBudgetConfig().MaxPlannerRounds {
		t.Fatalf("expected the three necessary planning decisions within the bounded event loop, got %d", len(rounds))
	}
	for _, pr := range rounds {
		if pr.Status != "completed" {
			t.Fatalf("planner round %d status is %s, expected completed", pr.Round, pr.Status)
		}
	}
	prRound2ID := rounds[1].ID

	// 3. Verify Work Items: Exactly 5 work items!
	// 4 from Round 1 + 1 from Round 2 (recheck_login_title)
	items, err := env.runService.ListWorkItemDetailsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list work item details: %v", err)
	}
	if len(items) != 5 {
		t.Fatalf("expected 5 work items, got %d", len(items))
	}

	var recheckItem *model.WorkItemDetail
	for i := range items {
		if items[i].Key == "recheck_login_title" {
			recheckItem = &items[i]
		}
		if items[i].Status != model.WorkItemStatusCompleted {
			t.Fatalf("work item %s status is %s, expected completed", items[i].Key, items[i].Status)
		}
	}

	if recheckItem == nil {
		t.Fatal("recheck_login_title work item not found")
	}

	// 4. Verify recheck_login_title traceability:
	// - Created in Round 2
	// - Has asset_ids (login asset)
	// - Has source_observation_ids (from round 1 login observation)
	if recheckItem.CreatedByPlannerRoundID == nil || *recheckItem.CreatedByPlannerRoundID != prRound2ID {
		t.Fatalf("expected recheck_login_title created_by_planner_round_id = %s, got %v", prRound2ID, recheckItem.CreatedByPlannerRoundID)
	}
	if len(recheckItem.TargetAssets) == 0 {
		t.Fatal("expected recheck_login_title to have linked target_assets")
	}
	if len(recheckItem.SourceObservations) == 0 {
		t.Fatal("expected recheck_login_title to have linked source_observations")
	}

	// 5. Verify Observations:
	// Round 1 items produced observations for sample_home, sample_login, sample_health.
	// Round 2 item produced observation for page_title.
	obsList, err := env.runService.ListObservationsByRun(ctx, run.ID)
	if err != nil {
		t.Fatalf("failed to list observations: %v", err)
	}
	if len(obsList) < 4 {
		t.Fatalf("expected at least 4 observations, got %d", len(obsList))
	}

	var foundPageTitleObs bool
	for _, obs := range obsList {
		if obs.ObsType == "page_title" {
			foundPageTitleObs = true
		}
		// Every endpoint inspection observation must have a valid non-nil asset_id!
		if obs.AssetID == nil {
			t.Fatalf("observation %s (type: %s) has nil asset_id", obs.ID, obs.ObsType)
		}
	}
	if !foundPageTitleObs {
		t.Fatal("expected observation of type 'page_title' from recheck_login_title")
	}

	// 6. Verify Asset Details With History API:
	// Find login asset ID and fetch its full history
	loginAssetID := recheckItem.TargetAssets[0].ID
	history, err := env.runService.GetAssetDetailsWithHistory(ctx, loginAssetID)
	if err != nil {
		t.Fatalf("failed to get asset details with history: %v", err)
	}
	if history.Asset.ExternalKey != "sample_login" {
		t.Fatalf("expected asset key 'sample_login', got '%s'", history.Asset.ExternalKey)
	}
	if len(history.RunHistory) == 0 {
		t.Fatal("expected at least 1 run history for asset")
	}
	runHistory := history.RunHistory[0]
	if len(runHistory.WorkItems) < 2 {
		t.Fatalf("expected at least 2 work items for login asset (analyze_login + recheck_login_title), got %d", len(runHistory.WorkItems))
	}
	if len(runHistory.Observations) < 2 {
		t.Fatalf("expected at least 2 observations for login asset (status + page_title), got %d", len(runHistory.Observations))
	}
	for _, obs := range runHistory.Observations {
		if obs.ToolCall == nil {
			t.Fatalf("expected observation %s to have linked tool call", obs.ID)
		}
	}

	t.Logf("Full traceability acceptance test passed with %d planner rounds, %d work items, and %d observations!",
		len(rounds), len(items), len(obsList))
}
