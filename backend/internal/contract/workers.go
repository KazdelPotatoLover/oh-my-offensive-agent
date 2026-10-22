package contract

import "fmt"

const (
	MinParallelWorkers         = 1
	MaxParallelWorkers         = 8
	DefaultExplorationWorkers  = 3
	LegacyMissingWorkerDefault = 1
)

// WorkerCapacity separates the accepted request from the live pool. Effective
// equals Requested after a successful create. Origin pacing must not be encoded
// by lowering Effective.
type WorkerCapacity struct {
	Requested int `json:"requested"`
	Effective int `json:"effective"`
	Active    int `json:"active"`
	Queued    int `json:"queued"`
}

// ModelCapacity is the shared upstream request limit. It is not the worker pool.
type ModelCapacity struct {
	Limit    int `json:"limit"`
	InFlight int `json:"in_flight"`
	Queued   int `json:"queued"`
}

// OriginPacing is a per-origin network wait. It does not change worker capacity.
type OriginPacing struct {
	Waiting bool   `json:"waiting"`
	Reason  string `json:"reason,omitempty"`
}

func (c WorkerCapacity) Validate() error {
	if c.Requested < 0 || c.Effective < 0 || c.Active < 0 || c.Queued < 0 {
		return invalidInput("worker capacity counts must be non-negative")
	}
	if c.Active > c.Effective && c.Effective > 0 {
		return invalidInput("active workers exceed effective capacity")
	}
	return nil
}

// AcceptWorkerCount returns the requested pool size or a concurrency error.
// It never rewrites a valid or out-of-range request into 1.
func AcceptWorkerCount(requested, serverMax int) (int, error) {
	if serverMax <= 0 || serverMax > MaxParallelWorkers {
		serverMax = MaxParallelWorkers
	}
	if requested < MinParallelWorkers || requested > MaxParallelWorkers || requested > serverMax {
		return 0, &Error{
			Code:            CodeConcurrencyOutOfRange,
			Scope:           ScopeRequest,
			Message:         fmt.Sprintf("max_parallel_workers must be %d-%d", MinParallelWorkers, serverMax),
			SuggestedAction: ActionNone,
		}
	}
	return requested, nil
}

// ResolveRequestedWorkers applies the create/snapshot rule.
// A missing field on an old snapshot stays at 1. A new exploration task with
// no field uses 3. A present value is accepted or rejected, never clamped.
func ResolveRequestedWorkers(fieldPresent bool, requested int, newExploration bool, serverMax int) (int, error) {
	if !fieldPresent {
		if newExploration {
			return DefaultExplorationWorkers, nil
		}
		return LegacyMissingWorkerDefault, nil
	}
	return AcceptWorkerCount(requested, serverMax)
}

// DiscoveryMustHonorRequestedWorkers is the discovery start rule. Discovery
// mode is not a reason to force the pool to 1.
func DiscoveryMustHonorRequestedWorkers(requested, serverMax int) (int, error) {
	return AcceptWorkerCount(requested, serverMax)
}

// BudgetTemplate is an explicit initial grant. Applying it is a create-path
// decision owned by the runtime. These values are not an automatic renewal.
type BudgetTemplate struct {
	Name                      string `json:"name"`
	MaxModelCalls             int    `json:"max_model_calls"`
	MaxToolCalls              int    `json:"max_tool_calls"`
	RunTimeoutSeconds         int    `json:"run_timeout_seconds"`
	MaxParallelWorkers        int    `json:"max_parallel_workers"`
	WorkerMaxModelCalls       int    `json:"worker_max_model_calls"`
	WorkerMaxToolCalls        int    `json:"worker_max_tool_calls"`
	WorkerTimeoutSeconds      int    `json:"worker_timeout_seconds"`
	PlannerReservedModelCalls int    `json:"planner_reserved_model_calls"`
	MainReservedModelCalls    int    `json:"main_reserved_model_calls"`
}

// SuggestedExplorationTemplate is the starting exploration grant from the
// round plan. Agent 1/2 may propose a measured revision; they must not apply a
// silent increase inside an existing snapshot.
func SuggestedExplorationTemplate() BudgetTemplate {
	return BudgetTemplate{
		Name:                      "exploration",
		MaxModelCalls:             80,
		MaxToolCalls:              200,
		RunTimeoutSeconds:         600,
		MaxParallelWorkers:        DefaultExplorationWorkers,
		WorkerMaxModelCalls:       8,
		WorkerMaxToolCalls:        25,
		WorkerTimeoutSeconds:      120,
		PlannerReservedModelCalls: 16,
		MainReservedModelCalls:    8,
	}
}

// SuggestedQuickTemplate matches the historical small-run defaults as an
// explicit choice, not as a hidden discovery override.
func SuggestedQuickTemplate() BudgetTemplate {
	return BudgetTemplate{
		Name:                      "quick_verification",
		MaxModelCalls:             30,
		MaxToolCalls:              30,
		RunTimeoutSeconds:         120,
		MaxParallelWorkers:        1,
		WorkerMaxModelCalls:       5,
		WorkerMaxToolCalls:        10,
		WorkerTimeoutSeconds:      60,
		PlannerReservedModelCalls: 6,
		MainReservedModelCalls:    4,
	}
}

// WorkerModelCallsAdmittable leaves the planner and main reserves untouched.
func WorkerModelCallsAdmittable(remaining, plannerReserved, mainReserved int) int {
	keep := plannerReserved + mainReserved
	if remaining <= keep {
		return 0
	}
	return remaining - keep
}
