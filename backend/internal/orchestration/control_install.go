package orchestration

import (
	"log/slog"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// PlannerControlInstaller registers the shared supervision tools. The engine
// does not import the control plane, so the composition root supplies this.
type PlannerControlInstaller interface {
	Install(reg *tools.Registry, scope contract.ExecutionScope, principal contract.Principal, onMutation func(toolName string)) error
}

func WithPlannerControlInstaller(installer PlannerControlInstaller) EngineOption {
	return func(e *Engine) {
		if installer != nil {
			e.controlInstaller = installer
		}
	}
}

func WithToolBatchSupervisor(supervisor *ToolBatchSupervisor) EngineOption {
	return func(e *Engine) {
		if supervisor != nil {
			e.steer = supervisor
		}
	}
}

func (e *Engine) installPlannerControl(reg *tools.Registry, sessionID uuid.UUID, onMutation func(string)) {
	if e == nil || e.controlInstaller == nil || reg == nil {
		return
	}
	scope := contract.ExecutionScope{
		RunID:       e.runID,
		EpochNumber: e.Epoch(),
		Role:        contract.RolePlanner,
		SessionID:   &sessionID,
	}
	principal := contract.Principal{
		Kind:    contract.PrincipalPlanner,
		ActorID: "planner:" + sessionID.String(),
		RunID:   e.runID,
	}
	if err := e.controlInstaller.Install(reg, scope, principal, onMutation); err != nil {
		// A missing installer is a composition failure. Do not fall back to
		// the direct-write continue/retry tools.
		slog.Error("engine: planner supervision tools were not installed", "run_id", e.runID, "error", err)
	}
}

// supervisorDecider adapts the shared runtime supervisor to the session port.
type supervisorDecider struct {
	supervisor *ToolBatchSupervisor
}

func (d supervisorDecider) DecideBeforeTool(workItemID uuid.UUID, batch []agent.ClosedToolCall, executed int) agent.ToolBatchDecision {
	if d.supervisor == nil {
		return agent.ToolBatchDecision{}
	}
	mapped := make([]ClosedToolCall, len(batch))
	for i, call := range batch {
		mapped[i] = ClosedToolCall{ID: call.ID, Name: call.Name, Result: call.Result}
	}
	decision := d.supervisor.DecideBeforeTool(workItemID, mapped, executed)
	out := agent.ToolBatchDecision{
		CloseUnexecuted: decision.CloseUnexecuted,
		Instruction:     decision.Instruction,
	}
	for _, call := range decision.ClosedCalls {
		out.ClosedCalls = append(out.ClosedCalls, agent.ClosedToolCall{ID: call.ID, Name: call.Name, Result: call.Result})
	}
	return out
}
