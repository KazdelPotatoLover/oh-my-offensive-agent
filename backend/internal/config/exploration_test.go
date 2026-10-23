package config

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestExplorationDefaultsAndGlobalOverride(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", "")
	b, err := ExplorationBudget()
	require.NoError(t, err)
	require.Equal(t, 3, b.MaxParallelWorkers)
	require.Equal(t, 1800, b.RunTimeoutSeconds)
	require.Equal(t, 20, b.WorkerMaxModelCalls)
	require.Greater(t, b.MaxModelCalls-b.PlannerReservedModelCalls-b.MainReservedModelCalls, 3*b.WorkerMaxModelCalls)
	t.Setenv("OMA_EXPLORATION_BUDGET", `{"max_parallel_workers":5,"run_timeout_seconds":3600,"max_model_calls":30}`)
	b, err = ExplorationBudget()
	require.NoError(t, err)
	b = ManagedReserves(b)
	require.Equal(t, 5, b.MaxParallelWorkers)
	require.Equal(t, 3600, b.RunTimeoutSeconds)
	require.Equal(t, 3, b.PlannerReservedModelCalls)
	require.Equal(t, 1, b.MainReservedModelCalls)
	require.Equal(t, 600, b.MaxToolCalls)
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
