package worker

import (
	"context"
	"sync"
	"testing"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

type recordingRevoker struct {
	mu    sync.Mutex
	calls int
	last  contract.RevokeRequest
}

func (r *recordingRevoker) Run(ctx context.Context) { <-ctx.Done() }

func (r *recordingRevoker) Revoke(ctx context.Context, req contract.RevokeRequest) (*contract.RevokeReport, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	r.last = req
	return &contract.RevokeReport{Kind: req.Kind, Cancelled: 1}, nil
}

func (r *recordingRevoker) ActiveAttempt(id uuid.UUID) (uuid.UUID, bool) {
	return uuid.MustParse("11111111-1111-1111-1111-111111111111"), true
}

func (r *recordingRevoker) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func TestKillWorkItemRevokesExternalJobAfterEngineSuccess(t *testing.T) {
	m := NewManager(nil, nil)
	runID := uuid.New()
	item := uuid.New()
	engine := &recordingRevoker{}
	external := &recordingRevoker{}
	m.cancels[runID] = func(error) {}
	m.executors[runID] = engine
	m.executorEpochs[runID] = 3
	m.UseResourceRevoker(external)

	if err := m.KillWorkItem(runID, item, "killed_by_operator"); err != nil {
		t.Fatal(err)
	}
	if engine.count() != 1 {
		t.Fatalf("engine revoke calls = %d", engine.count())
	}
	if external.count() != 1 {
		t.Fatalf("external revoke calls = %d", external.count())
	}
	external.mu.Lock()
	defer external.mu.Unlock()
	if external.last.Scope.AttemptID == nil || external.last.Scope.Role != contract.RoleWorker {
		t.Fatalf("external scope %+v", external.last.Scope)
	}
}
