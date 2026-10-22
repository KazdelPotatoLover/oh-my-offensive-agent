package provider

import (
	"context"
	"testing"
	"time"
)

func TestProcessRuntimeSharesAdmissionAcrossConsumers(t *testing.T) {
	t.Setenv("LLM_MAX_INFLIGHT", "1")
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "2048")
	t.Setenv("LLM_MODEL_RECOVERY_ATTEMPTS", "3")
	runtime := &ProcessRuntime{}
	a, _, _, err := runtime.Admission()
	if err != nil {
		t.Fatal(err)
	}
	b, _, _, err := runtime.Admission()
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer a.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := b.Acquire(ctx); err == nil {
		b.Release()
		t.Fatal("second consumer bypassed the shared cap")
	}
	if got := a.Snapshot(); got.InFlight != 1 || got.Queued != 0 {
		t.Fatalf("leaked cancelled admission: %+v", got)
	}
}
