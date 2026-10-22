package orchestration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
)

var (
	ErrModelBudgetExhausted            = errors.New("model budget exhausted")
	ErrExplorationModelBudgetExhausted = errors.New("exploration model budget exhausted")
	ErrToolBudgetExhausted             = errors.New("tool budget exhausted")
	ErrInsufficientTime                = errors.New("insufficient time remaining for work item")
)

// BudgetManager provides centralized, atomic budget tracking for model calls,
// tool calls, and wall-clock execution deadlines across Planner and all Workers.
type BudgetManager struct {
	mu          sync.Mutex
	cfg         model.BudgetConfig
	startTime   time.Time
	deadline    time.Time
	hasDeadline bool

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
	}

	timeout := cfg.RunTimeoutSeconds
	if timeout <= 0 {
		timeout = 120
	}
	bm.deadline = startTime.Add(time.Duration(timeout) * time.Second)
	bm.hasDeadline = true

	return bm
}

// UpdateEffectiveBudget updates the budget limits and deadline dynamically while strictly preserving
// cumulative usage counters.
func (b *BudgetManager) UpdateEffectiveBudget(cfg model.BudgetConfig) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cfg = cfg
	if cfg.MaxModelCalls > 0 {
		b.maxModelCalls = cfg.MaxModelCalls
		reserve := 0
		if b.maxModelCalls > 1 {
			reserve = 1
		}
		b.reserveForWrapup = reserve
		b.maxExplorationCalls = b.maxModelCalls - reserve
	}
	if cfg.MaxToolCalls > 0 {
		b.maxToolCalls = cfg.MaxToolCalls
	}
	if cfg.RunTimeoutSeconds > 0 {
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

	// Check total hard limit
	if int(b.consumedModelCalls) >= b.maxModelCalls {
		return ErrModelBudgetExhausted
	}

	// Check exploration budget
	if !isWrapup && int(b.consumedModelCalls) >= b.maxExplorationCalls {
		return ErrExplorationModelBudgetExhausted
	}

	b.consumedModelCalls++
	b.inflightModelCalls++
	return nil
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
	if int(b.consumedToolCalls) >= b.maxToolCalls {
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
	if int(b.consumedModelCalls) >= b.maxExplorationCalls {
		return false
	}

	// Tool quota reached -> stop claiming
	if int(b.consumedToolCalls) >= b.maxToolCalls {
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
	remaining := b.maxModelCalls - int(b.consumedModelCalls)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (b *BudgetManager) RemainingExplorationModelCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.maxExplorationCalls - int(b.consumedModelCalls)
	if remaining < 0 {
		return 0
	}
	return remaining
}

func (b *BudgetManager) RemainingToolCalls() int {
	b.mu.Lock()
	defer b.mu.Unlock()
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
	if int(b.consumedToolCalls) >= b.maxToolCalls {
		return model.CauseToolBudgetExhausted
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
		workerMaxModels:         maxModels,
		workerMaxTools:          maxTools,
		settlementModelsAllowed: 1, // 1 model call for settlement wrapup
	}
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
	if w.bm.RemainingTime() <= 0 {
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
	if w.bm.RemainingModelCalls() <= 0 {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseModelBudgetExhausted,
		}
	}

	// 3. Check Worker-local deadline
	if time.Now().After(w.localDeadline) {
		if !w.inSettlement.Load() {
			w.inSettlement.Store(true)
			settleSec := w.cfg.WorkerSettlementTimeoutSeconds
			if settleSec <= 0 {
				settleSec = 15
			}
			w.localDeadline = time.Now().Add(time.Duration(settleSec) * time.Second)
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

	// 4. Check Worker-local model call limits
	if !w.inSettlement.Load() {
		if w.localModelCalls >= w.workerMaxModels {
			w.inSettlement.Store(true)
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerModelBudgetExhausted,
				IsSettlement: true,
			}
		}
	} else {
		// In settlement
		if w.settlementModelsUsed >= w.settlementModelsAllowed {
			return agent.BudgetGateResult{
				Allowed:      false,
				Scope:        agent.ScopeWorkerLocal,
				Cause:        model.CauseWorkerSettlementComplete,
				IsSettlement: false,
			}
		}
	}

	if !isSettlement && contract.WorkerModelCallsAdmittable(w.bm.RemainingModelCalls(), w.plannerReserve, w.mainReserve) <= 0 {
		return agent.BudgetGateResult{
			Allowed: false,
			Scope:   agent.ScopeGlobal,
			Cause:   model.CauseModelBudgetExhausted,
		}
	}

	// 5. Acquire from global budget
	if err := w.bm.AcquireModelCall(ctx, isSettlement); err != nil {
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
	} else {
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
	if w.bm.RemainingTime() <= 0 {
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
	if w.bm.RemainingToolCalls() <= 0 {
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
		case "record_fact", "report_finding", "record_observation", "record_observation_batch", "read_observations", "list_facts", "node_detail", "graph_overview", "read_artifact", "get_worker_output", "list_artifacts", "write_artifact", "inspect_artifact", "list_worker_traces", "get_worker_trace", "search_all_worker_traces", "list_capability_profiles":
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
	if w.localToolCalls >= w.workerMaxTools {
		if !w.inSettlement.Load() {
			w.inSettlement.Store(true)
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
	b.startTime = started
	timeout := b.cfg.RunTimeoutSeconds
	if timeout <= 0 {
		timeout = 120
	}
	b.deadline = started.Add(time.Duration(timeout) * time.Second)
}
