package orchestration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

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
