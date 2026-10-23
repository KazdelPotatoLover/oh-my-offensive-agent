package orchestration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/model"
)

var (
	ErrModelBudgetExhausted            = errors.New("model budget exhausted")
	ErrExplorationModelBudgetExhausted = errors.New("exploration model budget exhausted")
	ErrToolBudgetExhausted             = errors.New("tool budget exhausted")
	ErrInsufficientTime                = errors.New("insufficient time remaining for work item")
	ErrWorkerBudgetReserved            = errors.New("remaining model calls are reserved for Planner/Main")
)

// BudgetManager provides centralized, atomic budget tracking for model calls,
// tool calls, and wall-clock execution deadlines across Planner and all Workers.
type BudgetManager struct {
	mu          sync.Mutex
	cfg         model.BudgetConfig
	startTime   time.Time
	deadline    time.Time
	hasDeadline bool
	// modelLimited and toolLimited stay true for policy-less managers so a
	// stored 0 keeps the historical fallback. v1 sets them from the frozen policy.
	modelLimited bool
	toolLimited  bool

	// Model calls
	maxModelCalls       int
	reserveForWrapup    int
	maxExplorationCalls int
	consumedModelCalls  int32
	inflightModelCalls  int32

	// Tool calls
	maxToolCalls      int
	consumedToolCalls int32

	// Wrapup state
	isWrapupStarted atomic.Bool
}

// NewBudgetManager initializes a centralized budget manager from the budget config.
func NewBudgetManager(cfg model.BudgetConfig, startTime time.Time) *BudgetManager {
	maxModels := cfg.MaxModelCalls
	if maxModels <= 0 {
		maxModels = 30
	}
	maxTools := cfg.MaxToolCalls
	if maxTools <= 0 {
		maxTools = 30
	}

	// If maxModelCalls > 1, reserve 1 call for final wrap-up summary
	reserve := 0
	if maxModels > 1 {
		reserve = 1
	}
	maxExploration := maxModels - reserve

	bm := &BudgetManager{
		cfg:                 cfg,
		startTime:           startTime,
		maxModelCalls:       maxModels,
		reserveForWrapup:    reserve,
		maxExplorationCalls: maxExploration,
		maxToolCalls:        maxTools,
		modelLimited:        true,
		toolLimited:         true,
	}

	timeout := cfg.RunTimeoutSeconds
	if timeout <= 0 {
		timeout = 120
	}
	bm.deadline = startTime.Add(time.Duration(timeout) * time.Second)
	bm.hasDeadline = true

	return bm
}

// NewBudgetManagerFromPolicy honors agent_limits.v1. A zero cap is unlimited.
// It does not invent the legacy 30/30/120 fallbacks.
func NewBudgetManagerFromPolicy(policy model.ExecutionPolicy, cfg model.BudgetConfig, startTime time.Time) *BudgetManager {
	bm := &BudgetManager{
		cfg:          cfg,
		startTime:    startTime,
		modelLimited: policy.ModelLimited(),
		toolLimited:  policy.ToolLimited(),
		hasDeadline:  policy.HasDeadline(),
	}
	if bm.modelLimited {
		maxModels := cfg.MaxModelCalls
		if maxModels <= 0 {
			maxModels = policy.HardLimits.ModelCalls
		}
		reserve := 0
		if maxModels > 1 {
			reserve = 1
		}
		bm.maxModelCalls = maxModels
		bm.reserveForWrapup = reserve
		bm.maxExplorationCalls = maxModels - reserve
	}
	if bm.toolLimited {
		maxTools := cfg.MaxToolCalls
		if maxTools <= 0 {
			maxTools = policy.HardLimits.ToolCalls
		}
		bm.maxToolCalls = maxTools
	}
	if bm.hasDeadline {
		timeout := cfg.RunTimeoutSeconds
		if timeout <= 0 {
			timeout = policy.TaskTimeoutSeconds
		}
		bm.deadline = startTime.Add(time.Duration(timeout) * time.Second)
	}
	return bm
}

