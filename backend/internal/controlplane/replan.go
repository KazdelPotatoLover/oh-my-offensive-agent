package controlplane

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
)

// ClosedToolCall is a synthetic result for a call that must not run.
// Field names match the runtime ToolBatchSupervisor port.
type ClosedToolCall struct {
	ID     string
	Name   string
	Result string
}

// ToolBatchDecision tells the session boundary how to treat a tool batch that
// was already proposed when steer arrived. Closing the batch is required so
// the model can re-evaluate. Injecting the instruction and then executing the
// old calls is not a valid decision.
type ToolBatchDecision struct {
	CloseUnexecuted bool
	Instruction     string
	ClosedCalls     []ClosedToolCall
}

// ReplanPort arms a worker to re-evaluate before the next tool action.
// NoteSteer must not persist a second steer row. Persistence stays in RunService.
type ReplanPort interface {
	NoteSteer(workItemID uuid.UUID, instruction string)
}

// MemoryReplan is the in-process port used until Agent 0 injects the shared
// runtime supervisor. DecideBeforeTool matches that supervisor: already
// executed calls stay, and every later call in the batch is closed.
type MemoryReplan struct {
	mu      sync.Mutex
	pending map[uuid.UUID]string
}

func NewMemoryReplan() *MemoryReplan {
	return &MemoryReplan{pending: make(map[uuid.UUID]string)}
}

func (s *MemoryReplan) NoteSteer(workItemID uuid.UUID, instruction string) {
	if s == nil || workItemID == uuid.Nil || instruction == "" {
		return
	}
	s.mu.Lock()
	if s.pending == nil {
		s.pending = make(map[uuid.UUID]string)
	}
	s.pending[workItemID] = instruction
	s.mu.Unlock()
}

// DecideBeforeTool closes every not-yet-started call when steer is waiting.
// executed is the number of calls from this batch that already ran.
func (s *MemoryReplan) DecideBeforeTool(workItemID uuid.UUID, batch []ClosedToolCall, executed int) ToolBatchDecision {
	if s == nil {
		return ToolBatchDecision{}
	}
	s.mu.Lock()
	instruction := s.pending[workItemID]
	if instruction == "" {
		s.mu.Unlock()
		return ToolBatchDecision{}
	}
	delete(s.pending, workItemID)
	s.mu.Unlock()

	decision := ToolBatchDecision{CloseUnexecuted: true, Instruction: instruction}
	if executed < 0 {
		executed = 0
	}
	for i := executed; i < len(batch); i++ {
		call := batch[i]
		call.Result = closedToolResult(instruction)
		decision.ClosedCalls = append(decision.ClosedCalls, call)
	}
	return decision
}

func closedToolResult(instruction string) string {
	return "tool batch closed before execution because operator steer requires a new model evaluation: " + instruction
}

// CloseUnexecutedBatch returns the messages to append after the assistant
// message. It does not rewrite assistant tool calls or native payloads.
// Tool results for the unexecuted calls come first, then the steer guidance,
// so a user message is never inserted between the assistant tool calls and
// their results.
func CloseUnexecutedBatch(assistant provider.ChatMessage, executed int, decision ToolBatchDecision) []provider.ChatMessage {
	if !decision.CloseUnexecuted {
		return nil
	}
	if executed < 0 {
		executed = 0
	}
	out := make([]provider.ChatMessage, 0, len(assistant.ToolCalls)-executed+1)
	for i := executed; i < len(assistant.ToolCalls); i++ {
		call := assistant.ToolCalls[i]
		result := closedToolResult(decision.Instruction)
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
	if strings.TrimSpace(decision.Instruction) != "" {
		out = append(out, provider.ChatMessage{
			Role:    provider.RoleUser,
			Content: "[Operator Steer Guidance]\n" + decision.Instruction,
		})
	}
	return out
}

// ProtocolPairingOK checks Chat and Responses call pairing. A user message
// between an assistant tool call and its tool result is a protocol break.
// Native function_call items must have a matching tool result.
func ProtocolPairingOK(messages []provider.ChatMessage) error {
	type pending struct {
		id  string
		pos int
	}
	var open []pending
	for i, m := range messages {
		switch m.Role {
		case provider.RoleAssistant:
			if len(open) > 0 {
				return fmt.Errorf("assistant message at %d while tool call %s is unanswered", i, open[0].id)
			}
			ids := make([]string, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				if tc.ID == "" {
					return fmt.Errorf("assistant tool call at %d is missing an id", i)
				}
				ids = append(ids, tc.ID)
				open = append(open, pending{id: tc.ID, pos: i})
			}
			if m.NativePayload != nil {
				for _, item := range m.NativePayload.Output {
					var kind struct {
						Type   string `json:"type"`
						CallID string `json:"call_id"`
					}
					if err := json.Unmarshal(item, &kind); err != nil {
						return fmt.Errorf("native payload at %d: %w", i, err)
					}
					if kind.Type == "function_call" {
						if kind.CallID == "" {
							return fmt.Errorf("native function_call at %d is missing call_id", i)
						}
						found := false
						for _, id := range ids {
							if id == kind.CallID {
								found = true
								break
							}
						}
						if !found {
							return fmt.Errorf("native function_call %s has no matching tool call", kind.CallID)
						}
					}
				}
			}
		case provider.RoleTool:
			if m.ToolCallID == "" {
				return fmt.Errorf("tool result at %d is missing call_id", i)
			}
			if len(open) == 0 || open[0].id != m.ToolCallID {
				return fmt.Errorf("tool result %s at %d does not match the next unanswered call", m.ToolCallID, i)
			}
			open = open[1:]
		case provider.RoleUser:
			if len(open) > 0 {
				return fmt.Errorf("user message at %d arrived before tool call %s was answered", i, open[0].id)
			}
		}
	}
	if len(open) > 0 {
		return fmt.Errorf("tool call %s was not answered", open[0].id)
	}
	return nil
}
