package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRemaining_ProfileIdentityAndFactLineage(t *testing.T) {
	e, runID, assets, tc := review2ProbeSetup(t)
	ctx := context.Background()
	f1, err := e.runService.RecordFact(ctx, runID, "One observation", "", "observed", tc.String(), nil, []uuid.UUID{assets[0].ID})
	require.NoError(t, err)
	f2, err := e.runService.RecordFact(ctx, runID, "Another observation", "", "observed", tc.String(), nil, []uuid.UUID{assets[0].ID})
	require.NoError(t, err)
	spec := service.PlanItemSpec{Key: "direction", Title: "Investigate evidence", Objective: "Investigate shared evidence", WorkType: "intent", InputData: map[string]any{"profile": "http_readonly"}, AssetIDs: []uuid.UUID{assets[0].ID}, SourceFactIDs: []uuid.UUID{f1.ID, f2.ID}}
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{spec})
	require.NoError(t, err)
	require.Len(t, items, 1)
	var links int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM exploration_edges e JOIN exploration_nodes n ON n.id=e.dst_id WHERE n.work_item_id=$1 AND e.rel='derived_from'`, items[0].ID).Scan(&links))
	require.Equal(t, 2, links)
	spec.Key = "renamed_direction"
	_, err = e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{spec})
	require.Error(t, err, "renaming the same direction must not bypass continuation limits")
	spec.Key = "unauthorized_profile"
	spec.InputData = map[string]any{"profile": "invented_shell"}
	_, err = e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{spec})
	require.Error(t, err)
	spec.Key = "foreign_fact"
	spec.InputData = map[string]any{"profile": "http_readonly"}
	spec.SourceFactIDs = []uuid.UUID{uuid.New()}
	_, err = e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{spec})
	require.Error(t, err)
}

func TestRemaining_ProfileEngineSelectsToolsAndPreservesOutputs(t *testing.T) {
	e := setupTestEnv(t)
	defer e.teardown()
	ctx := context.Background()
	config := json.RawMessage(`{"profiles":[{"name":"saved_response_analysis","version":"1","max_model_calls":5,"tools":[{"name":"read_sample"},{"name":"inspect_artifact"}]}]}`)
	task, err := e.taskService.Create(ctx, model.CreateTaskRequest{Name: "Profile tools", Scope: json.RawMessage(`{"endpoints":["sample_home"]}`), Config: config})
	require.NoError(t, err)
	run, _, err := e.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	var inspected bool
	p := &review2ScriptedEngineProvider{onCall: func(msgs []provider.ChatMessage, defs []provider.ToolDefinition) (*provider.ModelResponse, error) {
		last := msgs[len(msgs)-1]
		planner := strings.Contains(msgs[0].Content, "Lead Security")
		call := func(id, name, args string) (*provider.ModelResponse, error) {
			return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: id, Type: "function", Function: provider.FunctionCall{Name: name, Arguments: args}}}}, nil
		}
		if planner {
			if last.Role == provider.RoleTool {
				return &provider.ModelResponse{Content: "Plan acknowledged", FinishReason: "stop"}, nil
			}
			if strings.Contains(last.Content, "Break down") {
				return call("profile_plan", "submit_plan", `{"items":[{"key":"analyze_response","title":"Analyze evidence","objective":"Read sample then inspect the recorded artifact","work_type":"intent","input_data":{"profile":"saved_response_analysis"}}]}`)
			}
			return call("profile_finish", "suggest_completion", `{"final_summary":"Response analyzed","conclusion":"partial"}`)
		}
		for _, def := range defs {
			require.NotEqual(t, "get_endpoint", def.Name, "profile must not leak unapproved target tools")
		}
		if last.Role == provider.RoleTool && last.ToolCallID == "profile_read" {
			var data struct {
				Artifact struct {
					ID string `json:"id"`
				} `json:"_artifact"`
			}
			require.NoError(t, json.Unmarshal([]byte(last.Content), &data))
			require.NotEmpty(t, data.Artifact.ID)
			args, _ := json.Marshal(map[string]any{"artifact_id": data.Artifact.ID, "parse_json": true})
			return call("profile_inspect", "inspect_artifact", string(args))
		}
		if last.Role == provider.RoleTool && last.ToolCallID == "profile_inspect" {
			inspected = true
			return &provider.ModelResponse{Content: "Original response inspected through its immutable artifact", FinishReason: "stop"}, nil
		}
		return call("profile_read", "read_sample", `{"sample_id":"sample_home"}`)
	}}
	engine := orchestration.NewEngine(run.ID, e.runService, p, model.DefaultBudgetConfig())
	require.NoError(t, e.manager.StartExecutor(run.ID, engine))
	require.Eventually(t, func() bool { r, err := e.runService.Get(ctx, run.ID); return err == nil && r.Status.IsTerminal() }, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, e.manager.Wait(time.Second))
	require.True(t, inspected)
	artifacts, err := e.runService.ListArtifacts(ctx, run.ID, nil, 0, 100)
	require.NoError(t, err)
	require.NotEmpty(t, artifacts)
	var reads, inspections int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FILTER(WHERE tool_name='read_sample'),count(*) FILTER(WHERE tool_name='inspect_artifact') FROM agent_tool_calls WHERE run_id=$1 AND status='success'`, run.ID).Scan(&reads, &inspections))
	require.Equal(t, 1, reads)
	require.Equal(t, 1, inspections)
}

