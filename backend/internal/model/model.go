package model

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

type RunStatus string

const (
	RunStatusQueued      RunStatus = "queued"
	RunStatusRunning     RunStatus = "running"
	RunStatusStopping    RunStatus = "stopping"
	RunStatusCompleted   RunStatus = "completed"
	RunStatusFailed      RunStatus = "failed"
	RunStatusStopped     RunStatus = "stopped"
	RunStatusInterrupted RunStatus = "interrupted"
)

func (s RunStatus) IsTerminal() bool {
	return s == RunStatusCompleted || s == RunStatusFailed || s == RunStatusStopped || s == RunStatusInterrupted
}

func (s RunStatus) IsActive() bool {
	return s == RunStatusQueued || s == RunStatusRunning || s == RunStatusStopping
}

// IsAllowedTransition validates state transitions according to the explicit state machine.
// Terminal states cannot transition to any other state.
func IsAllowedTransition(from, to RunStatus) bool {
	switch from {
	case RunStatusQueued:
		return to == RunStatusRunning || to == RunStatusStopping || to == RunStatusStopped || to == RunStatusInterrupted
	case RunStatusRunning:
		return to == RunStatusCompleted || to == RunStatusFailed || to == RunStatusStopping || to == RunStatusInterrupted
	case RunStatusStopping:
		return to == RunStatusStopped || to == RunStatusInterrupted
	default:
		return false
	}
}

