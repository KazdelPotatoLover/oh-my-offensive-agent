package execution

import (
	"context"
	"sync"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// Fake is an explicit test double. Production wiring must not use it.
type Fake struct {
	mu     sync.Mutex
	Views  map[uuid.UUID]*contract.JobView
	Files  map[string][]byte
	Calls  []string
	RunErr error
	Block  bool
	lastID uuid.UUID
}

func NewFake() *Fake {
	return &Fake{Views: map[uuid.UUID]*contract.JobView{}, Files: map[string][]byte{}}
}

func (f *Fake) Run(ctx context.Context, scope contract.ExecutionScope, spec contract.JobSpec) (*contract.JobView, error) {
	f.mu.Lock()
	f.Calls = append(f.Calls, "run")
	block := f.Block
	err := f.RunErr
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if block {
		<-ctx.Done()
		id := uuid.New()
		view := &contract.JobView{JobID: id, State: contract.JobKilled}
		f.mu.Lock()
		f.Views[id] = view
		f.mu.Unlock()
		return view, nil
	}
	id := uuid.New()
	code := 0
	if len(spec.Argv) > 1 && spec.Argv[1] == "fail" {
		code = 2
	}
	view := &contract.JobView{JobID: id, State: contract.JobFinished, ExitCode: &code}
	f.mu.Lock()
	f.Views[id] = view
	f.lastID = id
	f.mu.Unlock()
	return view, nil
}

func (f *Fake) Cancel(ctx context.Context, scope contract.ExecutionScope, jobID uuid.UUID) (*contract.JobView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "cancel")
	view := f.Views[jobID]
	if view == nil {
		return nil, notFound("execution job was not found")
	}
	view.State = contract.JobKilled
	view.ExitCode = nil
	return view, nil
}

func (f *Fake) Inspect(ctx context.Context, scope contract.ExecutionScope, jobID uuid.UUID) (*contract.JobView, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "inspect")
	view := f.Views[jobID]
	if view == nil {
		return nil, notFound("execution job was not found")
	}
	return view, nil
}

func (f *Fake) WriteFile(ctx context.Context, scope contract.ExecutionScope, rel string, content []byte) (uuid.UUID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "write")
	if PhaseFrom(ctx) == PhaseSettlement {
		return uuid.Nil, scopeDenied("settlement cannot write workspace files")
	}
	f.Files[rel] = append([]byte(nil), content...)
	return uuid.New(), nil
}

func (f *Fake) ReadFile(ctx context.Context, scope contract.ExecutionScope, rel string, offset, limit int) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Calls = append(f.Calls, "read")
	content, ok := f.Files[rel]
	if !ok {
		return nil, notFound("workspace file does not exist")
	}
	if offset > len(content) {
		return []byte{}, nil
	}
	content = content[offset:]
	if limit > 0 && limit < len(content) {
		content = content[:limit]
	}
	return content, nil
}

var _ Runner = (*Fake)(nil)
