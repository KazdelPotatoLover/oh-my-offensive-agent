package model

import (
	"encoding/json"
	"fmt"
)

const AgentLimitsSchema = "agent_limits.v1"

// AgentLimit is one role's turn and duration contract. Under agent_limits.v1,
// 0 means that dimension has no cap.
type AgentLimit struct {
	MaxTurns           int `json:"max_turns"`
	RunTimeoutSeconds  int `json:"run_timeout_seconds"`
	SettlementMaxTurns int `json:"settlement_max_turns"`
}

// HardLimits are optional run-wide caps. 0 means the dimension is not capped.
// WorkerModelCalls and WorkerToolCalls are request caps, including retries.
// They are not MaxTurns.
type HardLimits struct {
	ModelCalls       int `json:"model_calls"`
	ToolCalls        int `json:"tool_calls"`
	PlannerRounds    int `json:"planner_rounds"`
	WorkItems        int `json:"work_items"`
	WorkerModelCalls int `json:"worker_model_calls"`
	WorkerToolCalls  int `json:"worker_tool_calls"`
}

// LegacyReserves keeps an explicit planner/main model-call cap when a task
// already set one. 0 means v1 does not impose a lifetime call cap.
type LegacyReserves struct {
	PlannerModelCalls int `json:"planner_model_calls"`
	MainModelCalls    int `json:"main_model_calls"`
}

// ExecutionPolicy is the frozen authority for a run created with agent_limits.v1.
// Compatible BudgetConfig fields are a projection of this object.
type ExecutionPolicy struct {
	SchemaVersion                  string         `json:"schema_version"`
	TaskTimeoutSeconds             int            `json:"task_timeout_seconds"`
	MaxParallelWorkers             int            `json:"max_parallel_workers"`
	Agents                         PolicyAgents   `json:"agents"`
	HardLimits                     HardLimits     `json:"hard_limits"`
	LegacyReserves                 LegacyReserves `json:"legacy_reserves"`
	ModelTimeoutSeconds            int            `json:"model_timeout_seconds"`
	ToolTimeoutSeconds             int            `json:"tool_timeout_seconds"`
	SummaryTimeoutSeconds          int            `json:"summary_timeout_seconds"`
	CleanupTimeoutSeconds          int            `json:"cleanup_timeout_seconds"`
	MaxToolOutputBytes             int            `json:"max_tool_output_bytes"`
	ContextWindowBytes             int            `json:"context_window_bytes"`
	MaxParallelModelCalls          int            `json:"max_parallel_model_calls"`
	WorkerSettlementTimeoutSeconds int            `json:"worker_settlement_timeout_seconds"`
}

type PolicyAgents struct {
	Worker    AgentLimit `json:"worker"`
	Planner   AgentLimit `json:"planner"`
	MainAgent AgentLimit `json:"mainagent"`
}

