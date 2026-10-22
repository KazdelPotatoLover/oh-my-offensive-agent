package agent

import (
	"context"
	"testing"
	"time"

	"agent-workbench/backend/internal/execution"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
)

func TestToolExecutionContextUsesHTTPRequestTimeout(t *testing.T) {
	budget := model.BudgetConfig{ToolTimeoutSeconds: 5}
	ctx, cancel := toolExecutionContext(context.Background(), "http_request", []byte(`{"timeout_seconds":30}`), budget, false, uuid.New(), 1)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("http_request context has no deadline")
	}
	left := time.Until(deadline)
	if left < 25*time.Second || left > 31*time.Second {
		t.Fatalf("http timeout left %s, want about 30s", left)
	}
	if execution.PhaseFrom(ctx) != execution.PhaseExplore {
		t.Fatalf("phase %s", execution.PhaseFrom(ctx))
	}
}

func TestToolExecutionContextClampsCommandToParentDeadline(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelParent()
	budget := model.BudgetConfig{ToolTimeoutSeconds: 5}
	ctx, cancel := toolExecutionContext(parent, "run_command", []byte(`{"timeout_seconds":60}`), budget, true, uuid.New(), 2)
	defer cancel()
	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("command context has no deadline")
	}
	left := time.Until(deadline)
	if left > 2500*time.Millisecond {
		t.Fatalf("command timeout %s extends the parent deadline", left)
	}
	if execution.PhaseFrom(ctx) != execution.PhaseSettlement {
		t.Fatalf("phase %s", execution.PhaseFrom(ctx))
	}
}

func TestToolExecutionContextCancelsWhenCommandDeadlinePassed(t *testing.T) {
	parent, cancelParent := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancelParent()
	time.Sleep(5 * time.Millisecond)
	ctx, cancel := toolExecutionContext(parent, "write_file", []byte(`{"timeout_seconds":30}`), model.BudgetConfig{}, false, uuid.Nil, 0)
	defer cancel()
	if ctx.Err() == nil {
		t.Fatal("expired parent still accepts a command")
	}
}
