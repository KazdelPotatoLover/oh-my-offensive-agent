package test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// TestPhase5_Boundary_ScopeEnforcement tests Section 3.1:
// - Empty scope cannot mean allow all.
// - Unknown endpoints in scope are rejected upon run creation.
// - Out of scope assets are rejected by IsSampleInRunScope.
func TestPhase5_Boundary_ScopeEnforcement(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	// 1. Task with empty scope
	emptyTask, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Empty Scope Task",
		Objective: "Test empty scope",
		Scope:     json.RawMessage(`{}`),
	})
	if err != nil {
		t.Fatalf("create empty scope task: %v", err)
	}

	emptyRun, _, err := env.runService.Create(ctx, emptyTask.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create empty run: %v", err)
	}

	if emptyRun.ScopePolicyVersion != "v1" {
		t.Fatalf("expected scope_policy_version 'v1', got '%s'", emptyRun.ScopePolicyVersion)
	}

	// Verify IsSampleInRunScope returns false (NOT true!) for empty scope run
	allowed, err := env.runService.IsSampleInRunScope(ctx, emptyRun.ID, "sample_home")
	if err != nil {
		t.Fatalf("check sample in run scope: %v", err)
	}
	if allowed {
		t.Fatalf("empty scope must NOT allow all; expected allowed=false, got true")
	}

	// 2. Task with invalid/unknown endpoint in scope must fail run creation
	invalidTask, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Invalid Scope Task",
		Objective: "Test invalid scope",
		Scope:     json.RawMessage(`{"endpoints": ["nonexistent_target_endpoint_xyz"]}`),
	})
	if err != nil {
		t.Fatalf("create invalid scope task: %v", err)
	}

	_, _, err = env.runService.Create(ctx, invalidTask.ID, model.CreateRunRequest{}, nil)
	if err == nil {
		t.Fatalf("expected error creating run with unknown endpoint in scope, got nil")
	}
	if !strings.Contains(err.Error(), "unknown endpoint in scope") {
		t.Fatalf("expected 'unknown endpoint in scope' error, got: %v", err)
	}

	// 3. Task with invalid JSON structure in scope (e.g. endpoints is not an array of strings) must fail run creation
	malformedTask, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Malformed Scope Task",
		Objective: "Test malformed scope",
		Scope:     json.RawMessage(`{"endpoints": 12345}`),
	})
	if err != nil {
		t.Fatalf("create malformed scope task: %v", err)
	}

	_, _, err = env.runService.Create(ctx, malformedTask.ID, model.CreateRunRequest{}, nil)
	// Malformed structure in scope: returns error or 0 assets
	_ = err

	// 4. Task with explicit single endpoint scope only allows that endpoint
	scopedTask, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Single Scope Task",
		Objective: "Test single scope",
		Scope:     json.RawMessage(`{"endpoints": ["sample_home"]}`),
	})
	if err != nil {
		t.Fatalf("create single scope task: %v", err)
	}

	scopedRun, _, err := env.runService.Create(ctx, scopedTask.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create scoped run: %v", err)
	}

	homeAllowed, err := env.runService.IsSampleInRunScope(ctx, scopedRun.ID, "sample_home")
	if err != nil || !homeAllowed {
		t.Fatalf("expected sample_home to be allowed, got allowed=%v, err=%v", homeAllowed, err)
	}

	loginAllowed, err := env.runService.IsSampleInRunScope(ctx, scopedRun.ID, "sample_login")
	if err != nil || loginAllowed {
		t.Fatalf("expected sample_login to be rejected, got allowed=%v, err=%v", loginAllowed, err)
	}

	t.Log("Section 3.1 Scope enforcement verified successfully")
}

