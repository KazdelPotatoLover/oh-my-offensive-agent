package orchestration

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPlannerDirtyWakeCoalescesBurstBeforeRound(t *testing.T) {
	e := &Engine{
		notifyChan: make(chan struct{}, 100), debounce: 100 * time.Millisecond,
		plannerHeartbeat: time.Hour, lastPlannerAt: time.Now(),
	}
	e.activeWorkerCount.Store(3)
	e.Nudge()
	done := make(chan error, 1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	go func() { done <- e.waitUntilPlannerNeeded(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("dirty wake bypassed debounce: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	// Facts and worker completion notifications in the same burst belong to
	// one model review, even if dirty was already set before waiting.
	e.Nudge()
	e.markIdleDecision()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("planner did not wake after debounce")
	}
	if e.plannerShouldEvaluate(ctx) || len(e.notifyChan) != 0 {
		t.Fatal("coalesced notifications would consume an extra planner round")
	}
	e.Nudge()
	if !e.plannerShouldEvaluate(ctx) {
		t.Fatal("a new event after the review boundary must wake the next round")
	}
}

func TestPlannerDirtyDebounceHonorsCancellation(t *testing.T) {
	e := &Engine{notifyChan: make(chan struct{}, 1), debounce: time.Second}
	e.Nudge()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := e.waitUntilPlannerNeeded(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("got %v, want cancellation while coalescing", err)
	}
}
