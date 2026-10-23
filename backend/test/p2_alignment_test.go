package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// TestP2_SetGoalsAndListGoals tests goal setting and listing in exploration graph.
func TestP2_SetGoalsAndListGoals(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P2 Goal Setting Task",
		Objective: "Test goal management in exploration graph",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	specs := []model.GoalSpec{
		{Text: "Discover internal administration endpoint", VulnClass: "recon"},
		{Text: "Verify SQL injection vulnerability in search parameter", VulnClass: "sqli"},
	}

	// 1. Set goals via Service
	nodes, err := env.runService.SetGoals(ctx, run.ID, specs)
	if err != nil {
		t.Fatalf("SetGoals failed: %v", err)
	}
	if len(nodes) != 2 {
		t.Fatalf("expected 2 goal nodes, got %d", len(nodes))
	}
	for _, n := range nodes {
		if n.Kind != "goal" {
			t.Errorf("expected kind 'goal', got '%s'", n.Kind)
		}
		if n.State != "open" {
			t.Errorf("expected state 'open', got '%s'", n.State)
		}
	}

	// 2. List goals
	listed, err := env.runService.ListGoals(ctx, run.ID)
	if err != nil {
		t.Fatalf("ListGoals failed: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("expected 2 listed goals, got %d", len(listed))
	}

	// 3. Test through Tool abstraction
	tool := tools.NewListGoalsTool(run.ID, env.runService)
	out, err := tool.Execute(ctx, []byte(`{}`))
	if err != nil {
		t.Fatalf("ListGoalsTool failed: %v", err)
	}
	var resp struct {
		Total int `json:"total"`
		Goals []struct {
			ID        string `json:"id"`
			State     string `json:"state"`
			Text      string `json:"text"`
			VulnClass string `json:"vulnclass"`
		} `json:"goals"`
	}
	if err := json.Unmarshal([]byte(out), &resp); err != nil {
		t.Fatalf("unmarshal tool output: %v", err)
	}
	if resp.Total != 2 || len(resp.Goals) != 2 {
		t.Fatalf("expected 2 goals in tool output, got %d", resp.Total)
	}
}

// TestP2_AssetGraph_Relations tests typed asset topology relationships.
func TestP2_AssetGraph_Relations(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	var rootAssetID, subAssetID uuid.UUID
	err := env.pool.QueryRow(ctx,
		`INSERT INTO assets (namespace, kind, external_key, name, description)
		 VALUES ('network', 'domain', 'example.com', 'Root Domain', 'Primary target domain')
		 RETURNING id`,
	).Scan(&rootAssetID)
	if err != nil {
		t.Fatalf("create root asset: %v", err)
	}

	err = env.pool.QueryRow(ctx,
		`INSERT INTO assets (namespace, kind, external_key, name, description)
		 VALUES ('network', 'subdomain', 'admin.example.com', 'Admin Subdomain', 'Administration portal')
		 RETURNING id`,
	).Scan(&subAssetID)
	if err != nil {
		t.Fatalf("create sub asset: %v", err)
	}

	// Establish typed relation: admin.example.com --subdomain_of--> example.com
	meta := json.RawMessage(`{"dns_record":"CNAME","discovered_method":"passive_dns"}`)
	rel, err := env.runService.AddAssetRelation(ctx, subAssetID, rootAssetID, "subdomain_of", meta)
	if err != nil {
		t.Fatalf("AddAssetRelation failed: %v", err)
	}
	if rel.RelationType != "subdomain_of" {
		t.Errorf("expected relation_type subdomain_of, got %s", rel.RelationType)
	}

	// Query relations for both assets
	subRels, err := env.runService.ListAssetRelations(ctx, subAssetID)
	if err != nil {
		t.Fatalf("ListAssetRelations for subAsset: %v", err)
	}
	if len(subRels) != 1 {
		t.Fatalf("expected 1 relation for subAsset, got %d", len(subRels))
	}

	rootRels, err := env.runService.ListAssetRelations(ctx, rootAssetID)
	if err != nil {
		t.Fatalf("ListAssetRelations for rootAsset: %v", err)
	}
	if len(rootRels) != 1 {
		t.Fatalf("expected 1 relation for rootAsset, got %d", len(rootRels))
	}
}

// TestP2_ExplorationGraph_LineageAndGoalProof (Contract E3):
// 2 Facts -> Derived Intent -> Yields Fact -> Proves Goal.
func TestP2_ExplorationGraph_LineageAndGoalProof(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "E3 Proof Task", Objective: "Prove goal from 2 facts", Scope: json.RawMessage(`{"endpoints":["sample_home"]}`)})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)

	// 1. Define 1 Goal: Extract DB version
	goals, err := env.runService.SetGoals(ctx, run.ID, []model.GoalSpec{
		{Text: "Extract Database Version through SQLi", VulnClass: "sqli"},
	})
	if err != nil {
		t.Fatalf("set goals: %v", err)
	}
	goalNode := goals[0]

	// 2. Worker 1 records Fact 1: /api/search exists
	fact1, err := env.runService.RecordFact(
		ctx, run.ID,
		"Discovered /api/search endpoint accepting q parameter",
		"HTTP 200 OK with JSON array response on GET /api/search?q=test",
		"observed",
		"GET /api/search?q=test -> HTTP 200",
		nil, nil,
	)
	if err != nil {
		t.Fatalf("record fact 1: %v", err)
	}

	// 3. Worker 2 records Fact 2: Syntax error detected
	fact2, err := env.runService.RecordFact(
		ctx, run.ID,
		"Single quote injection causes 500 SQL syntax error",
		"Payload q=test' triggered unhandled Postgres syntax error in response body",
		"observed",
		"GET /api/search?q=test' -> HTTP 500 'syntax error at or near'",
		nil, nil,
	)
	if err != nil {
		t.Fatalf("record fact 2: %v", err)
	}

	// 4. Planner derives new Intent from Fact 1 and Fact 2
	intentSummary := "Exploit Postgres SQLi via UNION SELECT on /api/search"
	intentNode, err := env.runService.AddIntentNode(
		ctx, run.ID,
		intentSummary,
		[]uuid.UUID{fact1.ID, fact2.ID},
		nil,
		10,
		nil,
	)
	if err != nil {
		t.Fatalf("AddIntentNode failed: %v", err)
	}

	// Verify derived_from edges
	detail, err := env.runService.GetExplorationNodeDetail(ctx, run.ID, intentNode.ID)
	if err != nil {
		t.Fatalf("GetExplorationNodeDetail: %v", err)
	}
	inEdges := detail["incoming_edges"].([]map[string]any)
	if len(inEdges) != 2 {
		t.Fatalf("expected 2 incoming derived_from edges, got %d", len(inEdges))
	}
	for _, e := range inEdges {
		if e["rel"] != "derived_from" {
			t.Errorf("expected rel derived_from, got %s", e["rel"])
		}
	}

	// 5. Worker executes intent and yields Fact 3: Database version extracted!
	var dbVersionToolCallID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call_union_select', 'read_sample', '{"sample_id":"sample_home"}', '{"status_code":200,"body":"PostgreSQL 15.2 on x86_64"}', 'success')
		RETURNING id`, run.ID).Scan(&dbVersionToolCallID)
	if err != nil {
		t.Fatalf("insert db version tool call: %v", err)
	}

	fact3, err := env.runService.RecordFact(
		ctx, run.ID,
		"Successfully extracted Postgres 15.2 banner via UNION SELECT",
		"Payload: q=' UNION SELECT 1, version(), 3-- yielded PostgreSQL 15.2 on x86_64",
		"observed",
		dbVersionToolCallID.String(),
		&intentNode.ID,
		nil,
	)
	if err != nil {
		t.Fatalf("record fact 3: %v", err)
	}

	// Verify yields edge from intentNode to fact3
	fact3Detail, err := env.runService.GetExplorationNodeDetail(ctx, run.ID, fact3.ID)
	if err != nil {
		t.Fatalf("fact3 detail: %v", err)
	}
	f3InEdges := fact3Detail["incoming_edges"].([]map[string]any)
	if len(f3InEdges) != 1 || f3InEdges[0]["rel"] != "yields" {
		t.Fatalf("expected 1 incoming yields edge for fact 3, got %+v", f3InEdges)
	}

	// 6. Prove Goal using Fact 3
	proveTool := tools.NewProveGoalTool(run.ID, env.runService, nil)
	proveArgs, _ := json.Marshal(map[string]string{
		"goal_id":     goalNode.ID.String(),
		"evidence_id": fact3.ID.String(),
		"reason":      "Postgres 15.2 banner extracted directly satisfies DB version goal",
	})
	proveOut, err := proveTool.Execute(ctx, proveArgs)
	if err != nil {
		t.Fatalf("prove_goal failed: %v", err)
	}

	var proveResp struct {
		Status      string `json:"status"`
		AllGoalsMet bool   `json:"all_goals_met"`
		GoalID      string `json:"goal_id"`
	}
	if err := json.Unmarshal([]byte(proveOut), &proveResp); err != nil {
		t.Fatalf("unmarshal prove response: %v", err)
	}
	if !proveResp.AllGoalsMet {
		t.Errorf("expected AllGoalsMet to be true")
	}

	// 7. Verify Goal node updated to 'met' and has 'proves' edge
	goalDetail, err := env.runService.GetExplorationNodeDetail(ctx, run.ID, goalNode.ID)
	if err != nil {
		t.Fatalf("goal detail: %v", err)
	}
	gNode := goalDetail["node"].(*model.ExplorationNode)
	if gNode.State != "met" {
		t.Errorf("expected goal node state 'met', got '%s'", gNode.State)
	}
	gInEdges := goalDetail["incoming_edges"].([]map[string]any)
	proves, spawns := 0, 0
	for _, e := range gInEdges {
		switch e["rel"] {
		case "proves":
			proves++
			if e["src_id"] != fact3.ID {
				t.Errorf("proves edge must come from the yielded fact, got %v", e["src_id"])
			}
		case "spawns":
			spawns++
		default:
			t.Errorf("unexpected incoming relation %v", e["rel"])
		}
	}
	if proves != 1 || spawns != 1 {
		t.Fatalf("expected one task-start spawns edge and exactly one proves edge, got %+v", gInEdges)
	}

	// 8. Verify GraphOverview
	overview, err := env.runService.GraphOverview(ctx, run.ID)
	if err != nil {
		t.Fatalf("GraphOverview failed: %v", err)
	}
	if overview["all_goals_met"] != true {
		t.Errorf("expected all_goals_met true in GraphOverview")
	}
	if overview["total_goals"] != 1 || overview["met_goals"] != 1 {
		t.Errorf("expected 1/1 goals met, got total=%v, met=%v", overview["total_goals"], overview["met_goals"])
	}
}

// TestP2_CrossRunBoundary_Reject verifies cross-run evidence and parent isolation.
func TestP2_CrossRunBoundary_Reject(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task1, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "CrossRun Task 1", Objective: "Test cross-run rejection 1"})
	if err != nil {
		t.Fatalf("create task 1: %v", err)
	}
	task2, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "CrossRun Task 2", Objective: "Test cross-run rejection 2"})
	if err != nil {
		t.Fatalf("create task 2: %v", err)
	}
	run1, _, err := env.runService.Create(ctx, task1.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}
	run2, _, err := env.runService.Create(ctx, task2.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}

	// Run 1: Set goal
	goals1, _ := env.runService.SetGoals(ctx, run1.ID, []model.GoalSpec{{Text: "Run 1 Goal"}})
	goal1 := goals1[0]

	// Run 2: Record fact
	fact2, _ := env.runService.RecordFact(ctx, run2.ID, "Fact from Run 2", "detail", "observed", "evidence", nil, nil)

	// 1. Proving Run 1's goal with Run 2's fact MUST be rejected
	_, err = env.runService.ProveGoal(ctx, run1.ID, goal1.ID, fact2.ID, "Trying to use Run 2 evidence")
	if err == nil {
		t.Fatalf("expected cross-run ProveGoal to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "cross-run") && !strings.Contains(err.Error(), "belongs to run") {
		t.Errorf("expected cross-run error message, got: %v", err)
	}

	// 2. Deriving Run 1's intent from Run 2's fact MUST be rejected
	_, err = env.runService.AddIntentNode(ctx, run1.ID, "Intent in Run 1", []uuid.UUID{fact2.ID}, nil, 1, nil)
	if err == nil {
		t.Fatalf("expected cross-run AddIntentNode to fail, but it succeeded")
	}
	if !strings.Contains(err.Error(), "cross-run") && !strings.Contains(err.Error(), "belongs to another run") {
		t.Errorf("expected cross-run error message, got: %v", err)
	}
}

// TestP2_SuggestCompletion_BlockedByUnmetGoals verifies that completion cannot be declared
// if any goal in the exploration graph remains unproven.
func TestP2_SuggestCompletion_BlockedByUnmetGoals(t *testing.T) {
	env := setupTestEnv(t)
	defer env.pool.Close()
	defer env.manager.Close()

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "Unmet Goal Block Task", Objective: "Test completion blocking", Scope: json.RawMessage(`{"endpoints":["sample_home"]}`)})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)

	// Define 2 goals
	goals, _ := env.runService.SetGoals(ctx, run.ID, []model.GoalSpec{
		{Text: "Goal 1: Find credentials"},
		{Text: "Goal 2: Gain shell access"},
	})
	goal1 := goals[0]
	goal2 := goals[1]

	suggestTool := tools.NewSuggestCompletionTool(run.ID, env.runService, nil)

	// 1. Neither goal proved -> suggest_completion complete MUST fail
	args := []byte(`{"final_summary": "All tests done", "conclusion": "complete"}`)
	_, err := suggestTool.Execute(ctx, args)
	if err == nil {
		t.Fatalf("expected suggest_completion to be blocked by unmet goals, but it succeeded")
	}
	if !strings.Contains(err.Error(), "goals are not met") {
		t.Errorf("expected 'goals are not met' in error, got: %v", err)
	}

	// Partial conclusion is allowed when goals are not fully met
	argsPartial := []byte(`{"final_summary": "Incomplete run", "conclusion": "partial"}`)
	outPartial, err := suggestTool.Execute(ctx, argsPartial)
	if err != nil {
		t.Fatalf("partial conclusion should be allowed, got error: %v", err)
	}
	if !strings.Contains(outPartial, `"conclusion":"partial"`) {
		t.Errorf("expected accepted partial conclusion, got: %s", outPartial)
	}

	// 2. Prove only Goal 1 -> suggest_completion complete MUST STILL fail
	var toolCall1ID, toolCall2ID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call_tc1', 'read_sample', '{"sample_id":"sample_home"}', '{"status_code":200,"body":"creds"}', 'success')
		RETURNING id`, run.ID).Scan(&toolCall1ID)
	if err != nil {
		t.Fatalf("insert tool call 1: %v", err)
	}
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call_tc2', 'read_sample', '{"sample_id":"sample_home"}', '{"status_code":200,"body":"sh$"}', 'success')
		RETURNING id`, run.ID).Scan(&toolCall2ID)
	if err != nil {
		t.Fatalf("insert tool call 2: %v", err)
	}

	fact1, _ := env.runService.RecordFact(ctx, run.ID, "Found admin credentials in .env", "detail", "observed", toolCall1ID.String(), nil, nil)
	allMet, err := env.runService.ProveGoal(ctx, run.ID, goal1.ID, fact1.ID, "Found creds")
	if err != nil {
		t.Fatalf("prove goal 1: %v", err)
	}
	if allMet {
		t.Fatalf("expected allMet to be false since Goal 2 is unmet")
	}

	_, err = suggestTool.Execute(ctx, args)
	if err == nil {
		t.Fatalf("expected suggest_completion complete to still fail with 1 unmet goal")
	}
	if !strings.Contains(err.Error(), "1 of 2 goals are not met") {
		t.Errorf("expected '1 of 2 goals are not met' in error, got: %v", err)
	}

	// 3. Prove Goal 2 -> suggest_completion complete MUST SUCCEED
	fact2, _ := env.runService.RecordFact(ctx, run.ID, "Gained shell via SSH with creds", "detail", "observed", toolCall2ID.String(), nil, nil)
	allMet, err = env.runService.ProveGoal(ctx, run.ID, goal2.ID, fact2.ID, "SSH shell confirmed")
	if err != nil {
		t.Fatalf("prove goal 2: %v", err)
	}
	if !allMet {
		t.Fatalf("expected allMet to be true now that both goals are proved")
	}

	outComplete, err := suggestTool.Execute(ctx, args)
	if err != nil {
		t.Fatalf("suggest_completion should succeed now, got: %v", err)
	}
	if !strings.Contains(outComplete, `"conclusion":"complete"`) {
		t.Errorf("expected accepted complete conclusion, got: %s", outComplete)
	}
}