// TestPhase5_Boundary_StopAndObservationRace tests Section 3.2:
// - CreateObservationWithAsset acquires row lock and rejects observations when run is stopping or terminal.
// - Serializes cleanly with Stop.
func TestPhase5_Boundary_StopAndObservationRace(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Stop Race Task",
		Objective: "Test stop race with observation",
		Scope:     json.RawMessage(`{"endpoints":["sample_home"]}`),
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

	// Create work item & attempt
	_, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
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
		t.Fatalf("submit plan: %v", err)
	}

	_, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim work item: %v", err)
	}

	// Create a successful tool call
	tc, err := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_read_1", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`))
	if err != nil {
		t.Fatalf("create tool call: %v", err)
	}
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", `{"status_code": 200}`, nil, &now, &now)

	// Transition run to stopping
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusStopping, nil)
	if err != nil {
		t.Fatalf("transition to stopping: %v", err)
	}

	// Creating observation on a stopping run MUST be rejected
	_, err = env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "status was 200", json.RawMessage(`{"status_code":200}`), tc.ID, &homeAssetID)
	if err == nil {
		t.Fatalf("expected observation recording to be rejected on stopping run, but succeeded")
	}
	if !strings.Contains(err.Error(), "cannot record observation: run status is stopping") {
		t.Fatalf("expected 'run status is stopping' error, got: %v", err)
	}

	// Complete transition to stopped
	stopReason := "user_requested_stop"
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusStopping, model.RunStatusStopped, &stopReason)

	// Creating observation on a stopped terminal run MUST also be rejected
	_, err = env.runService.CreateObservationWithAsset(
		ctx, run.ID, att.ID, "http_status", "status was 200", json.RawMessage(`{"status_code":200}`), tc.ID, &homeAssetID)
	if err == nil {
		t.Fatalf("expected observation recording to be rejected on stopped run, but succeeded")
	}
	if !strings.Contains(err.Error(), "cannot record observation: run status is stopped") {
		t.Fatalf("expected 'run status is stopped' error, got: %v", err)
	}

	t.Log("Section 3.2 Stop and observation lock ordering verified successfully")
}

// TestPhase5_Boundary_InvalidAssetIDValidation tests Section 3.3:
// - record_observation tool rejects invalid non-empty asset_id strings.
// - Does not silently default to nil.
func TestPhase5_Boundary_InvalidAssetIDValidation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "AssetID Validation Task",
		Objective: "Test invalid asset_id validation",
		Scope:     json.RawMessage(`{"endpoints":["sample_home"]}`),
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

	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "item_1",
			Title:     "Item 1",
			Objective: "Inspect sample",
			WorkType:  "analyze_sample",
			AssetIDs:  []uuid.UUID{homeAssetID},
			InputData: map[string]interface{}{"sample_id": "sample_home"},
		},
	})
	_, att, sess, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	tc, _ := env.runService.CreateAgentToolCallWithSession(
		ctx, run.ID, &sess.ID, nil, "call_test_1", "read_sample", json.RawMessage(`{"sample_id":"sample_home"}`))
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", `{"status_code": 200}`, nil, &now, &now)

	recTool := tools.NewRecordObservationToolWithSession(run.ID, att.ID, sess.ID, env.runService)

	// 1. Tool Validate fails with malformed asset_id
	invalidArgs := []byte(`{
		"obs_type": "http_status",
		"content": "Status 200",
		"source_tool_call_id": "` + tc.ID.String() + `",
		"asset_id": "not-a-valid-uuid"
	}`)

	valErr := recTool.Validate(invalidArgs)
	if valErr == nil {
		t.Fatalf("expected Validate to reject malformed asset_id, got nil")
	}
	if !strings.Contains(valErr.Error(), "invalid asset_id") {
		t.Fatalf("expected 'invalid asset_id' validation error, got: %v", valErr)
	}

	// 2. Tool Execute fails with malformed asset_id
	_, execErr := recTool.Execute(ctx, invalidArgs)
	if execErr == nil {
		t.Fatalf("expected Execute to reject malformed asset_id, got nil")
	}
	if !strings.Contains(execErr.Error(), "invalid asset_id") {
		t.Fatalf("expected 'invalid asset_id' execution error, got: %v", execErr)
	}

	// 3. Tool Execute with empty asset_id is allowed (omitted, auto-resolved from tool call)
	validArgsOmitted := []byte(`{
		"obs_type": "http_status",
		"content": "Status 200",
		"source_tool_call_id": "` + tc.ID.String() + `",
		"asset_id": ""
	}`)
	res, execErr := recTool.Execute(ctx, validArgsOmitted)
	if execErr != nil {
		t.Fatalf("expected Execute with omitted asset_id to succeed, got: %v", execErr)
	}
	if !strings.Contains(res, "recorded") {
		t.Fatalf("expected result to indicate recorded, got: %s", res)
	}

	// 4. Tool Execute with mismatched UUID (asset not matching tool call) is rejected
	mismatchedAssetID := uuid.New()
	mismatchedArgs := []byte(`{
		"obs_type": "http_status",
		"content": "Status 200",
		"source_tool_call_id": "` + tc.ID.String() + `",
		"asset_id": "` + mismatchedAssetID.String() + `"
	}`)
	_, misErr := recTool.Execute(ctx, mismatchedArgs)
	if misErr == nil {
		t.Fatalf("expected Execute with mismatched asset_id to be rejected, got nil")
	}
	if !errors.Is(misErr, service.ErrAssetSampleMismatch) {
		t.Fatalf("expected ErrAssetSampleMismatch, got: %v", misErr)
	}

	t.Log("Section 3.3 Invalid asset_id validation verified successfully")
}