func budgetManagerFor(policy *model.ExecutionPolicy, cfg model.BudgetConfig, start time.Time) *BudgetManager {
	if policy != nil && policy.IsV1() {
		return NewBudgetManagerFromPolicy(*policy, cfg, start)
	}
	return NewBudgetManager(cfg, start)
}

func (b *BudgetManager) HasDeadline() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.hasDeadline
}

func (b *BudgetManager) HasModelLimit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.modelLimited
}

func (b *BudgetManager) HasToolLimit() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.toolLimited
}

// UpdateEffectiveBudget updates the budget limits and deadline dynamically while strictly preserving
// cumulative usage counters. Unlimited dimensions are not turned into a small remainder.
func (b *BudgetManager) UpdateEffectiveBudget(cfg model.BudgetConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg
	if b.modelLimited && cfg.MaxModelCalls > 0 {
		b.maxModelCalls = cfg.MaxModelCalls
		reserve := 0
		if b.maxModelCalls > 1 {
			reserve = 1
		}
		b.reserveForWrapup = reserve
		b.maxExplorationCalls = b.maxModelCalls - reserve
	}
	if b.toolLimited && cfg.MaxToolCalls > 0 {
		b.maxToolCalls = cfg.MaxToolCalls
	}
	if b.hasDeadline && cfg.RunTimeoutSeconds > 0 {
		b.deadline = b.startTime.Add(time.Duration(cfg.RunTimeoutSeconds) * time.Second)
	}
}

// restoreUsage is called once before session admission. Persisted requests from
// this Run must not disappear from its hard cap when an engine is constructed.
func (b *BudgetManager) restoreUsage(models, tools int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.consumedModelCalls = int32(models)
	b.consumedToolCalls = int32(tools)
}

// AcquireModelCall atomically requests a quota slot for a model call.
// Non-wrapup calls cannot consume the reserved wrap-up quota.
func (b *BudgetManager) AcquireModelCall(ctx context.Context, isWrapup bool) error {
	return b.acquireModelCall(ctx, isWrapup, 0)
}

func (b *BudgetManager) acquireModelCall(ctx context.Context, isWrapup bool, keep int) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	if b.hasDeadline && !time.Now().Before(b.deadline) {
		return context.DeadlineExceeded
	}
	if !isWrapup && b.isWrapupStarted.Load() {
		return ErrExplorationModelBudgetExhausted
	}

	if b.modelLimited {
		// Check total hard limit
		if int(b.consumedModelCalls) >= b.maxModelCalls {
			return ErrModelBudgetExhausted
		}

		// Check exploration budget
		if !isWrapup && int(b.consumedModelCalls) >= b.maxExplorationCalls {
			return ErrExplorationModelBudgetExhausted
		}
		if !isWrapup && b.maxModelCalls-int(b.consumedModelCalls) <= keep {
			return ErrWorkerBudgetReserved
		}
	}

	b.consumedModelCalls++
	b.inflightModelCalls++
	return nil
}

// WorkerModelCallsRemaining is an admission snapshot, not a grant. The actual
// reserve check and counter increment also happen under the same budget lock.
func (b *BudgetManager) WorkerModelCallsRemaining(planner, main int) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.modelLimited {
		return -1
	}
	keep := max(planner+main, b.reserveForWrapup)
	return max(0, b.maxModelCalls-int(b.consumedModelCalls)-keep)
}

// ReleaseModelCall decrements the in-flight model call count when a call returns.
func (b *BudgetManager) ReleaseModelCall() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.inflightModelCalls > 0 {
		b.inflightModelCalls--
	}
}

// AcquireToolCall atomically requests quota for a tool call.
func (b *BudgetManager) AcquireToolCall(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()

	if ctx != nil && ctx.Err() != nil {
		return ctx.Err()
	}

	if b.isWrapupStarted.Load() {
		return ErrToolBudgetExhausted
	}
	if b.hasDeadline && !time.Now().Before(b.deadline) {
		return context.DeadlineExceeded
	}
	if b.toolLimited && int(b.consumedToolCalls) >= b.maxToolCalls {
		return ErrToolBudgetExhausted
	}

	b.consumedToolCalls++
	return nil
}

