package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExplorationDefaults_InheritOverrideAndFreeze(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", `{"max_parallel_workers":5,"run_timeout_seconds":2400}`)
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	tasks := service.NewTaskService(pool)
	runs := service.NewRunService(pool)
	create := func(config string) *model.Task {
		task, err := tasks.Create(ctx, model.CreateTaskRequest{Name: "托管探索", Objective: "检查入口并保存证据", Config: json.RawMessage(config)})
		require.NoError(t, err)
		return task
	}
	budgetOf := func(run *model.Run) model.BudgetConfig {
		var snapshot struct {
			Budget model.BudgetConfig `json:"budget"`
		}
		require.NoError(t, json.Unmarshal(run.ExecutionInput, &snapshot))
		return snapshot.Budget
	}
	task := create(`{"exploration":true,"executor":"orchestrator","provider":"mock"}`)
	key := uuid.NewString()
	run, existed, err := runs.Create(ctx, task.ID, model.CreateRunRequest{}, &key)
	require.NoError(t, err)
	require.False(t, existed)
	b := budgetOf(run)
	require.Equal(t, 5, b.MaxParallelWorkers)
	require.Equal(t, 2400, b.RunTimeoutSeconds)
	require.Equal(t, 0, b.MaxModelCalls)
	require.Equal(t, 0, b.MaxToolCalls)
	require.Equal(t, 0, b.WorkerMaxModelCalls)
	require.Equal(t, 1200, b.WorkerTimeoutSeconds)
	require.Equal(t, 0, b.PlannerReservedModelCalls)
	require.Equal(t, "agent_limits.v1", policyOf(t, run).SchemaVersion)
	require.False(t, policyOf(t, run).ModelLimited())

	// Changing deployment defaults cannot increase an already accepted grant,
	// including an idempotent retry of the original create request.
	t.Setenv("OMA_EXPLORATION_BUDGET", `{"max_parallel_workers":8,"max_model_calls":1000}`)
	saved, err := runs.Get(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, b, budgetOf(saved))
	replay, existed, err := runs.Create(ctx, task.ID, model.CreateRunRequest{}, &key)
	require.NoError(t, err)
	require.True(t, existed)
	require.Equal(t, b, budgetOf(replay))

	task = create(`{"exploration":true,"executor":"orchestrator","provider":"mock","budget":{"max_parallel_workers":4,"max_model_calls":90,"run_timeout_seconds":900}}`)
	run, _, err = runs.Create(ctx, task.ID, model.CreateRunRequest{Budget: &model.BudgetConfig{RunTimeoutSeconds: 600}}, nil)
	require.NoError(t, err)
	b = budgetOf(run)
	require.Equal(t, 4, b.MaxParallelWorkers, "task settings override global defaults")
	require.Equal(t, 600, b.RunTimeoutSeconds, "only the chosen request field overrides task settings")
	require.Equal(t, 90, b.MaxModelCalls)
	require.Equal(t, 0, b.PlannerReservedModelCalls)
	require.Equal(t, 0, b.MainReservedModelCalls)
	require.True(t, policyOf(t, run).ModelLimited())
	require.False(t, policyOf(t, run).ToolLimited())

	task = create(`{"exploration":true,"executor":"orchestrator","provider":"mock","budget":{"max_model_calls":40,"planner_reserved_model_calls":6,"main_reserved_model_calls":4}}`)
	run, _, err = runs.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	b = budgetOf(run)
	require.Equal(t, 40, b.MaxModelCalls)
	require.Equal(t, 6, b.PlannerReservedModelCalls, "an explicit old reserve is a compatible cap")
	require.Equal(t, 4, b.MainReservedModelCalls)

	zero := 0
	task = create(`{"exploration":true,"executor":"orchestrator","provider":"mock","budget":{"run_timeout_seconds":900}}`)
	run, _, err = runs.Create(ctx, task.ID, model.CreateRunRequest{ExecutionPolicy: &model.PolicyOverride{TaskTimeoutSeconds: &zero}}, nil)
	require.NoError(t, err)
	require.Equal(t, 0, budgetOf(run).RunTimeoutSeconds)
	require.False(t, policyOf(t, run).HasDeadline())

	task = create(`{"executor":"orchestrator","provider":"mock","budget":{"max_model_calls":12}}`)
	run, _, err = runs.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	b = budgetOf(run)
	require.Equal(t, 1, b.MaxParallelWorkers, "legacy non-exploration creation retains its defaults")
	require.Equal(t, 120, b.RunTimeoutSeconds)
	require.Equal(t, 12, b.MaxModelCalls)
	require.Nil(t, model.PolicyFromExecutionInput(run.ExecutionInput))
}

func policyOf(t *testing.T, run *model.Run) model.ExecutionPolicy {
	t.Helper()
	policy := model.PolicyFromExecutionInput(run.ExecutionInput)
	require.NotNil(t, policy)
	return *policy
}

func TestExplorationDefaults_HTTPInheritsGlobalAndTaskWorkerCounts(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", `{"max_parallel_workers":5}`)
	env := setupTestEnv(t)
	defer env.teardown()
	for _, tc := range []struct {
		config  string
		workers int
	}{
		{`{"exploration":true,"executor":"orchestrator","provider":"mock"}`, 5},
		{`{"exploration":true,"executor":"orchestrator","provider":"mock","budget":{"max_parallel_workers":2}}`, 2},
	} {
		task, err := env.taskService.Create(context.Background(), model.CreateTaskRequest{Name: "默认设置 HTTP", Objective: "检查入口", Config: json.RawMessage(tc.config)})
		require.NoError(t, err)
		response := httptest.NewRecorder()
		env.router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/tasks/"+task.ID.String()+"/runs", strings.NewReader(`{"provider":"mock","mock_scenario":"orchestration"}`)))
		require.Equal(t, http.StatusCreated, response.Code, response.Body.String())
		var run model.Run
		require.NoError(t, json.Unmarshal(response.Body.Bytes(), &run))
		env.manager.Stop(run.ID)
		var snapshot struct {
			Budget model.BudgetConfig `json:"budget"`
		}
		require.NoError(t, json.Unmarshal(run.ExecutionInput, &snapshot))
		require.Equal(t, tc.workers, snapshot.Budget.MaxParallelWorkers)
		require.Equal(t, 0, snapshot.Budget.MaxModelCalls)
		require.Equal(t, 0, snapshot.Budget.RunTimeoutSeconds)
		policy := model.PolicyFromExecutionInput(run.ExecutionInput)
		require.NotNil(t, policy)
		require.Equal(t, "agent_limits.v1", policy.SchemaVersion)
	}
}
