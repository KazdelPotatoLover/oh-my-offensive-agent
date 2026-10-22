package execution

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// Runner is the server-owned execution surface used by capability factories.
type Runner interface {
	contract.JobExecutor
	WriteFile(ctx context.Context, scope contract.ExecutionScope, rel string, content []byte) (uuid.UUID, error)
	ReadFile(ctx context.Context, scope contract.ExecutionScope, rel string, offset, limit int) ([]byte, error)
}

var (
	runnerMu sync.RWMutex
	bound    Runner
)

// RegisterAdapters mounts command and file tools. It is not called from init.
// Default profiles stay unchanged until a profile explicitly enables these names.
func RegisterAdapters(r Runner) error {
	if r == nil {
		return invalidInput("execution runner is not configured")
	}
	runnerMu.Lock()
	bound = r
	runnerMu.Unlock()
	for _, adapter := range []tools.Adapter{
		{Name: "run_command", Version: "1", Factory: func(c tools.AdapterContext) tools.Tool { return NewRunCommandTool(currentRunner(), c) }},
		{Name: "write_file", Version: "1", Factory: func(c tools.AdapterContext) tools.Tool { return NewWriteFileTool(currentRunner(), c) }},
		{Name: "read_file", Version: "1", Factory: func(c tools.AdapterContext) tools.Tool { return NewReadFileTool(currentRunner(), c) }},
		{Name: "inspect_command", Version: "1", Factory: func(c tools.AdapterContext) tools.Tool { return NewInspectCommandTool(currentRunner(), c) }},
	} {
		if err := tools.RegisterAdapter(adapter); err != nil {
			return err
		}
	}
	return nil
}

func currentRunner() Runner {
	runnerMu.RLock()
	defer runnerMu.RUnlock()
	return bound
}

func NewRunCommandTool(r Runner, c tools.AdapterContext) tools.Tool {
	return &runCommandTool{runner: r, scope: c}
}
func NewWriteFileTool(r Runner, c tools.AdapterContext) tools.Tool {
	return &writeFileTool{runner: r, scope: c}
}
func NewReadFileTool(r Runner, c tools.AdapterContext) tools.Tool {
	return &readFileTool{runner: r, scope: c}
}
func NewInspectCommandTool(r Runner, c tools.AdapterContext) tools.Tool {
	return &inspectCommandTool{runner: r, scope: c}
}

type runCommandTool struct {
	runner Runner
	scope  tools.AdapterContext
}

func (t *runCommandTool) Name() string { return "run_command" }
func (t *runCommandTool) Description() string {
	return "Run an allowlisted command in this work item's isolated container. The result is an execution record, not a verified finding."
}
func (t *runCommandTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["argv"],"properties":{"argv":{"type":"array","items":{"type":"string"}},"timeout_seconds":{"type":"integer"}}}`)
}
func (t *runCommandTool) Validate(b []byte) error {
	return decodeTool(b, &struct {
		Argv           []string `json:"argv"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}{})
}

func (t *runCommandTool) Execute(ctx context.Context, b []byte) (string, error) {
	if PhaseFrom(ctx) == PhaseSettlement {
		return "", scopeDenied("settlement cannot start commands")
	}
	var in struct {
		Argv           []string `json:"argv"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := decodeTool(b, &in); err != nil {
		return "", err
	}
	scope, err := t.executionScope(ctx)
	if err != nil {
		return "", err
	}
	view, err := t.runner.Run(ctx, scope, contract.JobSpec{
		Argv:           in.Argv,
		TimeoutSeconds: in.TimeoutSeconds,
		WorkItemID:     t.scope.WorkItemID,
	})
	if err != nil {
		return "", err
	}
	return marshalResult(view)
}

type writeFileTool struct {
	runner Runner
	scope  tools.AdapterContext
}

func (t *writeFileTool) Name() string { return "write_file" }
func (t *writeFileTool) Description() string {
	return "Write a file inside this work item workspace. Settlement cannot call this tool."
}
func (t *writeFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path","content"],"properties":{"path":{"type":"string"},"content":{"type":"string"}}}`)
}
func (t *writeFileTool) Validate(b []byte) error {
	return decodeTool(b, &struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{})
}
func (t *writeFileTool) Execute(ctx context.Context, b []byte) (string, error) {
	if PhaseFrom(ctx) == PhaseSettlement {
		return "", scopeDenied("settlement cannot write workspace files")
	}
	var in struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeTool(b, &in); err != nil {
		return "", err
	}
	scope, err := t.executionScope(ctx)
	if err != nil {
		return "", err
	}
	id, err := t.runner.WriteFile(ctx, scope, in.Path, []byte(in.Content))
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{
		"artifact_id":    id,
		"path":           in.Path,
		"empirical":      false,
		"evidence_class": "workspace_file",
	})
	return string(raw), err
}

