package handler

import (
	"context"
	"encoding/json"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/controlplane"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/tools"
)

// NewPlannerControlInstaller adapts the one supervision factory to the engine
// port. The engine does not import the control plane.
func NewPlannerControlInstaller(supervision *controlplane.Supervision) orchestration.PlannerControlInstaller {
	return supervisionInstaller{supervision: supervision}
}

// supervisionInstaller registers planner control tools from the one controller.
type supervisionInstaller struct {
	supervision *controlplane.Supervision
}

func (s supervisionInstaller) Install(reg *tools.Registry, scope contract.ExecutionScope, principal contract.Principal, onMutation func(string)) error {
	if s.supervision == nil {
		return persistenceUnavailable()
	}
	controlReg, err := s.supervision.Registry(scope, principal, nil, true)
	if err != nil {
		return err
	}
	for _, name := range contract.PlannerControlTools {
		tool, ok := controlReg.Get(name)
		if !ok {
			continue
		}
		toolName := name
		reg.Register(markingTool{inner: tool, onSuccess: func() {
			if onMutation != nil {
				onMutation(toolName)
			}
		}})
	}
	return nil
}

type markingTool struct {
	inner     tools.Tool
	onSuccess func()
}

func (t markingTool) Name() string                { return t.inner.Name() }
func (t markingTool) Description() string         { return t.inner.Description() }
func (t markingTool) Parameters() json.RawMessage { return t.inner.Parameters() }
func (t markingTool) Validate(argumentsJSON []byte) error {
	return t.inner.Validate(argumentsJSON)
}

func (t markingTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	out, err := t.inner.Execute(ctx, argumentsJSON)
	if err != nil {
		return out, err
	}
	if t.onSuccess != nil && controlToolSucceeded(out) {
		t.onSuccess()
	}
	return out, nil
}

func controlToolSucceeded(out string) bool {
	var body struct {
		Status   string `json:"status"`
		Replayed bool   `json:"replayed"`
	}
	if err := json.Unmarshal([]byte(out), &body); err != nil {
		return false
	}
	// An idempotent replay did not change work-item state. Treating it as a
	// new plan keeps the planner loop awake until the model budget is gone.
	if body.Replayed {
		return false
	}
	return body.Status == string(contract.CommandApplied) || body.Status == string(contract.CommandAccepted)
}

var _ orchestration.PlannerControlInstaller = supervisionInstaller{}
