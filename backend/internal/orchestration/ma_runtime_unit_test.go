package orchestration

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

func TestMARuntime_WorkersFromSnapshot_MissingStaysOne(t *testing.T) {
	n, err := WorkersFromSnapshot([]byte(`{"objective":"old"}`))
	if err != nil || n != 1 {
		t.Fatalf("missing snapshot workers=%d err=%v", n, err)
	}
	n, err = WorkersFromSnapshot([]byte(`{"budget":{"max_model_calls":30}}`))
	if err != nil || n != 1 {
		t.Fatalf("budget without workers=%d err=%v", n, err)
	}
	n, err = WorkersFromSnapshot([]byte(`{"budget":{"max_parallel_workers":4}}`))
	if err != nil || n != 4 {
		t.Fatalf("stored workers=%d err=%v", n, err)
	}
	if _, err = WorkersFromSnapshot([]byte(`{"budget":{"max_parallel_workers":9}}`)); err == nil {
		t.Fatal("out of range snapshot was accepted")
	}
	var coded *contract.Error
	if !errors.As(err, &coded) || coded.Code != contract.CodeConcurrencyOutOfRange {
		t.Fatalf("out of range err=%v", err)
	}
}

func TestMARuntimeHeartbeatReviewsHeldWorkers(t *testing.T) {
	e := &Engine{plannerHeartbeat: time.Second, lastPlannerAt: time.Now(), notifyChan: make(chan struct{}, 1)}
	e.activeWorkerCount.Store(2)
	if e.plannerShouldEvaluate(context.Background()) {
		t.Fatal("busy workers must not cause a planning loop")
	}
	e.lastPlannerAt = time.Now().Add(-2 * time.Second)
	if !e.plannerShouldEvaluate(context.Background()) {
		t.Fatal("heartbeat did not wake supervision")
	}
	e.lastPlannerAt = time.Now()
	e.Nudge()
	if !e.plannerShouldEvaluate(context.Background()) {
		t.Fatal("explicit event did not wake supervision")
	}
}

func TestMARuntime_ModelLimiter_WaitAndCancelWakes(t *testing.T) {
	lim := newModelLimiter(2)
	ctx := context.Background()
	if err := lim.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	if err := lim.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	var queued atomic.Int32
	lim.SetQueueHook(func(n int) {
		for {
			old := queued.Load()
			if int32(n) <= old || queued.CompareAndSwap(old, int32(n)) {
				return
			}
		}
	})

	started := make(chan struct{})
	released := make(chan struct{})
	go func() {
		close(started)
		if err := lim.Acquire(ctx); err != nil {
			t.Errorf("third acquire: %v", err)
			return
		}
		close(released)
		lim.Release()
	}()
	<-started
	time.Sleep(20 * time.Millisecond)
	if lim.Capacity().Queued != 1 || lim.Capacity().InFlight != 2 {
		t.Fatalf("capacity=%+v", lim.Capacity())
	}
	lim.Release()
	select {
	case <-released:
	case <-time.After(time.Second):
		t.Fatal("release did not wake the queued model call")
	}
	if queued.Load() < 1 {
		t.Fatal("queue hook did not observe a waiter")
	}

	if err := lim.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	woke := make(chan error, 1)
	go func() {
		woke <- lim.Acquire(cancelCtx)
	}()
	time.Sleep(20 * time.Millisecond)
	cancel()
	select {
	case err := <-woke:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel wake err=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not wake the queued model call")
	}
	if lim.Capacity().InFlight != 2 {
		t.Fatalf("cancelled waiter kept a slot: %+v", lim.Capacity())
	}
}

func TestMARuntime_SteerClosesUnexecutedToolBatch(t *testing.T) {
	sup := NewToolBatchSupervisor()
	id := uuid.New()
	batch := []ClosedToolCall{{ID: "1", Name: "old_action"}, {ID: "2", Name: "later_action"}}
	if decision := sup.DecideBeforeTool(id, batch, 0); decision.CloseUnexecuted {
		t.Fatal("steer was closed before it arrived")
	}
	sup.NoteSteer(id, "look again")
	decision := sup.DecideBeforeTool(id, batch, 1)
	if !decision.CloseUnexecuted || decision.Instruction != "look again" || len(decision.ClosedCalls) != 1 {
		t.Fatalf("decision=%+v", decision)
	}
	if decision.ClosedCalls[0].ID != "2" || decision.ClosedCalls[0].Result == "" {
		t.Fatalf("closed call=%+v", decision.ClosedCalls[0])
	}
	if again := sup.DecideBeforeTool(id, batch, 0); again.CloseUnexecuted {
		t.Fatal("steer decision was applied twice")
	}
}

func TestMARuntime_RevokeCausePreservesReason(t *testing.T) {
	if got := revokeCause("epoch_revoked"); string(got) != "epoch_revoked" {
		t.Fatalf("epoch cause=%s", got)
	}
	if got := revokeCause("killed_by_operator"); got != model.CauseWorkerKilled {
		t.Fatalf("kill cause=%s", got)
	}
	if got := revokeCause(""); got != model.CauseUserStop {
		t.Fatalf("empty cause=%s", got)
	}
	if got := revokeCause("protocol_error"); string(got) != "protocol_error" {
		t.Fatalf("local cause=%s", got)
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	cancel(model.TerminationCause("epoch_revoked"))
	if got := model.ResolveCancelCause(ctx); string(got) != "epoch_revoked" {
		t.Fatalf("resolved epoch cause=%s", got)
	}
}

func TestMARuntime_LimiterCancelDoesNotLeakUnderRace(t *testing.T) {
	lim := newModelLimiter(1)
	if err := lim.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithCancel(context.Background())
			go func() {
				time.Sleep(time.Millisecond)
				cancel()
			}()
			err := lim.Acquire(ctx)
			errCh <- err
			if err == nil {
				lim.Release()
			}
		}()
	}
	time.Sleep(5 * time.Millisecond)
	lim.Release()
	wg.Wait()
	close(errCh)
	if lim.Capacity().InFlight != 0 || lim.Capacity().Queued != 0 {
		t.Fatalf("limiter leaked: %+v", lim.Capacity())
	}
}