type Task struct {
	ID        uuid.UUID       `json:"id"`
	Name      string          `json:"name"`
	Objective string          `json:"objective"`
	Scope     json.RawMessage `json:"scope"`
	Config    json.RawMessage `json:"config"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
}

type Run struct {
	ID             uuid.UUID       `json:"id"`
	TaskID         uuid.UUID       `json:"task_id"`
	IdempotencyKey *string         `json:"idempotency_key,omitempty"`
	ConfigSnapshot json.RawMessage `json:"config_snapshot"`
	ExecutionInput json.RawMessage `json:"execution_input"`
	Status         RunStatus       `json:"status"`
	FinishReason   *string         `json:"finish_reason,omitempty"`
	FailAtStep     *int            `json:"fail_at_step,omitempty"`
	FinalOutput    *string         `json:"final_output,omitempty"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type RunEvent struct {
	ID          uuid.UUID       `json:"id"`
	RunID       uuid.UUID       `json:"run_id"`
	Seq         int             `json:"seq"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload"`
	SessionID   *uuid.UUID      `json:"session_id,omitempty"`
	WorkItemID  *uuid.UUID      `json:"work_item_id,omitempty"`
	AttemptID   *uuid.UUID      `json:"attempt_id,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

type AgentSession struct {
	ID            uuid.UUID       `json:"id"`
	RunID         uuid.UUID       `json:"run_id"`
	Role          string          `json:"role"` // 'planner', 'worker', 'legacy_agent'
	Round         *int            `json:"round,omitempty"`
	WorkItemID    *uuid.UUID      `json:"work_item_id,omitempty"`
	AttemptID     *uuid.UUID      `json:"attempt_id,omitempty"`
	Status        string          `json:"status"` // 'running', 'completed', 'failed', 'stopped', 'interrupted'
	InputSnapshot json.RawMessage `json:"input_snapshot"`
	FinishReason  *string         `json:"finish_reason,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	UpdatedAt     time.Time       `json:"updated_at"`
}

type PlannerRound struct {
	ID            uuid.UUID  `json:"id"`
	RunID         uuid.UUID  `json:"run_id"`
	Round         int        `json:"round"`
	TriggerReason string     `json:"trigger_reason"`
	SessionID     *uuid.UUID `json:"session_id,omitempty"`
	PlanSummary   *string    `json:"plan_summary,omitempty"`
	Status        string     `json:"status"`
	CreatedAt     time.Time  `json:"created_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
}

type WorkItemStatus string

const (
	WorkItemStatusPending   WorkItemStatus = "pending"
	WorkItemStatusRunning   WorkItemStatus = "running"
	WorkItemStatusCompleted WorkItemStatus = "completed"
	WorkItemStatusFailed    WorkItemStatus = "failed"
	WorkItemStatusCancelled WorkItemStatus = "cancelled"
	WorkItemStatusBlocked   WorkItemStatus = "blocked"
)

type WorkItem struct {
	ID               uuid.UUID       `json:"id"`
	RunID            uuid.UUID       `json:"run_id"`
	Key              string          `json:"key"`
	Title            string          `json:"title"`
	Objective        string          `json:"objective"`
	WorkType         string          `json:"work_type"`
	Status           WorkItemStatus  `json:"status"`
	BlockedReason    *string         `json:"blocked_reason,omitempty"`
	InputData        json.RawMessage `json:"input_data"`
	OutputData       json.RawMessage `json:"output_data"`
	CurrentAttemptID *uuid.UUID      `json:"current_attempt_id,omitempty"`
	CreatedAt        time.Time       `json:"created_at"`
	UpdatedAt        time.Time       `json:"updated_at"`
}

type WorkDependency struct {
	ID                  uuid.UUID `json:"id"`
	RunID               uuid.UUID `json:"run_id"`
	WorkItemID          uuid.UUID `json:"work_item_id"`
	DependsOnWorkItemID uuid.UUID `json:"depends_on_work_item_id"`
	CreatedAt           time.Time `json:"created_at"`
}

type Attempt struct {
	ID            uuid.UUID  `json:"id"`
	WorkItemID    uuid.UUID  `json:"work_item_id"`
	AttemptNumber int        `json:"attempt_number"`
	SessionID     *uuid.UUID `json:"session_id,omitempty"`
	Status        string     `json:"status"`
	StartedAt     time.Time  `json:"started_at"`
	FinishedAt    *time.Time `json:"finished_at,omitempty"`
	FinishReason  *string    `json:"finish_reason,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

type Observation struct {
	ID               uuid.UUID       `json:"id"`
	RunID            uuid.UUID       `json:"run_id"`
	AttemptID        uuid.UUID       `json:"attempt_id"`
	ObsType          string          `json:"obs_type"`
	Content          string          `json:"content"`
	Data             json.RawMessage `json:"data"`
	SourceToolCallID uuid.UUID       `json:"source_tool_call_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

type AgentMessage struct {
	ID         uuid.UUID       `json:"id"`
	RunID      uuid.UUID       `json:"run_id"`
	SessionID  *uuid.UUID      `json:"session_id,omitempty"`
	Seq        int             `json:"seq"`
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCallID *string         `json:"tool_call_id,omitempty"`
	ToolCalls  json.RawMessage `json:"tool_calls,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type ModelCall struct {
	ID               uuid.UUID  `json:"id"`
	RunID            uuid.UUID  `json:"run_id"`
	SessionID        *uuid.UUID `json:"session_id,omitempty"`
	Round            int        `json:"round"`
	Status           string     `json:"status"` // started, completed, failed, interrupted
	FinishReason     *string    `json:"finish_reason,omitempty"`
	RequestID        *string    `json:"request_id,omitempty"`
	PromptTokens     int        `json:"prompt_tokens"`
	CompletionTokens int        `json:"completion_tokens"`
	TotalTokens      int        `json:"total_tokens"`
	ErrorMessage     *string    `json:"error_message,omitempty"`
	StartedAt        time.Time  `json:"started_at"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	CreatedAt        time.Time  `json:"created_at"`
}

type AgentToolCall struct {
	ID           uuid.UUID       `json:"id"`
	RunID        uuid.UUID       `json:"run_id"`
	SessionID    *uuid.UUID      `json:"session_id,omitempty"`
	ModelCallID  *uuid.UUID      `json:"model_call_id,omitempty"`
	CallID       string          `json:"call_id"`
	ToolName     string          `json:"tool_name"`
	Arguments    json.RawMessage `json:"arguments"`
	Output       string          `json:"output"`
	Status       string          `json:"status"` // pending, running, success, failed, rejected, interrupted
	ErrorMessage *string         `json:"error_message,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

type BudgetConfig struct {
	MaxModelCalls       int `json:"max_model_calls"`
	MaxToolCalls        int `json:"max_tool_calls"`
	RunTimeoutSeconds   int `json:"run_timeout_seconds"`
	ModelTimeoutSeconds int `json:"model_timeout_seconds"`
	ToolTimeoutSeconds  int `json:"tool_timeout_seconds"`
	MaxToolOutputBytes  int `json:"max_tool_output_bytes"`
	MaxPlannerRounds    int `json:"max_planner_rounds"`
	MaxWorkItems        int `json:"max_work_items"`
	MaxParallelWorkers  int `json:"max_parallel_workers"`
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		MaxModelCalls:       12,
		MaxToolCalls:        16,
		RunTimeoutSeconds:   120,
		ModelTimeoutSeconds: 30,
		ToolTimeoutSeconds:  5,
		MaxToolOutputBytes:  8192,
		MaxPlannerRounds:    8,
		MaxWorkItems:        12,
		MaxParallelWorkers:  1,
	}
}

type CreateTaskRequest struct {
	Name      string          `json:"name"`
	Objective string          `json:"objective"`
	Scope     json.RawMessage `json:"scope,omitempty"`
	Config    json.RawMessage `json:"config,omitempty"`
}

type UpdateTaskRequest struct {
	Name      *string          `json:"name,omitempty"`
	Objective *string          `json:"objective,omitempty"`
	Scope     *json.RawMessage `json:"scope,omitempty"`
	Config    *json.RawMessage `json:"config,omitempty"`
}

type CreateRunRequest struct {
	FailAtStep   *int          `json:"fail_at_step,omitempty"`
	Executor     *string       `json:"executor,omitempty"`      // "fake" or "agent"
	Provider     *string       `json:"provider,omitempty"`      // "mock" or "openai"
	MockScenario *string       `json:"mock_scenario,omitempty"` // e.g. "normal", "multi_tool", etc.
	Budget       *BudgetConfig `json:"budget,omitempty"`
}
