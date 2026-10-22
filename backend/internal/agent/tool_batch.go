package agent

import (
	"github.com/google/uuid"

	"agent-workbench/backend/internal/provider"
)

// ClosedToolCall is the session-local view of a proposed call. It matches the
// runtime supervisor's fields so the engine can convert without a package cycle.
type ClosedToolCall struct {
	ID     string
	Name   string
	Result string
}

// ToolBatchDecision closes calls that must not run after steer arrives.
// A user message is never inserted between an assistant tool call and its result.
type ToolBatchDecision struct {
	CloseUnexecuted bool
	Instruction     string
	ClosedCalls     []ClosedToolCall
}

// ToolBatchDecider is injected by the engine. SessionRunner does not write steer rows.
type ToolBatchDecider interface {
	DecideBeforeTool(workItemID uuid.UUID, batch []ClosedToolCall, executed int) ToolBatchDecision
}

// WithToolBatchDecider closes an already chosen tool batch before the next action.
// workItemID is the worker item. Planner sessions leave the decider unset.
func WithToolBatchDecider(workItemID uuid.UUID, decider ToolBatchDecider) SessionRunnerOption {
	return func(r *SessionRunner) {
		r.workItemID = workItemID
		r.batchDecider = decider
	}
}

// CloseUnexecutedMessages matches controlplane.CloseUnexecutedBatch. Tool results
// come first, then one steer guidance message.
func CloseUnexecutedMessages(assistant provider.ChatMessage, executed int, decision ToolBatchDecision) []provider.ChatMessage {
	if !decision.CloseUnexecuted {
		return nil
	}
	if executed < 0 {
		executed = 0
	}
	out := make([]provider.ChatMessage, 0, len(assistant.ToolCalls)-executed+1)
	for i := executed; i < len(assistant.ToolCalls); i++ {
		call := assistant.ToolCalls[i]
		result := "tool batch closed before execution because operator steer requires a new model evaluation: " + decision.Instruction
		idx := i - executed
		if idx < len(decision.ClosedCalls) && decision.ClosedCalls[idx].Result != "" {
			result = decision.ClosedCalls[idx].Result
		}
		out = append(out, provider.ChatMessage{
			Role:       provider.RoleTool,
			ToolCallID: call.ID,
			Content:    result,
		})
	}
	if decision.Instruction != "" {
		out = append(out, provider.ChatMessage{
			Role:    provider.RoleUser,
			Content: "[Operator Steer Guidance]\n" + decision.Instruction,
		})
	}
	return out
}
