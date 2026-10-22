package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestRunRecovery_PlannerProviderAvailability(t *testing.T) {
	for _, stage := range []string{"initial", "followup"} {
		for _, status := range []int{502, 401} {
			t.Run(fmt.Sprintf("%s/%d", stage, status), func(t *testing.T) {
				env := setupTestEnv(t, false)
				defer env.teardown()
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				task := createTestTask(t, env, "recoverable planner error")
				budget := model.DefaultBudgetConfig()
				budget.MaxModelCalls = 20
				run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
				require.NoError(t, err)
				var failures atomic.Int32
				fake := semanticsProviderFunc(func(_ context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
					planner, followup := false, false
					for _, d := range defs {
						planner = planner || d.Name == "submit_plan"
						followup = followup || d.Name == "suggest_completion"
					}
					if !planner {
						return &provider.ModelResponse{FinishReason: "stop", Content: "Existing material reviewed."}, nil
					}
					if stage == "initial" || followup {
						failures.Add(1)
						class := provider.ClassClientHTTP
						if status == 502 {
							class = provider.ClassUpstream
						}
						return nil, &provider.CallError{Class: class, HTTPStatus: status, Retryable: status == 502, Summary: "upstream HTTP unavailable"}
					}
					if msgs[len(msgs)-1].Role == provider.RoleTool {
						return &provider.ModelResponse{FinishReason: "stop", Content: "Plan ready."}, nil
					}
					return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Function: provider.FunctionCall{Name: "submit_plan", Arguments: `{"items":[{"key":"review","title":"Review","objective":"Review existing material","work_type":"aggregate_summary"}]}`}}}}, nil
				})
				orchestration.NewEngine(run.ID, env.runService, fake, budget,
					orchestration.WithDebounce(time.Millisecond),
					orchestration.WithModelRecovery(provider.RecoveryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, MaxBackoff: time.Millisecond})).Run(ctx)
				final, err := env.runService.Get(context.Background(), run.ID)
				require.NoError(t, err)
				if status == 502 {
					require.Equal(t, model.RunStatusStopped, final.Status)
					require.Equal(t, "needs_input", *final.FinishReason)
					require.Contains(t, *final.FinalOutput, "模型服务暂时不可用")
					require.EqualValues(t, 3, failures.Load())
				} else {
					require.Equal(t, model.RunStatusFailed, final.Status)
					require.Equal(t, "provider_error", *final.FinishReason)
					require.EqualValues(t, 1, failures.Load())
				}
				assertNoActiveChildren(t, env, run.ID)
				before, _, err := env.runService.RunExecutionUsage(ctx, run.ID)
				require.NoError(t, err)
				epoch, err := env.runService.StartNewEpoch(ctx, run.ID, "model service recovered", "")
				require.NoError(t, err)
				require.Equal(t, 2, epoch)
				after, _, err := env.runService.RunExecutionUsage(ctx, run.ID)
				require.NoError(t, err)
				require.Equal(t, before, after, "epoch recovery must not reset model usage")
				var previousSummary string
				require.NoError(t, env.pool.QueryRow(ctx, `SELECT summary FROM execution_epochs WHERE run_id=$1 AND epoch_number=1`, run.ID).Scan(&previousSummary))
				require.Equal(t, *final.FinalOutput, previousSummary)
			})
		}
	}
}

func TestRunRecovery_WorkerReserveStopsEmptyAttempts(t *testing.T) {
	env := setupTestEnv(t, false)
	defer env.teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := createTestTask(t, env, "reserved worker quota")
	budget := model.DefaultBudgetConfig()
	budget.MaxModelCalls, budget.PlannerReservedModelCalls, budget.MainReservedModelCalls = 10, 5, 4
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	require.NoError(t, err)
	fake := semanticsProviderFunc(func(_ context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
		for _, d := range defs {
			if d.Name == "suggest_completion" || msgs[len(msgs)-1].Role == provider.RoleTool {
				return &provider.ModelResponse{FinishReason: "stop", Content: "Insufficient worker allowance."}, nil
			}
		}
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Function: provider.FunctionCall{Name: "submit_plan", Arguments: `{"items":[{"key":"review","title":"Review","objective":"Review","work_type":"aggregate_summary"}]}`}}}}, nil
	})
	orchestration.NewEngine(run.ID, env.runService, fake, budget, orchestration.WithDebounce(time.Millisecond)).Run(ctx)
	final, err := env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusStopped, final.Status)
	require.Equal(t, "needs_input", *final.FinishReason)
	require.Contains(t, *final.FinalOutput, "预留")
	var attempts int
	require.NoError(t, env.pool.QueryRow(ctx, `SELECT count(*) FROM attempts a JOIN work_items w ON w.id=a.work_item_id WHERE w.run_id=$1`, run.ID).Scan(&attempts))
	require.Zero(t, attempts, "no worker model allowance means no empty attempt")
	assertNoActiveChildren(t, env, run.ID)
}

