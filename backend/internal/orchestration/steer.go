package orchestration

import (
	"sync"

	"github.com/google/uuid"
)

// ToolBatchDecision tells the session boundary how to treat a tool batch that
// was already proposed when steer arrived. Closing the batch is required so
// the model can re-evaluate. Injecting the instruction and then executing the
// old calls is not a valid decision.
type ToolBatchDecision struct {
	CloseUnexecuted bool
	Instruction     string
	ClosedCalls     []ClosedToolCall
}

// ClosedToolCall is a synthetic result for a call that must not run.
type ClosedToolCall struct {
	ID     string
	Name   string
	Result string
}

// ToolBatchSupervisor is the pre-tool replan port. SessionRunner integration
// belongs to the provider/runtime owner; this type only decides and records.
type ToolBatchSupervisor struct {
	mu       sync.Mutex
	pending  map[uuid.UUID]string
	onDecide func(workItemID uuid.UUID, decision ToolBatchDecision)
}

func NewToolBatchSupervisor() *ToolBatchSupervisor {
	return &ToolBatchSupervisor{pending: make(map[uuid.UUID]string)}
}

func (s *ToolBatchSupervisor) SetDecisionHook(hook func(uuid.UUID, ToolBatchDecision)) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.onDecide = hook
	s.mu.Unlock()
}

// NoteSteer records guidance that must be applied before the next tool batch.
func (s *ToolBatchSupervisor) NoteSteer(workItemID uuid.UUID, instruction string) {
	if s == nil || workItemID == uuid.Nil || instruction == "" {
		return
	}
	s.mu.Lock()
	s.pending[workItemID] = instruction
	s.mu.Unlock()
}

// DecideBeforeTool closes every not-yet-started call in the batch when steer
// is waiting. executed is the number of calls from this batch that already ran.
func (s *ToolBatchSupervisor) DecideBeforeTool(workItemID uuid.UUID, batch []ClosedToolCall, executed int) ToolBatchDecision {
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
	hook := s.onDecide
	s.mu.Unlock()

	decision := ToolBatchDecision{CloseUnexecuted: true, Instruction: instruction}
	if executed < 0 {
		executed = 0
	}
	for i := executed; i < len(batch); i++ {
		call := batch[i]
		call.Result = "tool batch closed before execution because operator steer requires a new model evaluation: " + instruction
		decision.ClosedCalls = append(decision.ClosedCalls, call)
	}
	if hook != nil {
		hook(workItemID, decision)
	}
	return decision
}