// PolicyOverride distinguishes an omitted field from an explicit 0.
// Nested agents/hard_limits objects are accepted so a stored policy can be
// replayed as an override. A present numeric field, including 0, is explicit.
type PolicyOverride struct {
	TaskTimeoutSeconds             *int `json:"task_timeout_seconds,omitempty"`
	MaxParallelWorkers             *int `json:"max_parallel_workers,omitempty"`
	MaxModelCalls                  *int `json:"max_model_calls,omitempty"`
	MaxToolCalls                   *int `json:"max_tool_calls,omitempty"`
	MaxPlannerRounds               *int `json:"max_planner_rounds,omitempty"`
	MaxWorkItems                   *int `json:"max_work_items,omitempty"`
	WorkerMaxTurns                 *int `json:"worker_max_turns,omitempty"`
	WorkerRunTimeoutSeconds        *int `json:"worker_run_timeout_seconds,omitempty"`
	WorkerSettlementMaxTurns       *int `json:"worker_settlement_max_turns,omitempty"`
	WorkerMaxModelCalls            *int `json:"worker_max_model_calls,omitempty"`
	WorkerMaxToolCalls             *int `json:"worker_max_tool_calls,omitempty"`
	PlannerMaxTurns                *int `json:"planner_max_turns,omitempty"`
	PlannerRunTimeoutSeconds       *int `json:"planner_run_timeout_seconds,omitempty"`
	PlannerSettlementMaxTurns      *int `json:"planner_settlement_max_turns,omitempty"`
	MainMaxTurns                   *int `json:"main_max_turns,omitempty"`
	MainRunTimeoutSeconds          *int `json:"main_run_timeout_seconds,omitempty"`
	MainSettlementMaxTurns         *int `json:"main_settlement_max_turns,omitempty"`
	PlannerReservedModelCalls      *int `json:"planner_reserved_model_calls,omitempty"`
	MainReservedModelCalls         *int `json:"main_reserved_model_calls,omitempty"`
	WorkerSettlementTimeoutSeconds *int `json:"worker_settlement_timeout_seconds,omitempty"`
	ModelTimeoutSeconds            *int `json:"model_timeout_seconds,omitempty"`
	ToolTimeoutSeconds             *int `json:"tool_timeout_seconds,omitempty"`
	SummaryTimeoutSeconds          *int `json:"summary_timeout_seconds,omitempty"`
	CleanupTimeoutSeconds          *int `json:"cleanup_timeout_seconds,omitempty"`
	MaxToolOutputBytes             *int `json:"max_tool_output_bytes,omitempty"`
	ContextWindowBytes             *int `json:"context_window_bytes,omitempty"`
	MaxParallelModelCalls          *int `json:"max_parallel_model_calls,omitempty"`
}