func TestRunRecovery_ContinuationBudgetAndPlanLimit(t *testing.T) {
	env := setupTestEnv(t, false)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "continuation guard")
	budget := model.DefaultBudgetConfig()
	budget.MaxModelCalls, budget.MaxWorkItems = 5, 1
	budget.PlannerReservedModelCalls, budget.MainReservedModelCalls = 1, 1
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{Budget: &budget}, nil)
	require.NoError(t, err)
	_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{Key: "review", Title: "Review", Objective: "Review", WorkType: "aggregate_summary"}})
	require.NoError(t, err)
	wi, att, sess, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	for i := 0; i < 3; i++ {
		m, err := env.runService.CreateModelCallWithSession(ctx, run.ID, &sess.ID, i+1)
		require.NoError(t, err)
		require.NoError(t, env.runService.UpdateModelCall(ctx, m.ID, "completed", nil, nil, 0, 0, 0, nil))
	}
	require.NoError(t, env.runService.FinishWorkItem(ctx, wi.ID, att.ID, sess.ID, false, nil, "worker_model_budget_exhausted"))
	_, err = env.runService.ContinueWorkItem(ctx, run.ID, wi.ID, att.ID, "continue")
	var ce *contract.Error
	require.True(t, errors.As(err, &ce))
	require.Equal(t, contract.CodeRunBudgetExhausted, ce.Code)
	stored, err := env.runService.GetWorkItem(ctx, wi.ID)
	require.NoError(t, err)
	require.Equal(t, model.WorkItemStatusExhausted, stored.Status)
	require.Equal(t, *wi.CurrentAttemptID, *stored.CurrentAttemptID)
	callback := false
	tool := tools.NewSubmitPlanTool(run.ID, env.runService, func([]model.WorkItem) { callback = true })
	output, err := tool.Execute(ctx, []byte(`{"items":[{"key":"replacement","title":"Replacement","objective":"Repeat","work_type":"aggregate_summary"}]}`))
	require.NoError(t, err)
	require.Contains(t, output, "run_budget_exhausted")
	require.Contains(t, output, "limit=1")
	require.False(t, callback)
	key := uuid.NewString()
	_, err = env.runService.GrantBudget(ctx, run.ID, 5, 0, 0, "explicit test authorization", &key)
	require.NoError(t, err)
	_, err = env.runService.ContinueWorkItem(ctx, run.ID, wi.ID, att.ID, "budget granted")
	require.NoError(t, err)
	next, nextAtt, _, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, wi.ID, next.ID)
	require.Equal(t, 2, nextAtt.AttemptNumber)
	bindings, err := env.runService.WorkItemAssetBindings(ctx, run.ID, next.ID)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(string(bindings), "["))
}

func TestRunRecovery_WorkerReceivesAssignedAssetUUIDs(t *testing.T) {
	env := setupTestEnv(t, false)
	defer env.teardown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	target := "http://127.0.0.1:9/news"
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{Name: "asset mapping", Objective: "Review assigned asset metadata without fetching", Scope: json.RawMessage(`{"mode":"discovery","seeds":["` + target + `"]}`)})
	require.NoError(t, err)
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, assets, 1)
	var workerChecked atomic.Bool
	fake := semanticsProviderFunc(func(_ context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
		planner := false
		for _, d := range defs {
			planner = planner || d.Name == "submit_plan"
			if d.Name == "suggest_completion" {
				return &provider.ModelResponse{FinishReason: "stop", Content: "Mapping reviewed."}, nil
			}
		}
		if !planner {
			text := ""
			for _, m := range msgs {
				text += m.Content
			}
			if !strings.Contains(text, "Assigned assets") || !strings.Contains(text, assets[0].ID.String()) || !strings.Contains(text, target) {
				return nil, fmt.Errorf("missing assigned UUID/URL mapping")
			}
			workerChecked.Store(true)
			return &provider.ModelResponse{FinishReason: "stop", Content: "Assigned UUID copied."}, nil
		}
		if msgs[len(msgs)-1].Role == provider.RoleTool {
			return &provider.ModelResponse{FinishReason: "stop", Content: "Plan ready."}, nil
		}
		args := fmt.Sprintf(`{"items":[{"key":"mapping","title":"Mapping","objective":"Review assigned mapping","work_type":"aggregate_summary","asset_ids":[%q]}]}`, assets[0].ID.String())
		return &provider.ModelResponse{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "plan", Function: provider.FunctionCall{Name: "submit_plan", Arguments: args}}}}, nil
	})
	orchestration.NewEngine(run.ID, env.runService, fake, model.DefaultBudgetConfig(), orchestration.WithDebounce(time.Millisecond)).Run(ctx)
	require.True(t, workerChecked.Load(), "Worker did not receive the server's asset mapping")
	assertNoActiveChildren(t, env, run.ID)
}

func TestRunRecovery_HumanGrantBeforeRestart(t *testing.T) {
	env := setupTestEnv(t, false)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "grant stopped run")
	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `UPDATE runs SET status='stopped',finish_reason='needs_input',final_output='partial evidence' WHERE id=$1`, run.ID)
	require.NoError(t, err)
	command := map[string]any{"type": "grant_budget", "expected_epoch": 1, "payload": map[string]any{"additional_model_calls": 20, "additional_seconds": 600, "reason": "explicit human grant"}}
	path := "/api/runs/" + run.ID.String() + "/commands"
	rec := acceptanceJSON(t, env, http.MethodPost, path, command, map[string]string{"Idempotency-Key": "grant-before-recovery"})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	replay := acceptanceJSON(t, env, http.MethodPost, path, command, map[string]string{"Idempotency-Key": "grant-before-recovery"})
	require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
	stored, err := env.runService.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, model.RunStatusStopped, stored.Status, "a grant must not restart the Run")
	require.Equal(t, "partial evidence", *stored.FinalOutput)
	grants, err := env.runService.ListBudgetGrants(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, grants, 1)
	require.Equal(t, 20, grants[0].AdditionalModelCalls)
	assertNoActiveChildren(t, env, run.ID)
}
