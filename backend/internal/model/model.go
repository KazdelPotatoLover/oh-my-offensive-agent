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
	RunStatusPaused      RunStatus = "paused"
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
	return s == RunStatusQueued || s == RunStatusRunning || s == RunStatusStopping || s == RunStatusPaused
}

// IsAllowedTransition validates state transitions according to the explicit state machine.
// Terminal states cannot transition to any other state.
func IsAllowedTransition(from, to RunStatus) bool {
	switch from {
	case RunStatusQueued:
		return to == RunStatusRunning || to == RunStatusStopping || to == RunStatusStopped || to == RunStatusInterrupted || to == RunStatusFailed
	case RunStatusRunning:
		return to == RunStatusCompleted || to == RunStatusFailed || to == RunStatusStopping || to == RunStatusInterrupted || to == RunStatusPaused
	case RunStatusPaused:
		return to == RunStatusRunning || to == RunStatusStopping || to == RunStatusStopped || to == RunStatusInterrupted || to == RunStatusFailed
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
	ID                 uuid.UUID       `json:"id"`
	TaskID             uuid.UUID       `json:"task_id"`
	IdempotencyKey     *string         `json:"idempotency_key,omitempty"`
	ConfigSnapshot     json.RawMessage `json:"config_snapshot"`
	ExecutionInput     json.RawMessage `json:"execution_input"`
	ScopePolicyVersion string          `json:"scope_policy_version"`
	CurrentEpoch       int             `json:"current_epoch"`
	Status             RunStatus       `json:"status"`
	FinishReason       *string         `json:"finish_reason,omitempty"`
	FailAtStep         *int            `json:"fail_at_step,omitempty"`
	FinalOutput        *string         `json:"final_output,omitempty"`
	StartedAt          *time.Time      `json:"started_at,omitempty"`
	FinishedAt         *time.Time      `json:"finished_at,omitempty"`
	CreatedAt          time.Time       `json:"created_at"`
	UpdatedAt          time.Time       `json:"updated_at"`
}

type RunEvent struct {
	ID         uuid.UUID       `json:"id"`
	RunID      uuid.UUID       `json:"run_id"`
	Seq        int             `json:"seq"`
	Type       string          `json:"type"`
	Payload    json.RawMessage `json:"payload"`
	SessionID  *uuid.UUID      `json:"session_id,omitempty"`
	WorkItemID *uuid.UUID      `json:"work_item_id,omitempty"`
	AttemptID  *uuid.UUID      `json:"attempt_id,omitempty"`
	CreatedAt  time.Time       `json:"created_at"`
}

type AgentSession struct {
	ID            uuid.UUID       `json:"id"`
	RunID         uuid.UUID       `json:"run_id"`
	Role          string          `json:"role"` // 'planner', 'worker', 'legacy_agent'
	Round         *int            `json:"round,omitempty"`
	WorkItemID    *uuid.UUID      `json:"work_item_id,omitempty"`
	AttemptID     *uuid.UUID      `json:"attempt_id,omitempty"`
	Epoch         int             `json:"epoch"`
	Status        string          `json:"status"` // 'running', 'completed', 'failed', 'exhausted', 'stopped', 'interrupted'
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
	// WorkItemStatusExhausted means local worker budget ended before completion;
	// persisted evidence remains available for planner follow-up.
	WorkItemStatusExhausted WorkItemStatus = "exhausted"
	WorkItemStatusCancelled WorkItemStatus = "cancelled"
	WorkItemStatusBlocked   WorkItemStatus = "blocked"
)

type WorkItem struct {
	ID                      uuid.UUID       `json:"id"`
	RunID                   uuid.UUID       `json:"run_id"`
	Key                     string          `json:"key"`
	Title                   string          `json:"title"`
	Objective               string          `json:"objective"`
	WorkType                string          `json:"work_type"`
	Status                  WorkItemStatus  `json:"status"`
	BlockedReason           *string         `json:"blocked_reason,omitempty"`
	InputData               json.RawMessage `json:"input_data"`
	OutputData              json.RawMessage `json:"output_data"`
	CurrentAttemptID        *uuid.UUID      `json:"current_attempt_id,omitempty"`
	CreatedByPlannerRoundID *uuid.UUID      `json:"created_by_planner_round_id,omitempty"`
	CreatedAt               time.Time       `json:"created_at"`
	UpdatedAt               time.Time       `json:"updated_at"`
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
	Epoch         int        `json:"epoch"`
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
	AssetID          *uuid.UUID      `json:"asset_id,omitempty"`
	ObsType          string          `json:"obs_type"`
	Content          string          `json:"content"`
	Data             json.RawMessage `json:"data"`
	SourceToolCallID uuid.UUID       `json:"source_tool_call_id"`
	CreatedAt        time.Time       `json:"created_at"`
}

type AgentMessage struct {
	ProviderPayload json.RawMessage `json:"-"`
	ID              uuid.UUID       `json:"id"`
	RunID           uuid.UUID       `json:"run_id"`
	SessionID       *uuid.UUID      `json:"session_id,omitempty"`
	Seq             int             `json:"seq"`
	Role            string          `json:"role"`
	Content         string          `json:"content"`
	ToolCallID      *string         `json:"tool_call_id,omitempty"`
	ToolCalls       json.RawMessage `json:"tool_calls,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
}

type ModelCall struct {
	ResponseID       *string    `json:"response_id,omitempty"`
	UsageKnown       bool       `json:"usage_known"`
	ID               uuid.UUID  `json:"id"`
	RunID            uuid.UUID  `json:"run_id"`
	SessionID        *uuid.UUID `json:"session_id,omitempty"`
	Round            int        `json:"round"`
	Epoch            int        `json:"epoch"`
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
	Epoch        int             `json:"epoch"`
	Status       string          `json:"status"` // pending, running, success, failed, rejected, interrupted
	ErrorMessage *string         `json:"error_message,omitempty"`
	StartedAt    *time.Time      `json:"started_at,omitempty"`
	FinishedAt   *time.Time      `json:"finished_at,omitempty"`
	CreatedAt    time.Time       `json:"created_at"`
}

type BudgetConfig struct {
	MaxModelCalls                  int `json:"max_model_calls"`
	MaxToolCalls                   int `json:"max_tool_calls"`
	RunTimeoutSeconds              int `json:"run_timeout_seconds"`
	ModelTimeoutSeconds            int `json:"model_timeout_seconds"`
	ToolTimeoutSeconds             int `json:"tool_timeout_seconds"`
	SummaryTimeoutSeconds          int `json:"summary_timeout_seconds"`
	CleanupTimeoutSeconds          int `json:"cleanup_timeout_seconds"`
	MaxToolOutputBytes             int `json:"max_tool_output_bytes"`
	ContextWindowBytes             int `json:"context_window_bytes"`
	MaxPlannerRounds               int `json:"max_planner_rounds"`
	MaxWorkItems                   int `json:"max_work_items"`
	MaxParallelWorkers             int `json:"max_parallel_workers"`
	WorkerMaxModelCalls            int `json:"worker_max_model_calls"`
	WorkerMaxToolCalls             int `json:"worker_max_tool_calls"`
	WorkerTimeoutSeconds           int `json:"worker_timeout_seconds"`
	WorkerSettlementTimeoutSeconds int `json:"worker_settlement_timeout_seconds"`
	// Optional fields stay absent when zero so older snapshots keep their meaning.
	MaxParallelModelCalls     int `json:"max_parallel_model_calls,omitempty"`
	PlannerReservedModelCalls int `json:"planner_reserved_model_calls,omitempty"`
	MainReservedModelCalls    int `json:"main_reserved_model_calls,omitempty"`
}

// BudgetSummary is the planner/report-facing snapshot. It keeps model rounds
// and tool calls separate; a tool call is never presented as a page count.
type BudgetSummary struct {
	RemainingModelCalls       int `json:"remaining_model_calls"`
	RemainingExplorationCalls int `json:"remaining_exploration_model_calls"`
	RemainingToolCalls        int `json:"remaining_tool_calls"`
	RemainingSeconds          int `json:"remaining_seconds"`
}

func DefaultBudgetConfig() BudgetConfig {
	return BudgetConfig{
		MaxModelCalls:                  30,
		MaxToolCalls:                   30,
		RunTimeoutSeconds:              120,
		ModelTimeoutSeconds:            30,
		ToolTimeoutSeconds:             5,
		SummaryTimeoutSeconds:          30,
		CleanupTimeoutSeconds:          10,
		MaxToolOutputBytes:             8192,
		ContextWindowBytes:             98304,
		MaxPlannerRounds:               8,
		MaxWorkItems:                   12,
		MaxParallelWorkers:             1,
		WorkerMaxModelCalls:            5,
		WorkerMaxToolCalls:             10,
		WorkerTimeoutSeconds:           60,
		WorkerSettlementTimeoutSeconds: 15,
	}
}

// WithOverrides copies positive request fields onto a base budget. Zero means
// the field was omitted and must not wipe a task-config or default value.
func (b BudgetConfig) WithOverrides(override *BudgetConfig) BudgetConfig {
	if override == nil {
		return b
	}
	if override.MaxModelCalls > 0 {
		b.MaxModelCalls = override.MaxModelCalls
	}
	if override.MaxToolCalls > 0 {
		b.MaxToolCalls = override.MaxToolCalls
	}
	if override.RunTimeoutSeconds > 0 {
		b.RunTimeoutSeconds = override.RunTimeoutSeconds
	}
	if override.ModelTimeoutSeconds > 0 {
		b.ModelTimeoutSeconds = override.ModelTimeoutSeconds
	}
	if override.ToolTimeoutSeconds > 0 {
		b.ToolTimeoutSeconds = override.ToolTimeoutSeconds
	}
	if override.SummaryTimeoutSeconds > 0 {
		b.SummaryTimeoutSeconds = override.SummaryTimeoutSeconds
	}
	if override.CleanupTimeoutSeconds > 0 {
		b.CleanupTimeoutSeconds = override.CleanupTimeoutSeconds
	}
	if override.ContextWindowBytes > 0 {
		b.ContextWindowBytes = override.ContextWindowBytes
	}
	if override.MaxToolOutputBytes > 0 {
		b.MaxToolOutputBytes = override.MaxToolOutputBytes
	}
	if override.MaxPlannerRounds > 0 {
		b.MaxPlannerRounds = override.MaxPlannerRounds
	}
	if override.MaxWorkItems > 0 {
		b.MaxWorkItems = override.MaxWorkItems
	}
	if override.MaxParallelWorkers > 0 {
		b.MaxParallelWorkers = override.MaxParallelWorkers
	}
	if override.WorkerMaxModelCalls > 0 {
		b.WorkerMaxModelCalls = override.WorkerMaxModelCalls
	}
	if override.WorkerMaxToolCalls > 0 {
		b.WorkerMaxToolCalls = override.WorkerMaxToolCalls
	}
	if override.WorkerTimeoutSeconds > 0 {
		b.WorkerTimeoutSeconds = override.WorkerTimeoutSeconds
	}
	if override.WorkerSettlementTimeoutSeconds > 0 {
		b.WorkerSettlementTimeoutSeconds = override.WorkerSettlementTimeoutSeconds
	}
	if override.MaxParallelModelCalls > 0 {
		b.MaxParallelModelCalls = override.MaxParallelModelCalls
	}
	if override.PlannerReservedModelCalls > 0 {
		b.PlannerReservedModelCalls = override.PlannerReservedModelCalls
	}
	if override.MainReservedModelCalls > 0 {
		b.MainReservedModelCalls = override.MainReservedModelCalls
	}
	return b
}

// --- ARTEX Alignment Core Models (Epochs, Grants, Dual Graph, Planner Todos) ---

type ExecutionEpoch struct {
	ID           uuid.UUID  `json:"id"`
	RunID        uuid.UUID  `json:"run_id"`
	EpochNumber  int        `json:"epoch_number"`
	Status       string     `json:"status"` // 'running', 'completed', 'failed', 'stopped', 'interrupted'
	StartedAt    time.Time  `json:"started_at"`
	FinishedAt   *time.Time `json:"finished_at,omitempty"`
	FinishReason *string    `json:"finish_reason,omitempty"`
	Summary      *string    `json:"summary,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
}

type BudgetGrant struct {
	ID                   uuid.UUID `json:"id"`
	RunID                uuid.UUID `json:"run_id"`
	EpochNumber          int       `json:"epoch_number"`
	GrantedBy            string    `json:"granted_by"`
	AdditionalModelCalls int       `json:"additional_model_calls"`
	AdditionalToolCalls  int       `json:"additional_tool_calls"`
	AdditionalSeconds    int       `json:"additional_seconds"`
	Reason               string    `json:"reason"`
	IdempotencyKey       *string   `json:"idempotency_key,omitempty"`
	CreatedAt            time.Time `json:"created_at"`
}

type PlannerTodo struct {
	ID          uuid.UUID `json:"id"`
	RunID       uuid.UUID `json:"run_id"`
	ItemKey     string    `json:"item_key"`
	Description string    `json:"description"`
	Status      string    `json:"status"` // 'pending', 'in_progress', 'completed', 'dropped'
	DependsOn   []string  `json:"depends_on"`
	Version     int       `json:"version"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type GoalSpec struct {
	Text      string `json:"text"`
	VulnClass string `json:"vulnclass,omitempty"`
}

type ExplorationNode struct {
	IsNew          bool            `json:"-"`
	ID             uuid.UUID       `json:"id"`
	RunID          uuid.UUID       `json:"run_id"`
	Kind           string          `json:"kind"` // 'begin', 'goal', 'intent', 'fact', 'finding', 'hint', 'digest'
	Payload        json.RawMessage `json:"payload"`
	Priority       int             `json:"priority"`
	State          string          `json:"state"`
	Origin         *string         `json:"origin,omitempty"`
	Owner          *string         `json:"owner,omitempty"`
	BlockedReason  *string         `json:"blocked_reason,omitempty"`
	DeleteReason   *string         `json:"delete_reason,omitempty"`
	WorkItemID     *uuid.UUID      `json:"work_item_id,omitempty"`
	FindingID      *uuid.UUID      `json:"finding_id,omitempty"`
	ContentVersion int             `json:"content_version"`
	CreatedAt      time.Time       `json:"created_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	CompletedAt    *time.Time      `json:"completed_at,omitempty"`
}

type ExplorationEdge struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	SrcID     uuid.UUID `json:"src_id"`
	DstID     uuid.UUID `json:"dst_id"`
	Rel       string    `json:"rel"` // 'spawns', 'derived_from', 'yields', 'proves', 'covers'
	Reason    *string   `json:"reason,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type ExplorationAnchor struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	NodeID    uuid.UUID `json:"node_id"`
	AssetID   uuid.UUID `json:"asset_id"`
	CreatedAt time.Time `json:"created_at"`
}

type AssetRelation struct {
	ID            uuid.UUID       `json:"id"`
	SourceAssetID uuid.UUID       `json:"source_asset_id"`
	TargetAssetID uuid.UUID       `json:"target_asset_id"`
	RelationType  string          `json:"relation_type"`
	Metadata      json.RawMessage `json:"metadata"`
	CreatedAt     time.Time       `json:"created_at"`
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
	Executor     *string       `json:"executor,omitempty"`      // "fake" or "agent" or "orchestrator"
	Provider     *string       `json:"provider,omitempty"`      // "mock" or "openai"
	MockScenario *string       `json:"mock_scenario,omitempty"` // e.g. "normal", "multi_tool", "orchestration", "traceability"
	Budget       *BudgetConfig `json:"budget,omitempty"`
	// ExecutionPolicy distinguishes omit (inherit) from explicit 0 (unlimited)
	// on new exploration runs. It is ignored for legacy non-exploration creates.
	ExecutionPolicy *PolicyOverride `json:"execution_policy,omitempty"`
}

// --- Phase 4: Assets & Traceability Models ---

type Asset struct {
	ID          uuid.UUID       `json:"id"`
	Namespace   string          `json:"namespace"`
	Kind        string          `json:"kind"`
	ExternalKey string          `json:"external_key"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Metadata    json.RawMessage `json:"metadata"`
	CreatedAt   time.Time       `json:"created_at"`
	UpdatedAt   time.Time       `json:"updated_at"`
}

