package test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/controlplane"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func toolResponse(id, name string, args any) *provider.ModelResponse {
	b, _ := json.Marshal(args)
	return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: id, Function: provider.FunctionCall{Name: name, Arguments: string(b)}}}}
}

type isolatedPanicTool struct {
	phase    string
	released bool
	toolCtx  context.Context
}

func (p *isolatedPanicTool) Name() string                { return "panic_" + p.phase }
func (p *isolatedPanicTool) Description() string         { return "local panic fixture" }
func (p *isolatedPanicTool) Parameters() json.RawMessage { return json.RawMessage(`{"type":"object"}`) }
func (p *isolatedPanicTool) Validate([]byte) error {
	if p.phase == "validate" {
		panic("private panic payload")
	}
	return nil
}
func (p *isolatedPanicTool) Execute(ctx context.Context, _ []byte) (string, error) {
	p.toolCtx = ctx
	defer func() { p.released = true }()
	panic("private panic payload")
}

func TestToolPanic_WorkerPlannerPersistAndContinue(t *testing.T) {
	pool := ma3EvidencePool(t)
	svc := service.NewRunService(pool)
	ctx := context.Background()
	for _, role := range []string{"worker", "planner"} {
		t.Run(role, func(t *testing.T) {
			run, _, work := seedHTTPAsset(t, pool, `{}`, "http://127.0.0.1/", uuid.NewString())
			var session uuid.UUID
			var workPtr *uuid.UUID
			if role == "worker" {
				workPtr = &work
			}
			require.NoError(t, pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,status,work_item_id) VALUES($1,$2,'running',$3) RETURNING id`, run, role, workPtr).Scan(&session))
			reg := tools.NewRegistry()
			validate, execute := &isolatedPanicTool{phase: "validate"}, &isolatedPanicTool{phase: "execute"}
			reg.Register(validate)
			reg.Register(execute)
			turn := 0
			fake := &testFakeProvider{callFunc: func(ctx context.Context, messages []provider.ChatMessage, _ []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
				turn++
				if turn == 1 {
					return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{
						toolResponse("validation", validate.Name(), map[string]any{}).ToolCalls[0],
						toolResponse("execution", execute.Name(), map[string]any{}).ToolCalls[0],
					}}, nil
				}
				require.NoError(t, controlplane.ProtocolPairingOK(messages))
				for _, m := range messages {
					if m.Role == provider.RoleTool {
						require.NotContains(t, m.Content, "private panic payload")
						require.Contains(t, m.Content, "internal error")
					}
				}
				return &provider.ModelResponse{FinishReason: "stop", Content: "recovered tools"}, nil
			}}
			_, err := agent.NewSessionRunner(session, run, role, svc, fake, reg, model.DefaultBudgetConfig()).Run(ctx, []provider.ChatMessage{{Role: provider.RoleUser, Content: "exercise tool boundary"}})
			require.NoError(t, err)
			require.True(t, execute.released)
			require.ErrorIs(t, execute.toolCtx.Err(), context.Canceled)
			var failed, paired int
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM agent_tool_calls WHERE session_id=$1 AND status='failed' AND finished_at IS NOT NULL AND error_message NOT LIKE '%private panic payload%'`, session).Scan(&failed))
			require.Equal(t, 2, failed)
			require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM agent_messages WHERE session_id=$1 AND role='tool'`, session).Scan(&paired))
			require.Equal(t, 2, paired)
			var status string
			require.NoError(t, pool.QueryRow(ctx, `SELECT status FROM agent_sessions WHERE id=$1`, session).Scan(&status))
			require.Equal(t, "completed", status)
		})
	}
}

type panicWorkbench struct{ contract.WorkbenchQuery }

func (panicWorkbench) Snapshot(context.Context, uuid.UUID, contract.WorkbenchQueryParams) (*contract.WorkbenchSnapshot, error) {
	panic("private main panic")
}

func TestToolPanic_MainRecordsFailureAndContinues(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := createTestTask(t, env, "main panic")
	runID := insertRun(t, env, task.ID, "running", "running", 1)
	controller := controlplane.NewController(env.runService, nil, controlplane.NewMemoryReplan(), nil)
	fake := &scriptProvider{steps: []provider.ModelResponse{
		*toolResponse("panic", "get_situation", map[string]any{}),
		*toolResponse("after_panic", "graph_overview", map[string]any{}),
		{FinishReason: "stop", Content: "The first tool failed; the graph query succeeded."},
	}}
	main := controlplane.NewMainAgent(env.runService, controller, panicWorkbench{}, fake, nil)
	main.SyncDrain = true
	accepted, err := main.Submit(ctx, contract.Principal{Kind: contract.PrincipalHuman, ActorID: "local-test", RunID: runID}, "main-panic", contract.MainMessageSubmit{Message: "查询当前进展", ExpectedEpoch: 1})
	require.NoError(t, err)
	var failed, running int
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT count(*) FROM main_tool_calls WHERE run_id=$1 AND status='failed' AND finished_at IS NOT NULL`, runID).Scan(&failed))
	require.Equal(t, 1, failed)
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT count(*) FROM main_tool_calls WHERE run_id=$1 AND status='running'`, runID).Scan(&running))
	require.Zero(t, running)
	var status string
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT status FROM main_turns WHERE id=$1`, accepted.TurnID).Scan(&status))
	require.Equal(t, "completed", status)
	for _, messages := range fake.seen {
		require.NoError(t, controlplane.ProtocolPairingOK(messages))
		for _, m := range messages {
			require.False(t, strings.Contains(m.Content, "private main panic"))
		}
	}
}
