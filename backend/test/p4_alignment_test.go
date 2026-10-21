package test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// TestP4_ContractE5_WorkerBSearchesWorkerATrace tests Contract E5:
// Worker A executes steps with raw outputs containing unrecorded clues.
// Worker B uses search_all_worker_traces and get_worker_trace to find
// and extract the clue from Worker A's trace and reuse it!
func TestP4_ContractE5_WorkerBSearchesWorkerATrace(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "E5 Trace Search Task",
		Objective: "Test cross-worker trace search and step drilldown",
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)

	// 1. Create Work Item A and claim it
	planA, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "work_a",
			Title:     "Worker A Exploration",
			Objective: "Probe debug endpoints",
			WorkType:  "intent",
		},
	})
	if err != nil {
		t.Fatalf("submit plan A: %v", err)
	}
	wiA := planA[0]
	_, attA, sessA, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim work item A: %v", err)
	}

	// Worker A executes 3 tool calls. Tool call 2 outputs a hidden internal token!
	now := time.Now()
	clueToken := "SECRET_INTERNAL_DEBUG_TOKEN_7788"
	tc1, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessA.ID, nil, "call_1", "get_endpoint", json.RawMessage(`{"path":"/index"}`))
	_ = env.runService.UpdateAgentToolCall(ctx, tc1.ID, "success", "HTTP 200 OK HTML body homepage", nil, &now, &now)

	tc2, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessA.ID, nil, "call_2", "get_endpoint", json.RawMessage(`{"path":"/debug/vars"}`))
	fullOutput2 := fmt.Sprintf(`{"debug_mode": true, "environment": "staging", "auth": {"token": "%s", "role": "root"}}`, clueToken)
	_ = env.runService.UpdateAgentToolCall(ctx, tc2.ID, "success", fullOutput2, nil, &now, &now)

	tc3, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessA.ID, nil, "call_3", "get_endpoint", json.RawMessage(`{"path":"/health"}`))
	_ = env.runService.UpdateAgentToolCall(ctx, tc3.ID, "success", "HTTP 200 Healthy", nil, &now, &now)

	_ = env.runService.FinishWorkItem(ctx, wiA.ID, attA.ID, sessA.ID, true, json.RawMessage(`{}`), "completed")

	// 2. Create Work Item B and claim it
	planB, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "work_b",
			Title:     "Worker B Exploitation",
			Objective: "Exploit debug interface using found tokens",
			WorkType:  "intent",
		},
	})
	if err != nil {
		t.Fatalf("submit plan B: %v", err)
	}
	wiB := planB[0]
	_, attB, sessB, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim work item B: %v", err)
	}

	// 3. Worker B calls search_all_worker_traces for "SECRET_INTERNAL_DEBUG_TOKEN"
	searchTool := tools.NewSearchAllWorkerTracesTool(run.ID, &wiB.ID, env.runService)
	searchArgs, _ := json.Marshal(map[string]any{"q": "SECRET_INTERNAL_DEBUG_TOKEN"})
	searchOut, err := searchTool.Execute(ctx, searchArgs)
	if err != nil {
		t.Fatalf("search_all_worker_traces failed: %v", err)
	}

	var searchResp struct {
		Total int `json:"total"`
		Hits  []struct {
			StepID     string `json:"step_id"`
			WorkItemID string `json:"work_item_id"`
			ToolName   string `json:"tool_name"`
			Summary    string `json:"summary"`
		} `json:"hits"`
	}
	if err := json.Unmarshal([]byte(searchOut), &searchResp); err != nil {
		t.Fatalf("unmarshal search output: %v", err)
	}
	if searchResp.Total != 1 || len(searchResp.Hits) != 1 {
		t.Fatalf("expected exactly 1 hit for clue token, got %d", searchResp.Total)
	}
	hit := searchResp.Hits[0]
	if hit.StepID != tc2.ID.String() {
		t.Errorf("expected step_id %s, got %s", tc2.ID, hit.StepID)
	}
	if hit.WorkItemID != wiA.ID.String() {
		t.Errorf("expected work_item_id %s, got %s", wiA.ID, hit.WorkItemID)
	}
	// Check that summary was truncated (does not return the whole large JSON)
	if len(hit.Summary) > 105 {
		t.Errorf("expected summary <= 105 chars, got %d", len(hit.Summary))
	}

	// 4. Worker B calls get_worker_trace to inspect Work A's step summaries
	getTraceTool := tools.NewGetWorkerTraceTool(run.ID, env.runService)
	summaryArgs, _ := json.Marshal(map[string]any{"work_item_id": wiA.ID.String()})
	summaryOut, err := getTraceTool.Execute(ctx, summaryArgs)
	if err != nil {
		t.Fatalf("get_worker_trace summary stream failed: %v", err)
	}
	var streamResp struct {
		TotalSteps int `json:"total_steps"`
		Steps      []struct {
			StepID   string `json:"step_id"`
			ToolName string `json:"tool_name"`
			Summary  string `json:"summary"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(summaryOut), &streamResp); err != nil {
		t.Fatalf("unmarshal summary stream: %v", err)
	}
	if streamResp.TotalSteps != 3 {
		t.Fatalf("expected 3 steps in Worker A trace, got %d", streamResp.TotalSteps)
	}

	// 5. Worker B calls get_worker_trace with step_ids=[tc2.ID] to get the FULL content
	drilldownArgs, _ := json.Marshal(map[string]any{
		"work_item_id": wiA.ID.String(),
		"step_ids":     []string{tc2.ID.String()},
	})
	drilldownOut, err := getTraceTool.Execute(ctx, drilldownArgs)
	if err != nil {
		t.Fatalf("get_worker_trace drilldown failed: %v", err)
	}

	var drilldownResp struct {
		Steps []struct {
			StepID    string `json:"step_id"`
			ToolName  string `json:"tool_name"`
			Arguments string `json:"arguments"`
			Output    string `json:"output"`
		} `json:"steps"`
	}
	if err := json.Unmarshal([]byte(drilldownOut), &drilldownResp); err != nil {
		t.Fatalf("unmarshal drilldown output: %v", err)
	}
	if len(drilldownResp.Steps) != 1 {
		t.Fatalf("expected 1 step in drilldown, got %d", len(drilldownResp.Steps))
	}
	stepContent := drilldownResp.Steps[0]
	if !strings.Contains(stepContent.Output, clueToken) {
		t.Errorf("expected full output to contain clue token %s, got: %s", clueToken, stepContent.Output)
	}

	// 6. Worker B successfully records a fact using the clue found in Worker A's trace!
	factB, err := env.runService.RecordFact(
		ctx, run.ID,
		"Retrieved staging root auth token from debug interface",
		fmt.Sprintf("Auth token extracted from previous worker trace: %s", clueToken),
		"observed",
		fmt.Sprintf("token=%s", clueToken),
		&wiB.ID,
		nil,
	)
	if err != nil {
		t.Fatalf("Worker B record fact: %v", err)
	}
	if factB.ID == uuid.Nil {
		t.Errorf("expected valid fact ID")
	}

	_ = env.runService.FinishWorkItem(ctx, wiB.ID, attB.ID, sessB.ID, true, json.RawMessage(`{}`), "completed")

	// 7. Test list_worker_traces
	listTool := tools.NewListWorkerTracesTool(run.ID, env.runService)
	listOut, err := listTool.Execute(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("list_worker_traces failed: %v", err)
	}
	var listResp struct {
		Total  int `json:"total"`
		Traces []struct {
			WorkItemID string `json:"work_item_id"`
			Key        string `json:"key"`
			StepCount  int    `json:"step_count"`
			Status     string `json:"status"`
		} `json:"traces"`
	}
	if err := json.Unmarshal([]byte(listOut), &listResp); err != nil {
		t.Fatalf("unmarshal list traces: %v", err)
	}
	if listResp.Total != 2 {
		t.Fatalf("expected 2 worker traces, got %d", listResp.Total)
	}
}

// TestP4_GetWorkerTrace_StepCapAndOmitted verifies that requesting more than 5 steps
// returns the first 5 and populates omitted_step_ids (ARTEX parity).
func TestP4_GetWorkerTrace_StepCapAndOmitted(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Step Cap Task", Objective: "Test step cap"})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run.ID)

	plan, _ := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "work_many", Title: "Many Steps Work", Objective: "Execute 7 steps", WorkType: "intent"},
	})
	wi := plan[0]
	_, _, sess, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)

	now := time.Now()
	var stepIDs []string
	for i := 1; i <= 7; i++ {
		tc, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sess.ID, nil, fmt.Sprintf("call_%d", i), "get_endpoint", json.RawMessage(`{}`))
		_ = env.runService.UpdateAgentToolCall(ctx, tc.ID, "success", fmt.Sprintf("Step %d output", i), nil, &now, &now)
		stepIDs = append(stepIDs, tc.ID.String())
	}

	getTraceTool := tools.NewGetWorkerTraceTool(run.ID, env.runService)
	args, _ := json.Marshal(map[string]any{
		"work_item_id": wi.ID.String(),
		"step_ids":     stepIDs,
	})
	out, err := getTraceTool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("get_worker_trace failed: %v", err)
	}

	var resp struct {
		ReturnedStepIDs []string `json:"returned_step_ids"`
		OmittedStepIDs  []string `json:"omitted_step_ids"`
		Steps           []any    `json:"steps"`
		Notice          string   `json:"notice"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(resp.ReturnedStepIDs) != 5 {
		t.Errorf("expected 5 returned steps, got %d", len(resp.ReturnedStepIDs))
	}
	if len(resp.OmittedStepIDs) != 2 {
		t.Errorf("expected 2 omitted steps, got %d", len(resp.OmittedStepIDs))
	}
	if !strings.Contains(resp.Notice, "Maximum 5 steps") {
		t.Errorf("expected cap notice, got: %s", resp.Notice)
	}
}

// TestP4_CrossRunTraceIsolation verifies that a worker cannot search or read traces from another run.
func TestP4_CrossRunTraceIsolation(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task1, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Run 1 Task", Objective: "Run 1"})
	task2, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Run 2 Task", Objective: "Run 2"})

	run1, _, _ := env.runService.Create(ctx, task1.ID, model.CreateRunRequest{}, nil)
	run2, _, _ := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{}, nil)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run1.ID)
	_, _ = env.pool.Exec(ctx, `UPDATE runs SET status='running', started_at=now() WHERE id=$1`, run2.ID)

	// In Run 1, create a tool call with secret
	plan1, _ := env.runService.SubmitPlan(ctx, run1.ID, []service.PlanItemSpec{
		{Key: "work_1", Title: "Work 1", Objective: "obj", WorkType: "intent"},
	})
	wi1 := plan1[0]
	_, _, sess1, _ := env.runService.ClaimNextPendingWorkItem(ctx, run1.ID)
	now := time.Now()
	tc1, _ := env.runService.CreateAgentToolCallWithSession(ctx, run1.ID, &sess1.ID, nil, "call_1", "get_endpoint", json.RawMessage(`{}`))
	_ = env.runService.UpdateAgentToolCall(ctx, tc1.ID, "success", "CONFIDENTIAL_SECRET_12345", nil, &now, &now)

	// In Run 2, attempt to search for the secret
	searchTool := tools.NewSearchAllWorkerTracesTool(run2.ID, nil, env.runService)
	searchArgs, _ := json.Marshal(map[string]any{"q": "CONFIDENTIAL_SECRET_12345"})
	searchOut, err := searchTool.Execute(ctx, searchArgs)
	if err != nil {
		t.Fatalf("search failed: %v", err)
	}
	var searchResp struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal([]byte(searchOut), &searchResp)
	if searchResp.Total != 0 {
		t.Errorf("cross-run violation: Run 2 found trace from Run 1! total=%d", searchResp.Total)
	}

	// In Run 2, attempt to get Worker 1's trace directly
	getTraceTool := tools.NewGetWorkerTraceTool(run2.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]any{"work_item_id": wi1.ID.String()})
	_, err = getTraceTool.Execute(ctx, getArgs)
	if err == nil {
		t.Fatalf("expected get_worker_trace across runs to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "belongs to run") && !strings.Contains(err.Error(), "cross-run") {
		t.Errorf("expected cross-run error, got: %v", err)
	}
}