type RunAsset struct {
	ID        uuid.UUID       `json:"id"`
	RunID     uuid.UUID       `json:"run_id"`
	AssetID   uuid.UUID       `json:"asset_id"`
	Snapshot  json.RawMessage `json:"snapshot"`
	IsSeed    bool            `json:"is_seed"`
	Depth     int             `json:"depth"`
	CreatedAt time.Time       `json:"created_at"`
}

type WorkItemAsset struct {
	ID         uuid.UUID `json:"id"`
	WorkItemID uuid.UUID `json:"work_item_id"`
	AssetID    uuid.UUID `json:"asset_id"`
	CreatedAt  time.Time `json:"created_at"`
}

type WorkItemSource struct {
	ID                  uuid.UUID `json:"id"`
	WorkItemID          uuid.UUID `json:"work_item_id"`
	SourceObservationID uuid.UUID `json:"source_observation_id"`
	CreatedAt           time.Time `json:"created_at"`
}

// AssetSummary represents an asset with its usage statistics across runs and observations
type AssetSummary struct {
	Asset
	RunCount         int `json:"run_count"`
	ObservationCount int `json:"observation_count"`
	WorkItemCount    int `json:"work_item_count"`
}

// WorkItemDetail represents a work item enriched with target assets, source observations, and dependencies
type WorkItemDetail struct {
	WorkItem
	TargetAssets       []Asset       `json:"target_assets"`
	SourceObservations []Observation `json:"source_observations"`
	Dependencies       []WorkItem    `json:"dependencies"`
	CreatedRoundNum    *int          `json:"created_round_num,omitempty"`
}