// CanClaimWorkItem reports whether scheduling a new work item is permitted given
// current exploration budget, wrapup status, and remaining wall-clock time.
func (b *BudgetManager) CanClaimWorkItem(ctx context.Context) bool {
	b.mu.Lock()
	defer b.mu.Unlock()

	if (ctx != nil && ctx.Err() != nil) || b.isWrapupStarted.Load() {
		return false
	}

	// Exploration model quota reached -> stop claiming
	if b.modelLimited && int(b.consumedModelCalls) >= b.maxExplorationCalls {
		return false
	}

	// Tool quota reached -> stop claiming
	if b.toolLimited && int(b.consumedToolCalls) >= b.maxToolCalls {
		return false
	}

	// Wall-clock check
	if b.hasDeadline {
		remaining := time.Until(b.deadline)
		minRequired := b.minimumWorkRunway()
		if remaining < minRequired {
			return false
		}
	}

	return true
}

// RemainingTime returns the wall-clock duration until the run deadline.
func (b *BudgetManager) RemainingTime() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.hasDeadline {
		return time.Hour
	}
	return time.Until(b.deadline)
}

// InflightModelCalls returns the number of model calls currently executing.
func (b *BudgetManager) InflightModelCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.inflightModelCalls)
}

// ConsumedModelCalls returns the total model calls dispatched.
func (b *BudgetManager) ConsumedModelCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.consumedModelCalls)
}

// ConsumedToolCalls returns the total tool calls dispatched.
func (b *BudgetManager) ConsumedToolCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return int(b.consumedToolCalls)
}

func (b *BudgetManager) RemainingModelCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.modelLimited {
		return -1
	}
	remaining := b.maxModelCalls - int(b.consumedModelCalls)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (b *BudgetManager) RemainingExplorationModelCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.modelLimited {
		return -1
	}
	remaining := b.maxExplorationCalls - int(b.consumedModelCalls)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (b *BudgetManager) RemainingToolCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.toolLimited {
		return -1
	}
	remaining := b.maxToolCalls - int(b.consumedToolCalls)
	if remaining < 0 {
		return 0
	}
	return remaining
}

// HasWrapupQuota reports whether the reserved wrap-up model call quota is available.
func (b *BudgetManager) HasWrapupQuota() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.modelLimited {
		return true
	}
	return int(b.consumedModelCalls) < b.maxModelCalls
}

// MarkWrapupStarted marks the transition into the wrap-up phase.
func (b *BudgetManager) MarkWrapupStarted() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.isWrapupStarted.Store(true)
}

// IsWrapupStarted reports whether wrap-up has begun.
func (b *BudgetManager) IsWrapupStarted() bool {
	return b.isWrapupStarted.Load()
}

// ExhaustionCause explains why scheduling stopped, using the same time threshold
// as CanClaimWorkItem rather than misclassifying a short runway as model exhaustion.
func (b *BudgetManager) ExhaustionCause() model.TerminationCause {
	b.mu.Lock()
	defer b.mu.Unlock()
	minimum := b.minimumWorkRunway()
	if b.hasDeadline && time.Until(b.deadline) < minimum {
		return model.CauseTimeout
	}
	if b.toolLimited && int(b.consumedToolCalls) >= b.maxToolCalls {
		return model.CauseToolBudgetExhausted
	}
	if b.modelLimited && int(b.consumedModelCalls) >= b.maxExplorationCalls {
		return model.CauseModelBudgetExhausted
	}
	if !b.modelLimited && !b.toolLimited && !b.hasDeadline {
		return model.CauseNeedsInput
	}
	return model.CauseModelBudgetExhausted
}

// Very short, explicitly configured runs still admit useful work. Keep the
// admission margin within ten percent of their full runtime allowance.
func (b *BudgetManager) minimumWorkRunway() time.Duration {
	margin := time.Duration(b.cfg.ToolTimeoutSeconds) * time.Second
	if margin < 3*time.Second {
		margin = 3 * time.Second
	}
	if b.cfg.RunTimeoutSeconds > 0 {
		fraction := time.Duration(b.cfg.RunTimeoutSeconds) * time.Second / 10
		if fraction < margin {
			margin = fraction
		}
	}
	return margin
}