type readFileTool struct {
	runner Runner
	scope  tools.AdapterContext
}

func (t *readFileTool) Name() string { return "read_file" }
func (t *readFileTool) Description() string {
	return "Read a file from this work item workspace. Symlinks and paths outside the workspace are rejected."
}
func (t *readFileTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["path"],"properties":{"path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}}}`)
}
func (t *readFileTool) Validate(b []byte) error {
	return decodeTool(b, &struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}{})
}
func (t *readFileTool) Execute(ctx context.Context, b []byte) (string, error) {
	var in struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := decodeTool(b, &in); err != nil {
		return "", err
	}
	scope, err := t.executionScope(ctx)
	if err != nil {
		return "", err
	}
	content, err := t.runner.ReadFile(ctx, scope, in.Path, in.Offset, in.Limit)
	if err != nil {
		return "", err
	}
	raw, err := json.Marshal(map[string]any{
		"path":           in.Path,
		"content":        string(content),
		"empirical":      false,
		"evidence_class": "workspace_file",
	})
	return string(raw), err
}

type inspectCommandTool struct {
	runner Runner
	scope  tools.AdapterContext
}

func (t *inspectCommandTool) Name() string { return "inspect_command" }
func (t *inspectCommandTool) Description() string {
	return "Read a previously recorded command job. This does not start or repeat the command."
}
func (t *inspectCommandTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["job_id"],"properties":{"job_id":{"type":"string"}}}`)
}
func (t *inspectCommandTool) Validate(b []byte) error {
	return decodeTool(b, &struct {
		JobID string `json:"job_id"`
	}{})
}
func (t *inspectCommandTool) Execute(ctx context.Context, b []byte) (string, error) {
	var in struct {
		JobID string `json:"job_id"`
	}
	if err := decodeTool(b, &in); err != nil {
		return "", err
	}
	id, err := uuid.Parse(in.JobID)
	if err != nil {
		return "", invalidInput("job_id is not valid")
	}
	scope, err := t.executionScope(ctx)
	if err != nil {
		return "", err
	}
	view, err := t.runner.Inspect(ctx, scope, id)
	if err != nil {
		return "", err
	}
	return marshalResult(view)
}

func (t *runCommandTool) executionScope(ctx context.Context) (contract.ExecutionScope, error) {
	return scopeFrom(ctx, t.scope)
}
func (t *writeFileTool) executionScope(ctx context.Context) (contract.ExecutionScope, error) {
	return scopeFrom(ctx, t.scope)
}
func (t *readFileTool) executionScope(ctx context.Context) (contract.ExecutionScope, error) {
	return scopeFrom(ctx, t.scope)
}
func (t *inspectCommandTool) executionScope(ctx context.Context) (contract.ExecutionScope, error) {
	return scopeFrom(ctx, t.scope)
}

func scopeFrom(ctx context.Context, c tools.AdapterContext) (contract.ExecutionScope, error) {
	epoch, ok := epochFrom(ctx)
	if !ok && c.Service != nil {
		run, err := c.Service.Get(ctx, c.RunID)
		if err != nil {
			return contract.ExecutionScope{}, unavailable("execution run could not be read")
		}
		epoch = run.CurrentEpoch
		ok = epoch >= 1
	}
	if !ok {
		return contract.ExecutionScope{}, invalidInput("execution epoch is not available")
	}
	wi, at, se := c.WorkItemID, c.AttemptID, c.SessionID
	scope := contract.ExecutionScope{
		RunID:       c.RunID,
		EpochNumber: epoch,
		Role:        contract.RoleWorker,
		WorkItemID:  &wi,
		AttemptID:   &at,
	}
	if se != uuid.Nil {
		scope.SessionID = &se
	}
	if id := sourceToolCallFrom(ctx); id != nil {
		scope.SourceToolCallID = id
	}
	return scope, nil
}

func decodeTool(b []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return invalidInput("command arguments are not valid")
	}
	return nil
}

func marshalResult(view *contract.JobView) (string, error) {
	raw, err := json.Marshal(struct {
		*contract.JobView
		Empirical     bool   `json:"empirical"`
		EvidenceClass string `json:"evidence_class"`
	}{JobView: view, Empirical: false, EvidenceClass: "execution_result"})
	return string(raw), err
}