// ObservationDetail represents an observation enriched with its underlying tool call evidence
type ObservationDetail struct {
	Observation
	ToolCall *AgentToolCall `json:"tool_call,omitempty"`
}

// AssetRunHistory represents the work items and observations for an asset within a specific Run
type AssetRunHistory struct {
	RunID        uuid.UUID             `json:"run_id"`
	TaskID       uuid.UUID             `json:"task_id"`
	TaskName     string                `json:"task_name"`
	RunStatus    RunStatus             `json:"run_status"`
	FinishReason *string               `json:"finish_reason,omitempty"`
	StartedAt    *time.Time            `json:"started_at,omitempty"`
	FinishedAt   *time.Time            `json:"finished_at,omitempty"`
	DiscoveredBy *AssetDiscoveryDetail `json:"discovered_by,omitempty"`
	WorkItems    []WorkItemDetail      `json:"work_items"`
	Observations []ObservationDetail   `json:"observations"`
}

// AssetDetailsResponse represents the detailed view of an asset grouped by Run
type AssetDetailsResponse struct {
	Asset      Asset             `json:"asset"`
	RunHistory []AssetRunHistory `json:"run_history"`
}

type DiscoveryPolicy struct {
	Mode              string   `json:"mode,omitempty"` // "exact_paths" or "same_origin_html"
	Origin            string   `json:"origin"`
	AllowedMethods    []string `json:"allowed_methods"`
	AllowedPaths      []string `json:"allowed_paths,omitempty"`
	MaxPages          int      `json:"max_pages,omitempty"`           // default 20
	MaxDepth          int      `json:"max_depth,omitempty"`           // default 2
	RequestIntervalMs int      `json:"request_interval_ms,omitempty"` // default 500ms
}

type AssetDiscovery struct {
	ID               uuid.UUID `json:"id"`
	RunID            uuid.UUID `json:"run_id"`
	SourceAssetID    uuid.UUID `json:"source_asset_id"`
	SourceToolCallID uuid.UUID `json:"source_tool_call_id"`
	TargetAssetID    uuid.UUID `json:"target_asset_id"`
	RawHref          string    `json:"raw_href"`
	DiscoveredAt     time.Time `json:"discovered_at"`
}

type AssetDiscoveryDetail struct {
	AssetDiscovery
	SourceAssetName string `json:"source_asset_name,omitempty"`
	SourceAssetKey  string `json:"source_asset_key,omitempty"`
	TargetAssetName string `json:"target_asset_name,omitempty"`
	TargetAssetKey  string `json:"target_asset_key,omitempty"`
}
