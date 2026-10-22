package contract

import (
	"strings"

	"github.com/google/uuid"
)

// Role is the server-side actor of an execution. It is not accepted from a
// model or browser payload as an authorization claim.
type Role string

const (
	RoleMainAgent Role = "mainagent"
	RolePlanner   Role = "planner"
	RoleWorker    Role = "worker"
	RoleVerifier  Role = "verifier"
)

func (r Role) Known() bool {
	switch r {
	case RoleMainAgent, RolePlanner, RoleWorker, RoleVerifier:
		return true
	default:
		return false
	}
}

// ExecutionScope binds every execution, control effect, receipt, and job.
// SessionID is the current execution session. A stable WorkItem conversation
// may continue in a later Attempt; that does not revive the old tool process.
type ExecutionScope struct {
	RunID            uuid.UUID  `json:"run_id"`
	EpochNumber      int        `json:"epoch_number"`
	Role             Role       `json:"role"`
	WorkItemID       *uuid.UUID `json:"work_item_id,omitempty"`
	AttemptID        *uuid.UUID `json:"attempt_id,omitempty"`
	SessionID        *uuid.UUID `json:"session_id,omitempty"`
	CommandID        *uuid.UUID `json:"command_id,omitempty"`
	SourceToolCallID *uuid.UUID `json:"source_tool_call_id,omitempty"`
	JobID            *uuid.UUID `json:"job_id,omitempty"`
}

// Validate rejects a missing run/epoch and a worker scope that has no
// WorkItem/Attempt. Main, planner, and verifier scopes must not carry a
// forged worker binding; a command target belongs on Command.
func (s ExecutionScope) Validate() error {
	if s.RunID == uuid.Nil {
		return invalidInput("execution scope requires run_id")
	}
	if s.EpochNumber < 1 {
		return invalidInput("execution scope requires epoch_number >= 1")
	}
	if !s.Role.Known() {
		return invalidInput("unknown execution role")
	}
	switch s.Role {
	case RoleWorker:
		if s.WorkItemID == nil || *s.WorkItemID == uuid.Nil || s.AttemptID == nil || *s.AttemptID == uuid.Nil {
			return invalidInput("worker scope requires work_item_id and attempt_id")
		}
	default:
		if s.WorkItemID != nil || s.AttemptID != nil {
			return invalidInput("non-worker scope must not forge work_item_id or attempt_id")
		}
	}
	return nil
}

// PrincipalKind is established by the server call path.
type PrincipalKind string

const (
	PrincipalHuman     PrincipalKind = "human"
	PrincipalMainAgent PrincipalKind = "mainagent"
	PrincipalPlanner   PrincipalKind = "planner"
	PrincipalSystem    PrincipalKind = "system"
)

// Principal is never decoded from a tool or chat JSON body. ExplicitUserCommand
// is set by the HTTP boundary after it verifies the human asked for the action.
type Principal struct {
	Kind                PrincipalKind
	ActorID             string
	RunID               uuid.UUID
	ExplicitUserCommand bool
}

func (p Principal) Validate() error {
	switch p.Kind {
	case PrincipalHuman, PrincipalMainAgent, PrincipalPlanner, PrincipalSystem:
	default:
		return unauthorized("unknown principal")
	}
	if p.RunID == uuid.Nil {
		return invalidInput("principal requires run_id")
	}
	if strings.TrimSpace(p.ActorID) == "" {
		return invalidInput("principal requires actor_id")
	}
	return nil
}

// PrincipalFromRequestJSON always fails. Callers must not promote a JSON role.
func PrincipalFromRequestJSON(_ []byte) (Principal, error) {
	return Principal{}, unauthorized("principal is established by the server")
}