// WorkerBudgetGate enforces dual-layer budget constraints:
// Run-level hard limits via BudgetManager + Worker-local per-attempt limits and settlement.
type WorkerBudgetGate struct {
	mu                      sync.Mutex
	bm                      *BudgetManager
	cfg                     model.BudgetConfig
	startTime               time.Time
	localTimeout            time.Duration
	localDeadline           time.Time
	workerMaxModels         int
	workerMaxTools          int
	localModelCalls         int
	localToolCalls          int
	settlementModelsAllowed int
	settlementModelsUsed    int
	inSettlement            atomic.Bool
	limiter                 *ModelLimiter
	plannerReserve          int
	mainReserve             int
	policyV1                bool
	modelCapEnabled         bool
	toolCapEnabled          bool
	localDeadlineEnabled    bool
	turnsEnabled            bool
	maxLogicalTurns         int
	logicalTurns            int
	settlementTurnsEnabled  bool
	settlementMaxTurns      int
	settlementTurns         int
	settlementClockEnabled  bool
	settlementClock         time.Duration
}

func (w *WorkerBudgetGate) UseModelLimiter(limiter *ModelLimiter) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.limiter = limiter
	w.mu.Unlock()
}

func (w *WorkerBudgetGate) SetReserves(planner, main int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.plannerReserve = planner
	w.mainReserve = main
	w.mu.Unlock()
}

func NewWorkerBudgetGate(bm *BudgetManager, cfg model.BudgetConfig) *WorkerBudgetGate {
	maxModels := cfg.WorkerMaxModelCalls
	if maxModels <= 0 {
		maxModels = 5
	}
	maxTools := cfg.WorkerMaxToolCalls
	if maxTools <= 0 {
		maxTools = 10
	}
	timeoutSec := cfg.WorkerTimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = 60
	}
	now := time.Now()
	timeoutDur := time.Duration(timeoutSec) * time.Second
	return &WorkerBudgetGate{
		bm:                      bm,
		cfg:                     cfg,
		startTime:               now,
		localTimeout:            timeoutDur,
		localDeadline:           now.Add(timeoutDur),
		localDeadlineEnabled:    true,
		workerMaxModels:         maxModels,
		workerMaxTools:          maxTools,
		modelCapEnabled:         true,
		toolCapEnabled:          true,
		settlementModelsAllowed: 1, // 1 model call for settlement wrapup
	}
}

// NewWorkerBudgetGateForPolicy applies v1 turn, duration, and request caps.
// cfg is the worker budget after profile tightening, so a positive profile cap
// still bounds an otherwise unlimited worker dimension.
func NewWorkerBudgetGateForPolicy(bm *BudgetManager, cfg model.BudgetConfig, policy model.ExecutionPolicy) *WorkerBudgetGate {
	now := time.Now()
	gate := &WorkerBudgetGate{
		bm:       bm,
		cfg:      cfg,
		policyV1: true,
		startTime: now,
	}
	if cfg.WorkerMaxModelCalls > 0 {
		gate.modelCapEnabled = true
		gate.workerMaxModels = cfg.WorkerMaxModelCalls
	}
	if cfg.WorkerMaxToolCalls > 0 {
		gate.toolCapEnabled = true
		gate.workerMaxTools = cfg.WorkerMaxToolCalls
	}
	timeout := policy.Agents.Worker.RunTimeoutSeconds
	if cfg.WorkerTimeoutSeconds > 0 && (timeout <= 0 || cfg.WorkerTimeoutSeconds < timeout) {
		timeout = cfg.WorkerTimeoutSeconds
	}
	if timeout > 0 {
		gate.localDeadlineEnabled = true
		gate.localTimeout = time.Duration(timeout) * time.Second
		gate.localDeadline = now.Add(gate.localTimeout)
	}
	if policy.Agents.Worker.MaxTurns > 0 {
		gate.turnsEnabled = true
		gate.maxLogicalTurns = policy.Agents.Worker.MaxTurns
	}
	if policy.Agents.Worker.SettlementMaxTurns > 0 {
		gate.settlementTurnsEnabled = true
		gate.settlementMaxTurns = policy.Agents.Worker.SettlementMaxTurns
	}
	if cfg.WorkerSettlementTimeoutSeconds > 0 {
		gate.settlementClockEnabled = true
		gate.settlementClock = time.Duration(cfg.WorkerSettlementTimeoutSeconds) * time.Second
	} else if policy.WorkerSettlementTimeoutSeconds > 0 {
		gate.settlementClockEnabled = true
		gate.settlementClock = time.Duration(policy.WorkerSettlementTimeoutSeconds) * time.Second
	}
	return gate
}