func (o *PolicyOverride) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	set := func(key string, dest **int) error {
		field, ok := raw[key]
		if !ok {
			return nil
		}
		if string(field) == "null" {
			return nil
		}
		var n int
		if err := json.Unmarshal(field, &n); err != nil {
			return fmt.Errorf("%s: %w", key, err)
		}
		*dest = &n
		return nil
	}
	if err := set("task_timeout_seconds", &o.TaskTimeoutSeconds); err != nil {
		return err
	}
	if err := set("max_parallel_workers", &o.MaxParallelWorkers); err != nil {
		return err
	}
	if err := set("max_model_calls", &o.MaxModelCalls); err != nil {
		return err
	}
	if err := set("max_tool_calls", &o.MaxToolCalls); err != nil {
		return err
	}
	if err := set("max_planner_rounds", &o.MaxPlannerRounds); err != nil {
		return err
	}
	if err := set("max_work_items", &o.MaxWorkItems); err != nil {
		return err
	}
	if err := set("worker_max_turns", &o.WorkerMaxTurns); err != nil {
		return err
	}
	if err := set("worker_run_timeout_seconds", &o.WorkerRunTimeoutSeconds); err != nil {
		return err
	}
	if err := set("worker_settlement_max_turns", &o.WorkerSettlementMaxTurns); err != nil {
		return err
	}
	if err := set("worker_max_model_calls", &o.WorkerMaxModelCalls); err != nil {
		return err
	}
	if err := set("worker_max_tool_calls", &o.WorkerMaxToolCalls); err != nil {
		return err
	}
	if err := set("planner_max_turns", &o.PlannerMaxTurns); err != nil {
		return err
	}
	if err := set("planner_run_timeout_seconds", &o.PlannerRunTimeoutSeconds); err != nil {
		return err
	}
	if err := set("planner_settlement_max_turns", &o.PlannerSettlementMaxTurns); err != nil {
		return err
	}
	if err := set("main_max_turns", &o.MainMaxTurns); err != nil {
		return err
	}
	if err := set("main_run_timeout_seconds", &o.MainRunTimeoutSeconds); err != nil {
		return err
	}
	if err := set("main_settlement_max_turns", &o.MainSettlementMaxTurns); err != nil {
		return err
	}
	if err := set("planner_reserved_model_calls", &o.PlannerReservedModelCalls); err != nil {
		return err
	}
	if err := set("main_reserved_model_calls", &o.MainReservedModelCalls); err != nil {
		return err
	}
	if err := set("worker_settlement_timeout_seconds", &o.WorkerSettlementTimeoutSeconds); err != nil {
		return err
	}
	if err := set("model_timeout_seconds", &o.ModelTimeoutSeconds); err != nil {
		return err
	}
	if err := set("tool_timeout_seconds", &o.ToolTimeoutSeconds); err != nil {
		return err
	}
	if err := set("summary_timeout_seconds", &o.SummaryTimeoutSeconds); err != nil {
		return err
	}
	if err := set("cleanup_timeout_seconds", &o.CleanupTimeoutSeconds); err != nil {
		return err
	}
	if err := set("max_tool_output_bytes", &o.MaxToolOutputBytes); err != nil {
		return err
	}
	if err := set("context_window_bytes", &o.ContextWindowBytes); err != nil {
		return err
	}
	if err := set("max_parallel_model_calls", &o.MaxParallelModelCalls); err != nil {
		return err
	}
	if agents, ok := raw["agents"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(agents, &nested); err != nil {
			return fmt.Errorf("agents: %w", err)
		}
		if err := applyNestedAgent(nested["worker"], &o.WorkerMaxTurns, &o.WorkerRunTimeoutSeconds, &o.WorkerSettlementMaxTurns); err != nil {
			return fmt.Errorf("agents.worker: %w", err)
		}
		if err := applyNestedAgent(nested["planner"], &o.PlannerMaxTurns, &o.PlannerRunTimeoutSeconds, &o.PlannerSettlementMaxTurns); err != nil {
			return fmt.Errorf("agents.planner: %w", err)
		}
		if err := applyNestedAgent(nested["mainagent"], &o.MainMaxTurns, &o.MainRunTimeoutSeconds, &o.MainSettlementMaxTurns); err != nil {
			return fmt.Errorf("agents.mainagent: %w", err)
		}
	}
	if hard, ok := raw["hard_limits"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(hard, &nested); err != nil {
			return fmt.Errorf("hard_limits: %w", err)
		}
		assign := func(key string, dest **int) error {
			field, ok := nested[key]
			if !ok || string(field) == "null" {
				return nil
			}
			var n int
			if err := json.Unmarshal(field, &n); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dest = &n
			return nil
		}
		if err := assign("model_calls", &o.MaxModelCalls); err != nil {
			return err
		}
		if err := assign("tool_calls", &o.MaxToolCalls); err != nil {
			return err
		}
		if err := assign("planner_rounds", &o.MaxPlannerRounds); err != nil {
			return err
		}
		if err := assign("work_items", &o.MaxWorkItems); err != nil {
			return err
		}
		if err := assign("worker_model_calls", &o.WorkerMaxModelCalls); err != nil {
			return err
		}
		if err := assign("worker_tool_calls", &o.WorkerMaxToolCalls); err != nil {
			return err
		}
	}
	if reserves, ok := raw["legacy_reserves"]; ok {
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(reserves, &nested); err != nil {
			return fmt.Errorf("legacy_reserves: %w", err)
		}
		assign := func(key string, dest **int) error {
			field, ok := nested[key]
			if !ok || string(field) == "null" {
				return nil
			}
			var n int
			if err := json.Unmarshal(field, &n); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
			*dest = &n
			return nil
		}
		if err := assign("planner_model_calls", &o.PlannerReservedModelCalls); err != nil {
			return err
		}
		if err := assign("main_model_calls", &o.MainReservedModelCalls); err != nil {
			return err
		}
	}
	return nil
}

func applyNestedAgent(raw json.RawMessage, turns, timeout, settlement **int) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return err
	}
	read := func(key string, dest **int) error {
		field, ok := fields[key]
		if !ok || string(field) == "null" {
			return nil
		}
		var n int
		if err := json.Unmarshal(field, &n); err != nil {
			return err
		}
		*dest = &n
		return nil
	}
	if err := read("max_turns", turns); err != nil {
		return err
	}
	if err := read("run_timeout_seconds", timeout); err != nil {
		return err
	}
	return read("settlement_max_turns", settlement)
}

