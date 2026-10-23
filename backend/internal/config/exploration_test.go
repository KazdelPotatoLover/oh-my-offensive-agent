package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplorationDefaultsAndGlobalOverride(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", "")
	t.Setenv("OMA_AGENT_LIMITS", "")
	b, err := ExplorationBudget()
	require.NoError(t, err)
	require.Equal(t, 3, b.MaxParallelWorkers)
	require.Equal(t, 0, b.RunTimeoutSeconds)
	require.Equal(t, 0, b.MaxModelCalls)
	require.Equal(t, 0, b.MaxToolCalls)
	require.Equal(t, 0, b.WorkerMaxModelCalls)
	require.Equal(t, 1200, b.WorkerTimeoutSeconds)
	require.Equal(t, 0, b.PlannerReservedModelCalls)
	require.Equal(t, 0, b.MainReservedModelCalls)
	require.Positive(t, b.ModelTimeoutSeconds)

	t.Setenv("OMA_EXPLORATION_BUDGET", `{"max_parallel_workers":5,"run_timeout_seconds":3600,"max_model_calls":30}`)
	b, err = ExplorationBudget()
	require.NoError(t, err)
	require.Equal(t, 5, b.MaxParallelWorkers)
	require.Equal(t, 3600, b.RunTimeoutSeconds)
	require.Equal(t, 30, b.MaxModelCalls)
	require.Equal(t, 0, b.MaxToolCalls)
	require.Equal(t, 0, b.PlannerReservedModelCalls)
	require.Equal(t, 0, b.MainReservedModelCalls)
}

func TestExplorationGlobalSettingsRejectMistakes(t *testing.T) {
	for _, raw := range []string{`{"workers":5}`, `{"max_parallel_workers":9}`, `{"max_tool_calls":-1}`, `{} {}`, `null`, `not-json`} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("OMA_EXPLORATION_BUDGET", raw)
			_, err := ExplorationBudget()
			require.Error(t, err)
		})
	}
}

func TestLegacyManagedBudgetStaysBounded(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", "")
	b, err := LegacyManagedBudget()
	require.NoError(t, err)
	require.Equal(t, 3, b.MaxParallelWorkers)
	require.Equal(t, 1800, b.RunTimeoutSeconds)
	require.Equal(t, 240, b.MaxModelCalls)
	require.Equal(t, 600, b.MaxToolCalls)
	b = ManagedReserves(b)
	require.Equal(t, 24, b.PlannerReservedModelCalls)
	require.Equal(t, 8, b.MainReservedModelCalls)
}
