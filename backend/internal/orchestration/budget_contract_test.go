package orchestration

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
)

func TestBudgetConcurrentAdmissionAndReservedSummary(t *testing.T) {
	b := NewBudgetManager(model.BudgetConfig{MaxModelCalls: 2, MaxToolCalls: 1}, time.Now())
	var admitted atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if b.AcquireModelCall(context.Background(), false) == nil {
				admitted.Add(1)
			}
		}()
	}
	close(start)
	wg.Wait()
	if admitted.Load() != 1 || b.InflightModelCalls() != 1 {
		t.Fatalf("admitted=%d inflight=%d", admitted.Load(), b.InflightModelCalls())
	}
	if b.CanClaimWorkItem(context.Background()) {
		t.Fatal("claimed work after exploration quota consumed")
	}
	b.ReleaseModelCall()
	b.MarkWrapupStarted()
	if err := b.AcquireModelCall(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if err := b.AcquireModelCall(context.Background(), true); err == nil {
		t.Fatal("exceeded total hard limit")
	}
	b.ReleaseModelCall()
	if b.ConsumedModelCalls() != 2 || b.InflightModelCalls() != 0 {
		t.Fatal("wrong budget accounting")
	}
}

func TestBudgetSettlementAndDeadlineBlockAdmission(t *testing.T) {
	for _, scenario := range []string{"settling", "expired", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			start := time.Now()
			if scenario == "expired" {
				start = start.Add(-time.Hour)
			}
			b := NewBudgetManager(model.BudgetConfig{MaxModelCalls: 10, MaxToolCalls: 10, RunTimeoutSeconds: 60}, start)
			if scenario == "settling" {
				b.MarkWrapupStarted()
			}
			if scenario == "cancelled" {
				cancel()
			}
			if b.CanClaimWorkItem(ctx) {
				t.Fatal("work admitted after stop boundary")
			}
			if err := b.AcquireModelCall(ctx, false); err == nil {
				t.Fatal("model admitted after stop boundary")
			}
			if err := b.AcquireToolCall(ctx); err == nil {
				t.Fatal("tool admitted after stop boundary")
			}
		})
	}
}

func TestBudgetConcurrentToolsAndSettlementRespectBothHardCaps(t *testing.T) {
	b := NewBudgetManager(model.BudgetConfig{MaxModelCalls: 7, MaxToolCalls: 3}, time.Now())
	var models, tools atomic.Int32
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if i == 64 {
				b.MarkWrapupStarted()
			}
			if b.AcquireModelCall(context.Background(), i%2 == 0) == nil {
				models.Add(1)
				b.ReleaseModelCall()
			}
			if b.AcquireToolCall(context.Background()) == nil {
				tools.Add(1)
			}
		}(i)
	}
	close(start)
	wg.Wait()
	if models.Load() > 7 || tools.Load() > 3 || b.ConsumedModelCalls() != int(models.Load()) || b.ConsumedToolCalls() != int(tools.Load()) || b.InflightModelCalls() != 0 {
		t.Fatalf("models=%d tools=%d consumed=%d/%d inflight=%d", models.Load(), tools.Load(), b.ConsumedModelCalls(), b.ConsumedToolCalls(), b.InflightModelCalls())
	}
	if err := b.AcquireToolCall(context.Background()); err == nil {
		t.Fatal("tool admitted in settlement")
	}
	if err := b.AcquireModelCall(context.Background(), false); err == nil {
		t.Fatal("exploration admitted in settlement")
	}
}