// BuiltinExplorationPolicy is the v1 default before global, task, and request layers.
// Worker count 3 matches contract.DefaultExplorationWorkers. The model package
// does not import contract.
func BuiltinExplorationPolicy() ExecutionPolicy {
	protection := DefaultBudgetConfig()
	return ExecutionPolicy{
		SchemaVersion:      AgentLimitsSchema,
		TaskTimeoutSeconds: 0,
		MaxParallelWorkers: 3,
		Agents: PolicyAgents{
			Worker:    AgentLimit{MaxTurns: 0, RunTimeoutSeconds: 1200, SettlementMaxTurns: 10},
			Planner:   AgentLimit{MaxTurns: 0, RunTimeoutSeconds: 0, SettlementMaxTurns: 10},
			MainAgent: AgentLimit{MaxTurns: 0, RunTimeoutSeconds: 0, SettlementMaxTurns: 10},
		},
		ModelTimeoutSeconds:            protection.ModelTimeoutSeconds,
		ToolTimeoutSeconds:             protection.ToolTimeoutSeconds,
		SummaryTimeoutSeconds:          protection.SummaryTimeoutSeconds,
		CleanupTimeoutSeconds:          protection.CleanupTimeoutSeconds,
		MaxToolOutputBytes:             protection.MaxToolOutputBytes,
		ContextWindowBytes:             protection.ContextWindowBytes,
		MaxParallelModelCalls:          protection.MaxParallelModelCalls,
		WorkerSettlementTimeoutSeconds: 0,
	}
}

func (p ExecutionPolicy) IsV1() bool {
	return p.SchemaVersion == AgentLimitsSchema
}

func (p ExecutionPolicy) ModelLimited() bool  { return p.HardLimits.ModelCalls > 0 }
func (p ExecutionPolicy) ToolLimited() bool   { return p.HardLimits.ToolCalls > 0 }
func (p ExecutionPolicy) HasDeadline() bool   { return p.TaskTimeoutSeconds > 0 }
func (p ExecutionPolicy) PlannerRoundsLimited() bool {
	return p.HardLimits.PlannerRounds > 0
}
func (p ExecutionPolicy) WorkItemsLimited() bool { return p.HardLimits.WorkItems > 0 }

// ProjectBudget writes a compatible snapshot. Unlimited dimensions stay 0.
// Readers must use the policy, not re-apply DefaultBudgetConfig fallbacks.
func (p ExecutionPolicy) ProjectBudget() BudgetConfig {
	b := DefaultBudgetConfig()
	b.MaxParallelWorkers = p.MaxParallelWorkers
	b.RunTimeoutSeconds = p.TaskTimeoutSeconds
	b.MaxModelCalls = p.HardLimits.ModelCalls
	b.MaxToolCalls = p.HardLimits.ToolCalls
	b.MaxPlannerRounds = p.HardLimits.PlannerRounds
	b.MaxWorkItems = p.HardLimits.WorkItems
	b.WorkerMaxModelCalls = p.HardLimits.WorkerModelCalls
	b.WorkerMaxToolCalls = p.HardLimits.WorkerToolCalls
	b.WorkerTimeoutSeconds = p.Agents.Worker.RunTimeoutSeconds
	b.WorkerSettlementTimeoutSeconds = p.WorkerSettlementTimeoutSeconds
	b.PlannerReservedModelCalls = p.LegacyReserves.PlannerModelCalls
	b.MainReservedModelCalls = p.LegacyReserves.MainModelCalls
	if p.ModelTimeoutSeconds > 0 {
		b.ModelTimeoutSeconds = p.ModelTimeoutSeconds
	}
	if p.ToolTimeoutSeconds > 0 {
		b.ToolTimeoutSeconds = p.ToolTimeoutSeconds
	}
	if p.SummaryTimeoutSeconds > 0 {
		b.SummaryTimeoutSeconds = p.SummaryTimeoutSeconds
	}
	if p.CleanupTimeoutSeconds > 0 {
		b.CleanupTimeoutSeconds = p.CleanupTimeoutSeconds
	}
	if p.MaxToolOutputBytes > 0 {
		b.MaxToolOutputBytes = p.MaxToolOutputBytes
	}
	if p.ContextWindowBytes > 0 {
		b.ContextWindowBytes = p.ContextWindowBytes
	}
	if p.MaxParallelModelCalls > 0 {
		b.MaxParallelModelCalls = p.MaxParallelModelCalls
	}
	return b
}

