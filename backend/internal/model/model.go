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
	Status         RunStatus       `json:"status"`
	FinishReason   *string         `json:"finish_reason,omitempty"`
	FailAtStep     *int            `json:"fail_at_step,omitempty"`
	StartedAt      *time.Time      `json:"started_at,omitempty"`
	FinishedAt     *time.Time      `json:"finished_at,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
}

type RunEvent struct {
	ID        uuid.UUID       `json:"id"`
	RunID     uuid.UUID       `json:"run_id"`
	Seq       int             `json:"seq"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
	CreatedAt time.Time       `json:"created_at"`
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
	FailAtStep *int `json:"fail_at_step,omitempty"`
}
