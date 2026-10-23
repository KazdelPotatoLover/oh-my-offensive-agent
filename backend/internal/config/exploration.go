package config

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
)

// LegacyManagedBudget is the historical discovery grant (240/600/30 minutes).
// Discovery runs that do not opt into exploration keep this bounded snapshot so
// existing admission tests stay finite. New exploration runs use
// ResolveExplorationPolicy instead.
func LegacyManagedBudget() (model.BudgetConfig, error) {
	b := model.DefaultBudgetConfig()
	b.MaxParallelWorkers = contract.DefaultExplorationWorkers
	b.RunTimeoutSeconds = 1800
	b.MaxModelCalls, b.MaxToolCalls = 240, 600
	b.WorkerMaxModelCalls, b.WorkerMaxToolCalls = 20, 60
	b.WorkerTimeoutSeconds = 300
	b.MaxPlannerRounds, b.MaxWorkItems = 32, 48
	b.PlannerReservedModelCalls, b.MainReservedModelCalls = 24, 8
	override, err := envBudgetOverride()
	if err != nil {
		return b, err
	}
	b = b.WithOverrides(override)
	if _, err := contract.AcceptWorkerCount(b.MaxParallelWorkers, contract.MaxParallelWorkers); err != nil {
		return b, err
	}
	return b, nil
}

// ExplorationBudget is the compatible projection of the current v1 policy.
// It is resolved only when creating a new exploration run or answering the
// defaults API. Existing run snapshots stay authoritative.
func ExplorationBudget() (model.BudgetConfig, error) {
	policy, err := ResolveExplorationPolicy(model.BudgetConfig{}, nil, nil, nil)
	if err != nil {
		return model.BudgetConfig{}, err
	}
	return policy.ProjectBudget(), nil
}

// ResolveExplorationPolicy applies builtin → global → task → request.
// Positive BudgetConfig fields are explicit compatible limits. Pointer overrides
// may set a dimension back to unlimited. The result is frozen on the run.
func ResolveExplorationPolicy(taskBudget model.BudgetConfig, taskOverride *model.PolicyOverride, requestBudget *model.BudgetConfig, requestOverride *model.PolicyOverride) (model.ExecutionPolicy, error) {
	policy := model.BuiltinExplorationPolicy()
	globalBudget, err := envBudgetOverride()
	if err != nil {
		return policy, err
	}
	policy, err = policy.ApplyPositiveBudget(globalBudget)
	if err != nil {
		return policy, err
	}
	if raw := strings.TrimSpace(os.Getenv("OMA_AGENT_LIMITS")); raw != "" {
		var globalOverride model.PolicyOverride
		if err := json.Unmarshal([]byte(raw), &globalOverride); err != nil {
			return policy, fmt.Errorf("invalid OMA_AGENT_LIMITS: %w", err)
		}
		policy, err = policy.ApplyOverride(&globalOverride)
		if err != nil {
			return policy, err
		}
	}
	policy, err = policy.ApplyPositiveBudget(&taskBudget)
	if err != nil {
		return policy, err
	}
	policy, err = policy.ApplyOverride(taskOverride)
	if err != nil {
		return policy, err
	}
	policy, err = policy.ApplyPositiveBudget(requestBudget)
	if err != nil {
		return policy, err
	}
	policy, err = policy.ApplyOverride(requestOverride)
	if err != nil {
		return policy, err
	}
	if _, err := contract.AcceptWorkerCount(policy.MaxParallelWorkers, contract.MaxParallelWorkers); err != nil {
		return policy, err
	}
	return policy, nil
}

func envBudgetOverride() (*model.BudgetConfig, error) {
	raw := strings.TrimSpace(os.Getenv("OMA_EXPLORATION_BUDGET"))
	if raw == "" {
		return nil, nil
	}
	if !strings.HasPrefix(raw, "{") {
		return nil, fmt.Errorf("OMA_EXPLORATION_BUDGET must be a JSON object")
	}
	var override model.BudgetConfig
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&override); err != nil {
		return nil, fmt.Errorf("invalid OMA_EXPLORATION_BUDGET: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, fmt.Errorf("OMA_EXPLORATION_BUDGET must contain one JSON object")
	}
	if err := rejectNegativeBudgetFields(override); err != nil {
		return nil, fmt.Errorf("OMA_EXPLORATION_BUDGET fields must be non-negative")
	}
	return &override, nil
}

func rejectNegativeBudgetFields(b model.BudgetConfig) error {
	_, err := model.BuiltinExplorationPolicy().ApplyPositiveBudget(&b)
	return err
}

// ManagedReserves keeps scheduling headroom proportional to a bounded grant.
// Do not call it on an agent_limits.v1 projection: dividing an unlimited 0
// would invent a planner/main lifetime cap.
func ManagedReserves(b model.BudgetConfig) model.BudgetConfig {
	if b.MaxModelCalls <= 0 {
		return b
	}
	b.PlannerReservedModelCalls = min(b.PlannerReservedModelCalls, b.MaxModelCalls/10)
	b.MainReservedModelCalls = min(b.MainReservedModelCalls, b.MaxModelCalls/30)
	return b
}