// ApplyPositiveBudget maps omitted-zero BudgetConfig fields onto the policy.
// A zero field is inherit. A positive field is an explicit compatible limit.
// worker_max_model_calls becomes a request cap, not MaxTurns.
func (p ExecutionPolicy) ApplyPositiveBudget(b *BudgetConfig) (ExecutionPolicy, error) {
	if b == nil {
		return p, nil
	}
	if err := rejectNegativeBudget(b); err != nil {
		return p, err
	}
	if b.MaxParallelWorkers > 0 {
		p.MaxParallelWorkers = b.MaxParallelWorkers
	}
	if b.RunTimeoutSeconds > 0 {
		p.TaskTimeoutSeconds = b.RunTimeoutSeconds
	}
	if b.MaxModelCalls > 0 {
		p.HardLimits.ModelCalls = b.MaxModelCalls
	}
	if b.MaxToolCalls > 0 {
		p.HardLimits.ToolCalls = b.MaxToolCalls
	}
	if b.MaxPlannerRounds > 0 {
		p.HardLimits.PlannerRounds = b.MaxPlannerRounds
	}
	if b.MaxWorkItems > 0 {
		p.HardLimits.WorkItems = b.MaxWorkItems
	}
	if b.WorkerMaxModelCalls > 0 {
		p.HardLimits.WorkerModelCalls = b.WorkerMaxModelCalls
	}
	if b.WorkerMaxToolCalls > 0 {
		p.HardLimits.WorkerToolCalls = b.WorkerMaxToolCalls
	}
	if b.WorkerTimeoutSeconds > 0 {
		p.Agents.Worker.RunTimeoutSeconds = b.WorkerTimeoutSeconds
	}
	if b.WorkerSettlementTimeoutSeconds > 0 {
		p.WorkerSettlementTimeoutSeconds = b.WorkerSettlementTimeoutSeconds
	}
	if b.PlannerReservedModelCalls > 0 {
		p.LegacyReserves.PlannerModelCalls = b.PlannerReservedModelCalls
	}
	if b.MainReservedModelCalls > 0 {
		p.LegacyReserves.MainModelCalls = b.MainReservedModelCalls
	}
	if b.ModelTimeoutSeconds > 0 {
		p.ModelTimeoutSeconds = b.ModelTimeoutSeconds
	}
	if b.ToolTimeoutSeconds > 0 {
		p.ToolTimeoutSeconds = b.ToolTimeoutSeconds
	}
	if b.SummaryTimeoutSeconds > 0 {
		p.SummaryTimeoutSeconds = b.SummaryTimeoutSeconds
	}
	if b.CleanupTimeoutSeconds > 0 {
		p.CleanupTimeoutSeconds = b.CleanupTimeoutSeconds
	}
	if b.MaxToolOutputBytes > 0 {
		p.MaxToolOutputBytes = b.MaxToolOutputBytes
	}
	if b.ContextWindowBytes > 0 {
		p.ContextWindowBytes = b.ContextWindowBytes
	}
	if b.MaxParallelModelCalls > 0 {
		p.MaxParallelModelCalls = b.MaxParallelModelCalls
	}
	return p, nil
}

