package test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestContextRecovery_ShortHistoryCheckpointKeepsRawTranscript(t *testing.T) {
	requireMA5(t)
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "context", Title: "Context", Objective: "Reuse existing evidence", WorkType: "intent"}})
	require.NoError(t, err)
	_, _, sess, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	messages := []provider.ChatMessage{{Role: provider.RoleSystem, Content: "Immutable authorization"}, {Role: provider.RoleUser, Content: strings.Repeat("Original task and scope. ", 2400)}}
	for i := 0; i < 3; i++ {
		id := fmt.Sprintf("saved-%d", i)
		messages = append(messages,
			provider.ChatMessage{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: id, Function: provider.FunctionCall{Name: "read_artifact", Arguments: `{}`}}}, NativePayload: &provider.NativePayload{Format: "responses", Output: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"reasoning","encrypted_content":"opaque-%d"}`, i))}}},
			provider.ChatMessage{Role: provider.RoleTool, ToolCallID: id, Content: strings.Repeat("Saved output. ", 1100)})
	}
	original := provider.ConversationHash(messages)
	b := model.DefaultBudgetConfig()
	b.ContextWindowBytes = 80000
	p := &review2ScriptedEngineProvider{onCall: func(view []provider.ChatMessage, _ []provider.ToolDefinition) (*provider.ModelResponse, error) {
		packed, err := provider.EncodeCheckpoint(view)
		require.NoError(t, err)
		require.LessOrEqual(t, len(packed), b.ContextWindowBytes)
		require.Equal(t, messages[:2], view[:2])
		require.Equal(t, messages[6:], view[len(view)-2:])
		return &provider.ModelResponse{Content: "Existing output reused", FinishReason: "stop"}, nil
	}}
	sr := agent.NewSessionRunner(sess.ID, runID, "worker", e.runService, p, tools.NewRegistry(), b)
	res, err := sr.Run(ctx, messages)
	require.NoError(t, err)
	require.Equal(t, "completed", res.FinishReason)
	raw, err := e.runService.LoadWorkItemConversation(ctx, items[0].ID)
	require.NoError(t, err)
	require.Equal(t, original, provider.ConversationHash(raw[:len(messages)]))
	checkpoints, err := e.runService.ListContextCheckpoints(ctx, runID)
	require.NoError(t, err)
	require.Len(t, checkpoints, 1)
	require.Equal(t, "extractive_protocol_blocks_v2", checkpoints[0]["strategy"])
	restored, err := e.runService.LoadWorkItemContext(ctx, items[0].ID)
	require.NoError(t, err)
	require.Equal(t, messages[:2], restored[:2])
	require.NotNil(t, restored[len(restored)-3].NativePayload)
	var executions int
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT count(*) FROM agent_tool_calls WHERE session_id=$1`, sess.ID).Scan(&executions))
	require.Zero(t, executions, "restoring history must not execute old tools")
}

func TestContextRecovery_UncompressiblePlannerStopsWithDiagnosis(t *testing.T) {
	requireMA5(t)
	e := setupTestEnv(t)
	defer e.teardown()
	ctx := context.Background()
	task, err := e.taskService.Create(ctx, model.CreateTaskRequest{Name: "Oversized task", Objective: strings.Repeat("Protected task requirement. ", 5000), Scope: json.RawMessage(`{"endpoints":["sample_home"]}`)})
	require.NoError(t, err)
	run, _, err := e.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	p := &review2ScriptedEngineProvider{onCall: func(_ []provider.ChatMessage, _ []provider.ToolDefinition) (*provider.ModelResponse, error) {
		t.Error("an oversized protected input must stop before making a provider call")
		return &provider.ModelResponse{FinishReason: "stop"}, nil
	}}
	engine := orchestration.NewEngine(run.ID, e.runService, p, model.DefaultBudgetConfig())
	require.NoError(t, e.manager.StartExecutor(run.ID, engine))
	require.Eventually(t, func() bool { r, err := e.runService.Get(ctx, run.ID); return err == nil && r.Status.IsTerminal() }, 5*time.Second, 20*time.Millisecond)
	require.NoError(t, e.manager.Wait(time.Second))
	finished, err := e.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusStopped, finished.Status)
	require.NotNil(t, finished.FinishReason)
	require.Equal(t, "needs_input", *finished.FinishReason)
	var status, reason string
	require.NoError(t, e.pool.QueryRow(ctx, `SELECT status,finish_reason FROM agent_sessions WHERE run_id=$1 AND role='planner' ORDER BY created_at DESC LIMIT 1`, run.ID).Scan(&status, &reason))
	require.Equal(t, "stopped", status)
	require.Equal(t, "context_limit_exceeded", reason)
}