func (w *WorkerBudgetGate) NoteLogicalTurn(inSettlement bool) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if inSettlement || w.inSettlement.Load() {
		w.settlementTurns++
		return
	}
	w.logicalTurns++
}

func (w *WorkerBudgetGate) enterSettlementLocked() {
	if w.inSettlement.Load() {
		return
	}
	w.inSettlement.Store(true)
	if !w.policyV1 {
		settleSec := w.cfg.WorkerSettlementTimeoutSeconds
		if settleSec <= 0 {
			settleSec = 15
		}
		w.localDeadlineEnabled = true
		w.localDeadline = time.Now().Add(time.Duration(settleSec) * time.Second)
		return
	}
	if w.settlementClockEnabled {
		w.localDeadlineEnabled = true
		w.localDeadline = time.Now().Add(w.settlementClock)
		return
	}
	w.localDeadlineEnabled = false
}

func (w *WorkerBudgetGate) TryAcquire() bool {
	res := w.TryAcquireModel(context.Background(), w.inSettlement.Load())
	return res.Allowed
}

func (w *WorkerBudgetGate) TryAcquireModel(ctx context.Context, isSettlement bool) agent.BudgetGateResult {
	w.mu.Lock()
	limiter := w.limiter
	w.mu.Unlock()
	held := false
	if limiter != nil {
		if err := limiter.Acquire(ctx); err != nil {
			return agent.BudgetGateResult{
				Allowed: false,
				Scope:   agent.ScopeGlobal,
				Cause:   model.ResolveCancelCause(ctx),
			}
		}
		held = true
	}
	res := w.tryAcquireModelLocked(ctx, isSettlement)
	if held && !res.Allowed && limiter != nil {
		limiter.Release()
	}
	return res
}

func (w *WorkerBudgetGate) tryAcquireModelLocked(ctx context.Context, isSettlement bool) agent.BudgetGateResult {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 1. Check Global Run-level deadline
	if w.bm.HasDeadline() && w.bm.RemainingTime() <= 0 {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseTimeout,
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.ResolveCancelCause(ctx),
		}
	}

	// 2. Check Global Run-level model budget
	if w.bm.HasModelLimit() && w.bm.RemainingModelCalls() <= 0 {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseModelBudgetExhausted,
		}
	}

	// 3. Check Worker-local deadline
	if w.localDeadlineEnabled && time.Now().After(w.localDeadline) {
		if !w.inSettlement.Load() {
			w.enterSettlementLocked()
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerTimeout,
				IsSettlement: true,
			}
		}
		return agent.BudgetGateResult{
			Allowed:      false,
			Scope:        agent.ScopeWorkerLocal,
			Cause:        model.CauseWorkerTimeout,
			IsSettlement: false,
		}
	}

	// 4. Check Worker-local model call limits and logical turns
	if !w.inSettlement.Load() {
		if w.turnsEnabled && w.logicalTurns >= w.maxLogicalTurns {
			w.enterSettlementLocked()
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerModelBudgetExhausted,
				IsSettlement: true,
			}
		}
		if w.modelCapEnabled && w.localModelCalls >= w.workerMaxModels {
			w.enterSettlementLocked()
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerModelBudgetExhausted,
				IsSettlement: true,
			}
		}
	} else if w.policyV1 {
		if w.settlementTurnsEnabled && w.settlementTurns >= w.settlementMaxTurns {
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerSettlementComplete,
				IsSettlement: false,
			}
		}
	} else if w.settlementModelsUsed >= w.settlementModelsAllowed {
		return agent.BudgetGateResult{
			Allowed:      false,
			Scope:        agent.ScopeWorkerLocal,
			Cause:        model.CauseWorkerSettlementComplete,
			IsSettlement: false,
		}
	}

	// 5. Acquire from global budget
	if err := w.bm.acquireModelCall(ctx, isSettlement, w.plannerReserve+w.mainReserve); err != nil {
		if errors.Is(err, ErrWorkerBudgetReserved) {
			return agent.BudgetGateResult{Scope: agent.ScopeGlobal, Cause: model.CauseWorkerBudgetReserved}
		}
		if errors.Is(err, ErrModelBudgetExhausted) {
			return agent.BudgetGateResult{
				Allowed: false,
				Scope:   agent.ScopeGlobal,
				Cause:   model.CauseModelBudgetExhausted,
			}
		}
		if errors.Is(err, ErrExplorationModelBudgetExhausted) {
			return agent.BudgetGateResult{
				Allowed: false,
				Scope:   agent.ScopeGlobal,
				Cause:   model.CauseModelBudgetExhausted,
			}
		}
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseTimeout,
		}
	}

	if w.inSettlement.Load() {
		w.settlementModelsUsed++
	} else if w.modelCapEnabled {
		w.localModelCalls++
	}

	return agent.BudgetGateResult{
		Allowed:      true,
		Scope:        agent.ScopeWorkerLocal,
		IsSettlement: w.inSettlement.Load(),
	}
}

