package orchestration

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
)

func TestPlannerAvailabilityClassification(t *testing.T) {
	for _, tt := range []struct {
		name string
		err  error
		want model.TerminationCause
	}{
		{"502", &provider.CallError{Class: provider.ClassUpstream, HTTPStatus: 502, Retryable: true}, model.CauseNeedsInput},
		{"429", &provider.CallError{Class: provider.ClassRateLimited, HTTPStatus: 429, Retryable: true}, model.CauseNeedsInput},
		{"transport", &provider.CallError{Class: provider.ClassTransport, Retryable: true}, model.CauseNeedsInput},
		{"timeout", &provider.CallError{Class: provider.ClassTimeout, Retryable: true}, model.CauseNeedsInput},
		{"401", &provider.CallError{Class: provider.ClassClientHTTP, HTTPStatus: 401}, model.TerminationCause("provider_error")},
		{"unknown", fmt.Errorf("unknown provider failure"), model.TerminationCause("provider_error")},
		{"protocol", provider.ErrProtocol, model.TerminationCause("protocol_error")},
		{"local context limit", provider.ErrContextLimit, model.CauseNeedsInput},
	} {
		t.Run(tt.name, func(t *testing.T) {
			err := fmt.Errorf("provider error: %w", tt.err)
			if got := plannerFailureCause(nil, err); got != tt.want {
				t.Fatalf("got %s want %s", got, tt.want)
			}
			if (plannerFailureSummary(err) != nil) != (tt.want == model.CauseNeedsInput) {
				t.Fatal("recoverable stops should receive specific recovery guidance")
			}
		})
	}
}

func TestWorkerSetupCancellationPreservesStopReason(t *testing.T) {
	for _, cause := range []model.TerminationCause{model.CauseUserStop, model.CauseShutdown, model.CauseTimeout, model.CauseNeedsInput} {
		ctx, cancel := context.WithCancelCause(context.Background())
		cancel(cause)
		for _, fallback := range []string{"profile_load_failed", "workspace_unavailable", "context_restore_failed"} {
			if got := workerSetupFailureReason(ctx, fallback); got != string(cause) {
				t.Fatalf("got %s want %s", got, cause)
			}
		}
	}
	if got := workerSetupFailureReason(context.Background(), "profile_load_failed"); got != "profile_load_failed" {
		t.Fatal("actual profile errors must remain diagnosable")
	}
}

func TestWorkerReservesAtomicAcrossGates(t *testing.T) {
	b := NewBudgetManager(model.BudgetConfig{MaxModelCalls: 10}, time.Now())
	var admitted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			gate := NewWorkerBudgetGate(b, model.BudgetConfig{})
			gate.SetReserves(4, 3)
			res := gate.TryAcquireModel(context.Background(), false)
			if res.Allowed {
				admitted.Add(1)
				b.ReleaseModelCall()
			} else if res.Cause != model.CauseWorkerBudgetReserved {
				t.Errorf("wrong denial cause: %+v", res)
			}
		}()
	}
	wg.Wait()
	if admitted.Load() != 3 || b.RemainingModelCalls() != 7 || b.WorkerModelCallsRemaining(4, 3) != 0 {
		t.Fatalf("admitted=%d remaining=%d", admitted.Load(), b.RemainingModelCalls())
	}
	if err := b.AcquireModelCall(context.Background(), false); err != nil {
		t.Fatalf("Planner must still be allowed to review: %v", err)
	}
}