func rejectNegativeBudget(b *BudgetConfig) error {
	values := []int{
		b.MaxModelCalls, b.MaxToolCalls, b.RunTimeoutSeconds, b.ModelTimeoutSeconds,
		b.ToolTimeoutSeconds, b.SummaryTimeoutSeconds, b.CleanupTimeoutSeconds,
		b.MaxToolOutputBytes, b.ContextWindowBytes, b.MaxPlannerRounds, b.MaxWorkItems,
		b.MaxParallelWorkers, b.WorkerMaxModelCalls, b.WorkerMaxToolCalls, b.WorkerTimeoutSeconds,
		b.WorkerSettlementTimeoutSeconds, b.MaxParallelModelCalls, b.PlannerReservedModelCalls,
		b.MainReservedModelCalls,
	}
	for _, n := range values {
		if n < 0 {
			return fmt.Errorf("budget fields must be non-negative")
		}
	}
	return nil
}

// ApplyOverride applies explicit pointer fields. 0 means unlimited for a limit
// dimension. Worker count cannot be 0.
func (p ExecutionPolicy) ApplyOverride(o *PolicyOverride) (ExecutionPolicy, error) {
	if o == nil {
		return p, nil
	}
	setNonNeg := func(name string, src *int, dest *int) error {
		if src == nil {
			return nil
		}
		if *src < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
		*dest = *src
		return nil
	}
	if err := setNonNeg("task_timeout_seconds", o.TaskTimeoutSeconds, &p.TaskTimeoutSeconds); err != nil {
		return p, err
	}
	if o.MaxParallelWorkers != nil {
		if *o.MaxParallelWorkers < 1 || *o.MaxParallelWorkers > 8 {
			return p, fmt.Errorf("max_parallel_workers must be 1-8")
		}
		p.MaxParallelWorkers = *o.MaxParallelWorkers
	}
	if err := setNonNeg("max_model_calls", o.MaxModelCalls, &p.HardLimits.ModelCalls); err != nil {
		return p, err
	}
	if err := setNonNeg("max_tool_calls", o.MaxToolCalls, &p.HardLimits.ToolCalls); err != nil {
		return p, err
	}
	if err := setNonNeg("max_planner_rounds", o.MaxPlannerRounds, &p.HardLimits.PlannerRounds); err != nil {
		return p, err
	}
	if err := setNonNeg("max_work_items", o.MaxWorkItems, &p.HardLimits.WorkItems); err != nil {
		return p, err
	}
	if err := setNonNeg("worker_max_model_calls", o.WorkerMaxModelCalls, &p.HardLimits.WorkerModelCalls); err != nil {
		return p, err
	}
	if err := setNonNeg("worker_max_tool_calls", o.WorkerMaxToolCalls, &p.HardLimits.WorkerToolCalls); err != nil {
		return p, err
	}
	if err := setNonNeg("worker_max_turns", o.WorkerMaxTurns, &p.Agents.Worker.MaxTurns); err != nil {
		return p, err
	}
	if err := setNonNeg("worker_run_timeout_seconds", o.WorkerRunTimeoutSeconds, &p.Agents.Worker.RunTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("worker_settlement_max_turns", o.WorkerSettlementMaxTurns, &p.Agents.Worker.SettlementMaxTurns); err != nil {
		return p, err
	}
	if err := setNonNeg("planner_max_turns", o.PlannerMaxTurns, &p.Agents.Planner.MaxTurns); err != nil {
		return p, err
	}
	if err := setNonNeg("planner_run_timeout_seconds", o.PlannerRunTimeoutSeconds, &p.Agents.Planner.RunTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("planner_settlement_max_turns", o.PlannerSettlementMaxTurns, &p.Agents.Planner.SettlementMaxTurns); err != nil {
		return p, err
	}
	if err := setNonNeg("main_max_turns", o.MainMaxTurns, &p.Agents.MainAgent.MaxTurns); err != nil {
		return p, err
	}
	if err := setNonNeg("main_run_timeout_seconds", o.MainRunTimeoutSeconds, &p.Agents.MainAgent.RunTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("main_settlement_max_turns", o.MainSettlementMaxTurns, &p.Agents.MainAgent.SettlementMaxTurns); err != nil {
		return p, err
	}
	if err := setNonNeg("planner_reserved_model_calls", o.PlannerReservedModelCalls, &p.LegacyReserves.PlannerModelCalls); err != nil {
		return p, err
	}
	if err := setNonNeg("main_reserved_model_calls", o.MainReservedModelCalls, &p.LegacyReserves.MainModelCalls); err != nil {
		return p, err
	}
	if err := setNonNeg("worker_settlement_timeout_seconds", o.WorkerSettlementTimeoutSeconds, &p.WorkerSettlementTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("model_timeout_seconds", o.ModelTimeoutSeconds, &p.ModelTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("tool_timeout_seconds", o.ToolTimeoutSeconds, &p.ToolTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("summary_timeout_seconds", o.SummaryTimeoutSeconds, &p.SummaryTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("cleanup_timeout_seconds", o.CleanupTimeoutSeconds, &p.CleanupTimeoutSeconds); err != nil {
		return p, err
	}
	if err := setNonNeg("max_tool_output_bytes", o.MaxToolOutputBytes, &p.MaxToolOutputBytes); err != nil {
		return p, err
	}
	if err := setNonNeg("context_window_bytes", o.ContextWindowBytes, &p.ContextWindowBytes); err != nil {
		return p, err
	}
	if err := setNonNeg("max_parallel_model_calls", o.MaxParallelModelCalls, &p.MaxParallelModelCalls); err != nil {
		return p, err
	}
	if p.ModelTimeoutSeconds <= 0 || p.ToolTimeoutSeconds <= 0 || p.SummaryTimeoutSeconds <= 0 || p.CleanupTimeoutSeconds <= 0 {
		return p, fmt.Errorf("protection timeouts must stay positive")
	}
	return p, nil
}

// PolicyFromExecutionInput returns the frozen v1 policy, or nil when the run
// predates the schema. A missing policy must keep legacy zero-as-fallback rules.
func PolicyFromExecutionInput(raw json.RawMessage) *ExecutionPolicy {
	if len(raw) == 0 {
		return nil
	}
	var wrapped struct {
		ExecutionPolicy *ExecutionPolicy `json:"execution_policy"`
	}
	if json.Unmarshal(raw, &wrapped) != nil || wrapped.ExecutionPolicy == nil {
		return nil
	}
	if wrapped.ExecutionPolicy.SchemaVersion != AgentLimitsSchema {
		return nil
	}
	return wrapped.ExecutionPolicy
}

// PublicLimitView is the defaults API shape. It never includes credentials.
type PublicLimitView struct {
	SchemaVersion           string `json:"schema_version"`
	TaskTimeoutLimited      bool   `json:"task_timeout_limited"`
	TaskTimeoutSeconds      int    `json:"task_timeout_seconds"`
	ModelCallsLimited       bool   `json:"model_calls_limited"`
	ToolCallsLimited        bool   `json:"tool_calls_limited"`
	PlannerRoundsLimited    bool   `json:"planner_rounds_limited"`
	WorkItemsLimited        bool   `json:"work_items_limited"`
	WorkerTurnsLimited      bool   `json:"worker_turns_limited"`
	WorkerRunTimeoutSeconds int    `json:"worker_run_timeout_seconds"`
	MaxParallelWorkers      int    `json:"max_parallel_workers"`
}

func (p ExecutionPolicy) PublicLimits() PublicLimitView {
	return PublicLimitView{
		SchemaVersion:           p.SchemaVersion,
		TaskTimeoutLimited:      p.HasDeadline(),
		TaskTimeoutSeconds:      p.TaskTimeoutSeconds,
		ModelCallsLimited:       p.ModelLimited(),
		ToolCallsLimited:        p.ToolLimited(),
		PlannerRoundsLimited:    p.PlannerRoundsLimited(),
		WorkItemsLimited:        p.WorkItemsLimited(),
		WorkerTurnsLimited:      p.Agents.Worker.MaxTurns > 0,
		WorkerRunTimeoutSeconds: p.Agents.Worker.RunTimeoutSeconds,
		MaxParallelWorkers:      p.MaxParallelWorkers,
	}
}