func (w *WorkerBudgetGate) TryAcquireTool(ctx context.Context, toolName string, isSettlement bool) agent.BudgetGateResult {
	w.mu.Lock()
	defer w.mu.Unlock()

	// 1. Check Global Run-level limits
	if w.bm.HasDeadline() && w.bm.RemainingTime() <= 0 {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseTimeout,
		}
	}
	if ctx != nil && ctx.Err() != nil {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.ResolveCancelCause(ctx),
		}
	}
	if w.bm.HasToolLimit() && w.bm.RemainingToolCalls() <= 0 {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseToolBudgetExhausted,
		}
	}

	// 2. Check settlement restrictions on exploratory tools
	if w.inSettlement.Load() {
		safe := false
		switch toolName {
		case "record_fact", "report_finding", "record_observation", "record_observation_batch", "read_observations", "list_facts", "node_detail", "graph_overview", "read_artifact", "get_worker_output", "list_artifacts", "write_artifact", "inspect_artifact", "list_worker_traces", "get_worker_trace", "search_all_worker_traces", "list_capability_profiles", "read_file", "inspect_command":
			safe = true
		}
		if !safe {
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerSettlementComplete,
				IsSettlement: true,
			}
		}
	}

	// 3. Check worker local tool calls
	if w.toolCapEnabled && w.localToolCalls >= w.workerMaxTools {
		if !w.inSettlement.Load() {
			w.enterSettlementLocked()
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerToolBudgetExhausted,
				IsSettlement: true,
			}
		}
		return agent.BudgetGateResult{
			Allowed:      false,
			Scope:        agent.ScopeWorkerLocal,
			Cause:        model.CauseWorkerToolBudgetExhausted,
			IsSettlement: false,
		}
	}

	// 4. Acquire from global budget
	if err := w.bm.AcquireToolCall(ctx); err != nil {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseToolBudgetExhausted,
		}
	}

	w.localToolCalls++
	return agent.BudgetGateResult{
		Allowed:      true,
		Scope:        agent.ScopeWorkerLocal,
		IsSettlement: w.inSettlement.Load(),
	}
}

func (b *BudgetManager) SetStartTime(started time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	previousStart := b.startTime
	previousDeadline := b.deadline
	b.startTime = started
	if !b.hasDeadline {
		return
	}
	timeout := b.cfg.RunTimeoutSeconds
	if timeout <= 0 && !previousDeadline.IsZero() {
		timeout = int(previousDeadline.Sub(previousStart) / time.Second)
	}
	if timeout <= 0 {
		timeout = 120
	}
	b.deadline = started.Add(time.Duration(timeout) * time.Second)
}
