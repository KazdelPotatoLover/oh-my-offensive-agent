package contract

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"
)

const WorkbenchSchemaVersion = "workbench.v1"

// WorkbenchRun is the run header inside a snapshot. Status uses the existing
// run status vocabulary, not a display sentence.
type WorkbenchRun struct {
	ID          uuid.UUID `json:"id"`
	Status      string    `json:"status"`
	EpochNumber int       `json:"epoch_number"`
}

// PlannerView.Status is idle, planning, waiting, needs_input, failed, or stopped.
type PlannerView struct {
	Status    string     `json:"status"`
	Reason    string     `json:"reason,omitempty"`
	SessionID *uuid.UUID `json:"session_id,omitempty"`
}

func (p PlannerView) Validate() error {
	switch p.Status {
	case "idle", "planning", "waiting", "needs_input", "failed", "stopped":
		return nil
	default:
		return invalidInput("unknown planner status")
	}
}

// DirectionView is one stable WorkItem. Attempt and session identify the
// current execution, not a model-call count.
type DirectionView struct {
	WorkItemID     uuid.UUID       `json:"work_item_id"`
	Objective      string          `json:"objective"`
	SourceRefs     []string        `json:"source_refs,omitempty"`
	Status         string          `json:"status"`
	AttemptID      *uuid.UUID      `json:"attempt_id,omitempty"`
	SessionID      *uuid.UUID      `json:"session_id,omitempty"`
	LatestStep     string          `json:"latest_step,omitempty"`
	ProgressAt     *time.Time      `json:"progress_at,omitempty"`
	LocalBudget    json.RawMessage `json:"local_budget,omitempty"`
	Error          *ErrorBody      `json:"error,omitempty"`
	AllowedActions []CommandType   `json:"allowed_actions,omitempty"`
}

func (d DirectionView) Validate() error {
	if d.WorkItemID == uuid.Nil || d.Status == "" {
		return invalidInput("direction requires work_item_id and status")
	}
	for _, action := range d.AllowedActions {
		if !action.Known() || !action.TargetsWorkItem() {
			return invalidInput("direction allowed_actions must be work-item commands")
		}
	}
	return nil
}

type WorkbenchEvent struct {
	Seq     int64           `json:"seq"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

// MainConversationCursor is a page cursor, not the conversation body.
type MainConversationCursor struct {
	LastSeq      int64      `json:"last_seq"`
	ActiveTurnID *uuid.UUID `json:"active_turn_id"`
}

// WorkbenchSnapshot is the workbench.v1 read model. last_event_seq pages events
// only. Clients refresh the snapshot even when the event cursor is unchanged.
type WorkbenchSnapshot struct {
	SchemaVersion    string                 `json:"schema_version"`
	SnapshotAt       time.Time              `json:"snapshot_at"`
	Run              WorkbenchRun           `json:"run"`
	WorkerCapacity   WorkerCapacity         `json:"worker_capacity"`
	ModelCapacity    ModelCapacity          `json:"model_capacity"`
	OriginPacing     *OriginPacing          `json:"origin_pacing,omitempty"`
	Budget           json.RawMessage        `json:"budget"`
	Goals            []json.RawMessage      `json:"goals"`
	Planner          PlannerView            `json:"planner"`
	Directions       []DirectionView        `json:"directions"`
	MainConversation MainConversationCursor `json:"main_conversation"`
	Events           []WorkbenchEvent       `json:"events"`
	LastEventSeq     int64                  `json:"last_event_seq"`
	HasMoreEvents    bool                   `json:"has_more_events"`
}

func (s WorkbenchSnapshot) Validate() error {
	if s.SchemaVersion != WorkbenchSchemaVersion {
		return invalidInput("schema_version must be workbench.v1")
	}
	if s.SnapshotAt.IsZero() || s.Run.ID == uuid.Nil || s.Run.Status == "" || s.Run.EpochNumber < 1 {
		return invalidInput("workbench snapshot requires snapshot_at, run id, status, and epoch")
	}
	if err := s.WorkerCapacity.Validate(); err != nil {
		return err
	}
	if s.ModelCapacity.Limit < 0 || s.ModelCapacity.InFlight < 0 || s.ModelCapacity.Queued < 0 {
		return invalidInput("model capacity counts must be non-negative")
	}
	if err := s.Planner.Validate(); err != nil {
		return err
	}
	for _, direction := range s.Directions {
		if err := direction.Validate(); err != nil {
			return err
		}
	}
	if s.LastEventSeq < 0 || s.MainConversation.LastSeq < 0 {
		return invalidInput("cursors must be non-negative")
	}
	if s.Budget == nil {
		return invalidInput("budget must be present, use an empty object when unknown")
	}
	if s.Goals == nil || s.Events == nil || s.Directions == nil {
		return invalidInput("goals, directions, and events must be arrays")
	}
	return nil
}

type WorkbenchQueryParams struct {
	AfterEventSeq int64
	Limit         int
}

// WorkbenchQuery is the read model. It does not start an engine or mutate a run.
type WorkbenchQuery interface {
	Snapshot(ctx context.Context, runID uuid.UUID, q WorkbenchQueryParams) (*WorkbenchSnapshot, error)
	MainMessages(ctx context.Context, runID uuid.UUID, afterSeq int64, limit int) (*MainMessagePage, error)
}

type MainMessageSubmit struct {
	Message       string `json:"message"`
	ExpectedEpoch int    `json:"expected_epoch"`
}

func (s MainMessageSubmit) Validate() error {
	if strings.TrimSpace(s.Message) == "" {
		return invalidInput("message is required")
	}
	if s.ExpectedEpoch < 1 {
		return invalidInput("expected_epoch must be >= 1")
	}
	return nil
}

type MainMessageAccepted struct {
	MessageID uuid.UUID `json:"message_id"`
	TurnID    uuid.UUID `json:"turn_id"`
	SessionID uuid.UUID `json:"session_id"`
}

// MainMessage.Role is user, assistant, or action. An action must cite a real
// command, model call, tool call, or work item.
type MainMessage struct {
	Seq         int64      `json:"seq"`
	ID          uuid.UUID  `json:"id"`
	Role        string     `json:"role"`
	Text        string     `json:"text,omitempty"`
	TurnID      *uuid.UUID `json:"turn_id,omitempty"`
	TurnStatus  string     `json:"turn_status,omitempty"`
	CommandID   *uuid.UUID `json:"command_id,omitempty"`
	ModelCallID *uuid.UUID `json:"model_call_id,omitempty"`
	ToolCallID  *uuid.UUID `json:"tool_call_id,omitempty"`
	WorkItemID  *uuid.UUID `json:"work_item_id,omitempty"`
}

func (m MainMessage) Validate() error {
	switch m.Role {
	case "user", "assistant", "action":
	default:
		return invalidInput("unknown main message role")
	}
	if m.ID == uuid.Nil || m.Seq < 1 {
		return invalidInput("main message requires id and seq")
	}
	if m.TurnStatus != "" {
		switch m.TurnStatus {
		case "queued", "running", "completed", "failed", "cancelled":
		default:
			return invalidInput("unknown turn_status")
		}
	}
	if m.Role == "action" && m.CommandID == nil && m.ModelCallID == nil && m.ToolCallID == nil && m.WorkItemID == nil {
		return invalidInput("action messages must reference a real command, model call, tool call, or work item")
	}
	return nil
}

type MainMessagePage struct {
	Messages []MainMessage `json:"messages"`
	LastSeq  int64         `json:"last_seq"`
	HasMore  bool          `json:"has_more"`
}
