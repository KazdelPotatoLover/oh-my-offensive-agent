package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"reflect"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
)

// ExplorationBudget is resolved only when creating a new run. Existing run
// snapshots and grants remain authoritative on resume.
func ExplorationBudget() (model.BudgetConfig, error) {
	b := model.DefaultBudgetConfig()
	b.MaxParallelWorkers = contract.DefaultExplorationWorkers
	b.RunTimeoutSeconds = 1800
	b.MaxModelCalls, b.MaxToolCalls = 240, 600
	b.WorkerMaxModelCalls, b.WorkerMaxToolCalls = 20, 60
	b.WorkerTimeoutSeconds = 300
	b.MaxPlannerRounds, b.MaxWorkItems = 32, 48
	b.PlannerReservedModelCalls, b.MainReservedModelCalls = 24, 8
	if raw := strings.TrimSpace(os.Getenv("OMA_EXPLORATION_BUDGET")); raw != "" {
		if !strings.HasPrefix(raw, "{") {
			return b, fmt.Errorf("OMA_EXPLORATION_BUDGET must be a JSON object")
		}
		var override model.BudgetConfig
		decoder := json.NewDecoder(strings.NewReader(raw))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&override); err != nil {
			return b, fmt.Errorf("invalid OMA_EXPLORATION_BUDGET: %w", err)
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			return b, fmt.Errorf("OMA_EXPLORATION_BUDGET must contain one JSON object")
		}
		fields := reflect.ValueOf(override)
		for i := 0; i < fields.NumField(); i++ {
			if fields.Field(i).Int() < 0 {
				return b, fmt.Errorf("OMA_EXPLORATION_BUDGET fields must be non-negative")
			}
		}
		b = b.WithOverrides(&override)
	}
	if _, err := contract.AcceptWorkerCount(b.MaxParallelWorkers, contract.MaxParallelWorkers); err != nil {
		return b, err
	}
	return b, nil
}

// ManagedReserves keeps scheduling headroom proportional to the final grant.
// Explicit positive reserves are preserved by the create path afterwards.
func ManagedReserves(b model.BudgetConfig) model.BudgetConfig {
	b.PlannerReservedModelCalls = min(b.PlannerReservedModelCalls, b.MaxModelCalls/10)
	b.MainReservedModelCalls = min(b.MainReservedModelCalls, b.MaxModelCalls/30)
	return b
}