func TestRemaining_ArtifactPagingAndWorkspaceIsolation(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	root := t.TempDir()
	e.runService.SetWorkspaceRoot(root)
	content := strings.Repeat("证据✓", 20000)
	a, err := e.runService.SaveArtifact(ctx, runID, nil, nil, nil, "evidence.txt", "text/plain", content)
	require.NoError(t, err)
	var rebuilt strings.Builder
	offset := 0
	for {
		page, err := e.runService.ReadArtifact(ctx, runID, a.ID, offset, 8192)
		require.NoError(t, err)
		rebuilt.WriteString(page["content"].(string))
		offset = page["next_offset"].(int)
		if !page["has_more"].(bool) {
			break
		}
	}
	require.Equal(t, content, rebuilt.String())
	_, err = e.runService.ReadArtifact(ctx, uuid.New(), a.ID, 0, 100)
	require.ErrorIs(t, err, service.ErrCrossRunBoundary)
	_, err = e.runService.SaveArtifact(ctx, runID, nil, nil, nil, "../escape", "text/plain", "payload")
	require.Error(t, err)
	_, err = e.runService.EnsureWorkspace(ctx, uuid.New(), nil)
	require.Error(t, err)
	files, err := filepath.Glob(filepath.Join(root, "runs", runID.String(), "*evidence.txt"))
	require.NoError(t, err)
	require.Len(t, files, 1)
	bytes, err := os.ReadFile(files[0])
	require.NoError(t, err)
	require.Equal(t, content, string(bytes))
	// A service restart can still read the authoritative body without a disk projection.
	e.runService.SetWorkspaceRoot(t.TempDir())
	page, err := e.runService.ReadArtifact(ctx, runID, a.ID, 0, 100)
	require.NoError(t, err)
	require.True(t, page["has_more"].(bool))
}

func TestRemaining_ContextCheckpointRunsAndRestoresNativeHistory(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "checkpoint", Title: "Context", Objective: "Restore context", WorkType: "intent"}})
	require.NoError(t, err)
	_, _, sess, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	messages := []provider.ChatMessage{{Role: provider.RoleSystem, Content: "Immutable scope"}}
	for i := 0; i < 30; i++ {
		messages = append(messages, provider.ChatMessage{Role: provider.RoleUser, Content: strings.Repeat("historical progress ", 300)}, provider.ChatMessage{Role: provider.RoleAssistant, Content: "Recorded", NativePayload: &provider.NativePayload{Format: "responses", Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"opaque-source"}`)}}})
	}
	b := model.DefaultBudgetConfig()
	b.ContextWindowBytes = 40000
	p := &review2ScriptedEngineProvider{onCall: func(view []provider.ChatMessage, _ []provider.ToolDefinition) (*provider.ModelResponse, error) {
		require.Less(t, len(view), len(messages))
		require.Equal(t, "Immutable scope", view[0].Content)
		return &provider.ModelResponse{Content: "Bounded context restored", FinishReason: "stop"}, nil
	}}
	sr := agent.NewSessionRunner(sess.ID, runID, "worker", e.runService, p, tools.NewRegistry(), b)
	_, err = sr.Run(ctx, messages)
	require.NoError(t, err)
	checkpoints, err := e.runService.ListContextCheckpoints(ctx, runID)
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	view, err := e.runService.LoadWorkItemContext(ctx, items[0].ID)
	require.NoError(t, err)
	require.Less(t, len(view), len(messages))
	raw, err := e.runService.LoadWorkItemConversation(ctx, items[0].ID)
	require.NoError(t, err)
	require.Greater(t, len(raw), len(view))
	require.NotNil(t, view[len(view)-2].NativePayload)
	_, err = e.pool.Exec(ctx, `UPDATE agent_messages SET content='tampered original' WHERE session_id=$1 AND seq=2`, sess.ID)
	require.NoError(t, err)
	_, err = e.runService.LoadWorkItemContext(ctx, items[0].ID)
	require.Error(t, err, "checkpoint must not silently mask a source mismatch")
}
