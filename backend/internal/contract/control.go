package contract

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/google/uuid"
)

const (
	RouteCommands     = "/api/runs/{runID}/commands"
	RouteWorkbench    = "/api/runs/{runID}/workbench"
	RouteMainMessages = "/api/runs/{runID}/main/messages"
)

// CommandType is the unified control vocabulary. Existing tool names
// continue_work_item and retry_work_item map to continue_work and retry_work.
type CommandType string

const (
	CmdHint         CommandType = "hint"
	CmdAddIntent    CommandType = "add_intent"
	CmdSteerWork    CommandType = "steer_work"
	CmdKillWork     CommandType = "kill_work"
	CmdContinueWork CommandType = "continue_work"
	CmdRetryWork    CommandType = "retry_work"
	CmdPauseRun     CommandType = "pause_run"
	CmdResumeRun    CommandType = "resume_run"
	CmdStopRun      CommandType = "stop_run"
	CmdStartEpoch   CommandType = "start_epoch"
	CmdGrantBudget  CommandType = "grant_budget"
)

func (t CommandType) Known() bool {
	switch t {
	case CmdHint, CmdAddIntent, CmdSteerWork, CmdKillWork, CmdContinueWork, CmdRetryWork,
		CmdPauseRun, CmdResumeRun, CmdStopRun, CmdStartEpoch, CmdGrantBudget:
		return true
	default:
		return false
	}
}

func (t CommandType) TargetsWorkItem() bool {
	switch t {
	case CmdSteerWork, CmdKillWork, CmdContinueWork, CmdRetryWork:
		return true
	default:
		return false
	}
}

// CommandStatus values: accepted means persisted, not yet observed by the worker.
type CommandStatus string

const (
	CommandAccepted CommandStatus = "accepted"
	CommandApplied  CommandStatus = "applied"
	CommandRejected CommandStatus = "rejected"
)

// Command is the body of POST /commands. It has no role or principal field.
type Command struct {
	Type              CommandType     `json:"type"`
	ExpectedEpoch     int             `json:"expected_epoch"`
	WorkItemID        *uuid.UUID      `json:"work_item_id,omitempty"`
	ExpectedAttemptID *uuid.UUID      `json:"expected_attempt_id,omitempty"`
	Payload           json.RawMessage `json:"payload,omitempty"`
}

func (c Command) Validate() error {
	if !c.Type.Known() {
		return invalidInput("unknown command type")
	}
	if c.ExpectedEpoch < 1 {
		return invalidInput("expected_epoch must be >= 1")
	}
	if c.Type.TargetsWorkItem() && (c.WorkItemID == nil || *c.WorkItemID == uuid.Nil) {
		return invalidInput("work_item_id is required")
	}
	return nil
}

type SteerPayload struct {
	Instruction string `json:"instruction"`
}

type KillPayload struct {
	Reason string `json:"reason,omitempty"`
}

type HintPayload struct {
	Text string `json:"text"`
}

type AddIntentPayload struct {
	Summary       string      `json:"summary"`
	Priority      int         `json:"priority,omitempty"`
	SourceFactIDs []uuid.UUID `json:"source_fact_ids,omitempty"`
}

type ContinuePayload struct {
	Instruction string `json:"instruction,omitempty"`
}

type StartEpochPayload struct {
	Reason  string `json:"reason"`
	Summary string `json:"summary,omitempty"`
}

type GrantBudgetPayload struct {
	AdditionalModelCalls int `json:"additional_model_calls"`
	AdditionalToolCalls  int `json:"additional_tool_calls"`
	AdditionalSeconds    int `json:"additional_seconds"`
}

// CommandResult is returned for a new or replayed idempotent command.
// Delivered is separate from Status: accepted does not mean the worker has read it.
// Replayed is set only on the response copy of an existing command. It is not
// stored, so the persisted result remains the original apply.
type CommandResult struct {
	CommandID   uuid.UUID       `json:"command_id"`
	Status      CommandStatus   `json:"status"`
	Type        CommandType     `json:"type"`
	EpochNumber int             `json:"epoch_number"`
	WorkItemID  *uuid.UUID      `json:"work_item_id,omitempty"`
	AttemptID   *uuid.UUID      `json:"attempt_id,omitempty"`
	Delivered   bool            `json:"delivered"`
	Replayed    bool            `json:"replayed,omitempty"`
	Result      json.RawMessage `json:"result,omitempty"`
	Error       *ErrorBody      `json:"error,omitempty"`
}

// ExecutionController is the single writer for control commands. REST, planner
// tools, and Main Agent call it. It must not call a model or wait for process
// exit while holding a database lock. There is no second controller.
type ExecutionController interface {
	Apply(ctx context.Context, principal Principal, scope ExecutionScope, idempotencyKey string, cmd Command) (*CommandResult, error)
}

// CommandRequestHash is the idempotency digest. Hash the decoded command, not
// the raw HTTP body. Payload object key order remains significant.
func CommandRequestHash(cmd Command) (string, error) {
	if err := cmd.Validate(); err != nil {
		return "", err
	}
	body, err := json.Marshal(cmd)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:]), nil
}

// MainMessageRequestHash covers the user turn body. The header key is separate.
func MainMessageRequestHash(body MainMessageSubmit) (string, error) {
	if err := body.Validate(); err != nil {
		return "", err
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// RequireIdempotencyKey enforces the header used by commands and main messages.
func RequireIdempotencyKey(key string) error {
	key = strings.TrimSpace(key)
	if key == "" || len(key) > 200 {
		return invalidInput("Idempotency-Key is required")
	}
	return nil
}

// CommandAllowed encodes who may submit a command. grant_budget stays on the
// structured human authorization path. Planner cannot start an epoch, pause,
// resume, stop, or grant budget. Main Agent mutations require ExplicitUserCommand.
func CommandAllowed(p Principal, cmd CommandType) error {
	if err := p.Validate(); err != nil {
		return err
	}
	if !cmd.Known() {
		return invalidInput("unknown command type")
	}
	if cmd == CmdGrantBudget {
		if p.Kind != PrincipalHuman {
			return unauthorized("grant_budget requires structured human authorization")
		}
		return nil
	}
	switch p.Kind {
	case PrincipalHuman:
		return nil
	case PrincipalSystem:
		if cmd == CmdStopRun || cmd == CmdKillWork {
			return nil
		}
		return unauthorized("system principal cannot submit this command")
	case PrincipalPlanner:
		switch cmd {
		case CmdHint, CmdAddIntent, CmdSteerWork, CmdKillWork, CmdContinueWork, CmdRetryWork:
			return nil
		default:
			return unauthorized("planner cannot grant budget, start an epoch, or change run lifecycle")
		}
	case PrincipalMainAgent:
		if !p.ExplicitUserCommand {
			return unauthorized("main agent mutations require an explicit user command")
		}
		if cmd == CmdStartEpoch || cmd == CmdPauseRun || cmd == CmdResumeRun || cmd == CmdStopRun ||
			cmd == CmdHint || cmd == CmdAddIntent || cmd == CmdSteerWork || cmd == CmdKillWork ||
			cmd == CmdContinueWork || cmd == CmdRetryWork {
			return nil
		}
		return unauthorized("main agent cannot apply this command")
	default:
		return unauthorized("unknown principal")
	}
}
