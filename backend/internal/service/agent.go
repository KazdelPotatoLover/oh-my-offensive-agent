package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-workbench/backend/internal/checker"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrInvalidObservationSource = errors.New("invalid observation source tool call: not found, mismatched session/run, or tool call did not succeed")
	ErrDependencyCycle          = errors.New("work dependency cycle detected")
	ErrWorkItemConflict         = errors.New("work item key conflict with differing definition")
	ErrAssetOutOfScope          = errors.New("asset is outside run or work item scope")
	ErrAssetSampleMismatch      = errors.New("asset and sample_id mismatch")
	ErrUnsupportedWorkType      = errors.New("unsupported work_type; supported: inspect_http_endpoint, analyze_sample, inspect_sample_endpoint, aggregate_summary, run_security_check")
	ErrUnknownCheckID           = errors.New("unknown check_id: must be a registered check in catalog")
)

// --- Sessions ---

// CreateSession creates a new agent session for a planner or worker.
func (s *RunService) CreateSession(ctx context.Context, runID uuid.UUID, role string, round *int, workItemID, attemptID *uuid.UUID, inputSnapshot json.RawMessage) (*model.AgentSession, error) {
	if inputSnapshot == nil {
		inputSnapshot = json.RawMessage(`{}`)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockRunAdmission(ctx, tx, runID); err != nil {
		return nil, err
	}
	var sess model.AgentSession
	err = tx.QueryRow(ctx,
		`INSERT INTO agent_sessions (id, run_id, role, round, work_item_id, attempt_id, status, input_snapshot)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'running', $6)
		 RETURNING id, run_id, role, round, work_item_id, attempt_id, status, input_snapshot, finish_reason, created_at, updated_at`,
		runID, role, round, workItemID, attemptID, inputSnapshot).Scan(
		&sess.ID, &sess.RunID, &sess.Role, &sess.Round, &sess.WorkItemID, &sess.AttemptID,
		&sess.Status, &sess.InputSnapshot, &sess.FinishReason, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("create agent_session: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &sess, nil
}

// UpdateSessionStatus updates session state and finish reason.
func (s *RunService) UpdateSessionStatus(ctx context.Context, sessionID uuid.UUID, status string, finishReason *string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE agent_sessions SET status = $1, finish_reason = $2, updated_at = now() WHERE id = $3 AND status = 'running'`,
		status, finishReason, sessionID)
	if err != nil {
		return fmt.Errorf("update agent_session: %w", err)
	}
	return nil
}

// GetSession retrieves an agent session by ID.
func (s *RunService) GetSession(ctx context.Context, sessionID uuid.UUID) (*model.AgentSession, error) {
	var sess model.AgentSession
	err := s.pool.QueryRow(ctx,
		`SELECT id, run_id, role, round, work_item_id, attempt_id, status, input_snapshot, finish_reason, created_at, updated_at
		 FROM agent_sessions WHERE id = $1`, sessionID).Scan(
		&sess.ID, &sess.RunID, &sess.Role, &sess.Round, &sess.WorkItemID, &sess.AttemptID,
		&sess.Status, &sess.InputSnapshot, &sess.FinishReason, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get agent_session: %w", err)
	}
	return &sess, nil
}

// ListSessionsByRun retrieves all sessions for a run.
func (s *RunService) ListSessionsByRun(ctx context.Context, runID uuid.UUID) ([]model.AgentSession, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, role, round, work_item_id, attempt_id, status, input_snapshot, finish_reason, created_at, updated_at
		 FROM agent_sessions WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list agent_sessions: %w", err)
	}
	defer rows.Close()

	var sessions []model.AgentSession
	for rows.Next() {
		var sess model.AgentSession
		if err := rows.Scan(&sess.ID, &sess.RunID, &sess.Role, &sess.Round, &sess.WorkItemID, &sess.AttemptID,
			&sess.Status, &sess.InputSnapshot, &sess.FinishReason, &sess.CreatedAt, &sess.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan agent_session: %w", err)
		}
		sessions = append(sessions, sess)
	}
	if sessions == nil {
		sessions = []model.AgentSession{}
	}
	return sessions, nil
}

// --- Messages ---

// InsertAgentMessage inserts a new message in the conversation history for a run.
func (s *RunService) InsertAgentMessage(ctx context.Context, runID uuid.UUID, role string, content string, toolCallID *string, toolCalls json.RawMessage) (*model.AgentMessage, error) {
	return s.InsertAgentMessageWithSession(ctx, runID, nil, role, content, toolCallID, toolCalls)
}

// InsertAgentMessageWithSession inserts a new message scoped to a specific session (or run).
func (s *RunService) InsertAgentMessageWithSession(ctx context.Context, runID uuid.UUID, sessionID *uuid.UUID, role string, content string, toolCallID *string, toolCalls json.RawMessage) (*model.AgentMessage, error) {
	return s.InsertAgentMessageWithPayload(ctx, runID, sessionID, role, content, toolCallID, toolCalls, nil)
}

func (s *RunService) InsertAgentMessageWithPayload(ctx context.Context, runID uuid.UUID, sessionID *uuid.UUID, role string, content string, toolCallID *string, toolCalls, providerPayload json.RawMessage) (*model.AgentMessage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	if err = lockRunAdmission(ctx, tx, runID); err != nil {
		return nil, err
	}

	var nextSeq int
	if sessionID != nil {
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_messages WHERE session_id = $1`, *sessionID).Scan(&nextSeq)
	} else {
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_messages WHERE run_id = $1 AND session_id IS NULL`, runID).Scan(&nextSeq)
	}
	if err != nil {
		return nil, fmt.Errorf("get next seq for agent_messages: %w", err)
	}

	var msg model.AgentMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO agent_messages (id, run_id, session_id, seq, role, content, tool_call_id, tool_calls, provider_payload)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, run_id, session_id, seq, role, content, tool_call_id, tool_calls, provider_payload, created_at`,
		runID, sessionID, nextSeq, role, content, toolCallID, toolCalls, providerPayload).Scan(
		&msg.ID, &msg.RunID, &msg.SessionID, &msg.Seq, &msg.Role, &msg.Content, &msg.ToolCallID, &msg.ToolCalls, &msg.ProviderPayload, &msg.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert agent_message: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit agent_message: %w", err)
	}
	return &msg, nil
}

// ListAgentMessages retrieves all conversation messages for a run in sequence order.
func (s *RunService) ListAgentMessages(ctx context.Context, runID uuid.UUID) ([]model.AgentMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, session_id, seq, role, content, tool_call_id, tool_calls, provider_payload, created_at
		 FROM agent_messages WHERE run_id = $1 ORDER BY created_at ASC, seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query agent_messages: %w", err)
	}
	defer rows.Close()

	var msgs []model.AgentMessage
	for rows.Next() {
		var m model.AgentMessage
		if err := rows.Scan(&m.ID, &m.RunID, &m.SessionID, &m.Seq, &m.Role, &m.Content, &m.ToolCallID, &m.ToolCalls, &m.ProviderPayload, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan agent_message: %w", err)
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []model.AgentMessage{}
	}
	return msgs, nil
}

// ListAgentMessagesBySession retrieves messages scoped to a session.
func (s *RunService) ListAgentMessagesBySession(ctx context.Context, sessionID uuid.UUID) ([]model.AgentMessage, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, session_id, seq, role, content, tool_call_id, tool_calls, provider_payload, created_at
		 FROM agent_messages WHERE session_id = $1 ORDER BY seq ASC`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("query session messages: %w", err)
	}
	defer rows.Close()

	var msgs []model.AgentMessage
	for rows.Next() {
		var m model.AgentMessage
		if err := rows.Scan(&m.ID, &m.RunID, &m.SessionID, &m.Seq, &m.Role, &m.Content, &m.ToolCallID, &m.ToolCalls, &m.ProviderPayload, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan session message: %w", err)
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []model.AgentMessage{}
	}
	return msgs, nil
}

// LoadSessionConversation reconstructs only this session's persisted conversation.
// Native provider state stays internal and is validated before reuse.
func (s *RunService) LoadSessionConversation(ctx context.Context, sessionID uuid.UUID) ([]provider.ChatMessage, error) {
	records, err := s.ListAgentMessagesBySession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	messages := make([]provider.ChatMessage, 0, len(records))
	for _, record := range records {
		m := provider.ChatMessage{Role: provider.ChatRole(record.Role), Content: record.Content}
		if record.ToolCallID != nil {
			m.ToolCallID = *record.ToolCallID
		}
		if len(record.ToolCalls) > 0 {
			if err := json.Unmarshal(record.ToolCalls, &m.ToolCalls); err != nil {
				return nil, fmt.Errorf("decode persisted tool calls: %w", err)
			}
		}
		if len(record.ProviderPayload) > 0 {
			if err := json.Unmarshal(record.ProviderPayload, &m.NativePayload); err != nil {
				return nil, fmt.Errorf("decode persisted provider payload: %w", err)
			}
		}
		messages = append(messages, m)
	}
	return messages, nil
}

// --- Model Calls ---

// CreateModelCall records the start of a model invocation.
func (s *RunService) CreateModelCall(ctx context.Context, runID uuid.UUID, round int) (*model.ModelCall, error) {
	return s.CreateModelCallWithSession(ctx, runID, nil, round)
}

// CreateModelCallWithSession records the start of a model invocation within a session.
func (s *RunService) CreateModelCallWithSession(ctx context.Context, runID uuid.UUID, sessionID *uuid.UUID, round int) (*model.ModelCall, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockRunAdmission(ctx, tx, runID); err != nil {
		return nil, err
	}
	var call model.ModelCall
	err = tx.QueryRow(ctx,
		`INSERT INTO model_calls (id, run_id, session_id, round, status, started_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, 'started', now())
		 RETURNING id, run_id, session_id, round, status, finish_reason, request_id, response_id, usage_known, prompt_tokens, completion_tokens,
		           total_tokens, error_message, started_at, finished_at, created_at`,
		runID, sessionID, round).Scan(
		&call.ID, &call.RunID, &call.SessionID, &call.Round, &call.Status, &call.FinishReason, &call.RequestID, &call.ResponseID, &call.UsageKnown,
		&call.PromptTokens, &call.CompletionTokens, &call.TotalTokens, &call.ErrorMessage,
		&call.StartedAt, &call.FinishedAt, &call.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert model_call: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &call, nil
}

// UpdateModelCall updates a model call record upon completion or failure.
func (s *RunService) UpdateModelCall(ctx context.Context, id uuid.UUID, status string, finishReason *string, requestID *string, promptTokens, completionTokens, totalTokens int, errMsg *string) error {
	return s.UpdateModelCallWithMetadata(ctx, id, status, finishReason, requestID, nil, false, promptTokens, completionTokens, totalTokens, errMsg)
}

func (s *RunService) UpdateModelCallWithMetadata(ctx context.Context, id uuid.UUID, status string, finishReason, requestID, responseID *string, usageKnown bool, promptTokens, completionTokens, totalTokens int, errMsg *string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE model_calls
		 SET status = $1, finish_reason = $2, request_id = $3, prompt_tokens = $4,
		     completion_tokens = $5, total_tokens = $6, error_message = $7, finished_at = now(), response_id = $9, usage_known = $10
		 WHERE id = $8 AND status = 'started'`,
		status, finishReason, requestID, promptTokens, completionTokens, totalTokens, errMsg, id, responseID, usageKnown)
	if err != nil {
		return fmt.Errorf("update model_call: %w", err)
	}
	return nil
}

// ListModelCalls retrieves all model calls for a run.
func (s *RunService) ListModelCalls(ctx context.Context, runID uuid.UUID) ([]model.ModelCall, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, session_id, round, status, finish_reason, request_id, response_id, usage_known, prompt_tokens, completion_tokens,
		        total_tokens, error_message, started_at, finished_at, created_at
		 FROM model_calls WHERE run_id = $1 ORDER BY round ASC, created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query model_calls: %w", err)
	}
	defer rows.Close()

	var calls []model.ModelCall
	for rows.Next() {
		var c model.ModelCall
		if err := rows.Scan(&c.ID, &c.RunID, &c.SessionID, &c.Round, &c.Status, &c.FinishReason, &c.RequestID, &c.ResponseID, &c.UsageKnown,
			&c.PromptTokens, &c.CompletionTokens, &c.TotalTokens, &c.ErrorMessage,
			&c.StartedAt, &c.FinishedAt, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan model_call: %w", err)
		}
		calls = append(calls, c)
	}
	if calls == nil {
		calls = []model.ModelCall{}
	}
	return calls, nil
}

// --- Tool Calls ---

// CreateAgentToolCall records a tool request from the model.
func (s *RunService) CreateAgentToolCall(ctx context.Context, runID uuid.UUID, modelCallID *uuid.UUID, callID string, toolName string, arguments json.RawMessage) (*model.AgentToolCall, error) {
	return s.CreateAgentToolCallWithSession(ctx, runID, nil, modelCallID, callID, toolName, arguments)
}

// CreateAgentToolCallWithSession records a tool request scoped to a session.
func (s *RunService) CreateAgentToolCallWithSession(ctx context.Context, runID uuid.UUID, sessionID *uuid.UUID, modelCallID *uuid.UUID, callID string, toolName string, arguments json.RawMessage) (*model.AgentToolCall, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockRunAdmission(ctx, tx, runID); err != nil {
		return nil, err
	}
	var tc model.AgentToolCall
	err = tx.QueryRow(ctx,
		`INSERT INTO agent_tool_calls (id, run_id, session_id, model_call_id, call_id, tool_name, arguments, status)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, 'pending')
		 RETURNING id, run_id, session_id, model_call_id, call_id, tool_name, arguments, output, status,
		           error_message, started_at, finished_at, created_at`,
		runID, sessionID, modelCallID, callID, toolName, arguments).Scan(
		&tc.ID, &tc.RunID, &tc.SessionID, &tc.ModelCallID, &tc.CallID, &tc.ToolName, &tc.Arguments, &tc.Output,
		&tc.Status, &tc.ErrorMessage, &tc.StartedAt, &tc.FinishedAt, &tc.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert agent_tool_call: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &tc, nil
}

// UpdateAgentToolCall updates status, output, and error for a tool call.
func (s *RunService) UpdateAgentToolCall(ctx context.Context, id uuid.UUID, status string, output string, errMsg *string, startedAt, finishedAt *time.Time) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE agent_tool_calls
		 SET status = $1, output = $2, error_message = $3,
		     started_at = COALESCE($4, started_at),
		     finished_at = COALESCE($5, finished_at)
		 WHERE id = $6 AND status IN ('pending', 'running')`,
		status, output, errMsg, startedAt, finishedAt, id)
	if err != nil {
		return fmt.Errorf("update agent_tool_call: %w", err)
	}
	return nil
}

// ListAgentToolCalls retrieves all tool calls for a run.
func (s *RunService) ListAgentToolCalls(ctx context.Context, runID uuid.UUID) ([]model.AgentToolCall, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, session_id, model_call_id, call_id, tool_name, arguments, output, status,
		        error_message, started_at, finished_at, created_at
		 FROM agent_tool_calls WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query agent_tool_calls: %w", err)
	}
	defer rows.Close()

	var calls []model.AgentToolCall
	for rows.Next() {
		var c model.AgentToolCall
		if err := rows.Scan(&c.ID, &c.RunID, &c.SessionID, &c.ModelCallID, &c.CallID, &c.ToolName, &c.Arguments,
			&c.Output, &c.Status, &c.ErrorMessage, &c.StartedAt, &c.FinishedAt, &c.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan agent_tool_call: %w", err)
		}
		calls = append(calls, c)
	}
	if calls == nil {
		calls = []model.AgentToolCall{}
	}
	return calls, nil
}

// --- Planner Rounds ---

// CreatePlannerRound creates a new round of planning.
func (s *RunService) CreatePlannerRound(ctx context.Context, runID uuid.UUID, round int, triggerReason string, sessionID *uuid.UUID) (*model.PlannerRound, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if err = lockRunAdmission(ctx, tx, runID); err != nil {
		return nil, err
	}
	var pr model.PlannerRound
	err = tx.QueryRow(ctx,
		`INSERT INTO planner_rounds (id, run_id, round, trigger_reason, session_id, status)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, 'running')
		 RETURNING id, run_id, round, trigger_reason, session_id, plan_summary, status, created_at, finished_at`,
		runID, round, triggerReason, sessionID).Scan(
		&pr.ID, &pr.RunID, &pr.Round, &pr.TriggerReason, &pr.SessionID, &pr.PlanSummary, &pr.Status,
		&pr.CreatedAt, &pr.FinishedAt)
	if err != nil {
		return nil, fmt.Errorf("create planner_round: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &pr, nil
}

// UpdatePlannerRound updates a planner round status and summary.
func (s *RunService) UpdatePlannerRound(ctx context.Context, id uuid.UUID, status string, planSummary *string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE planner_rounds SET status = $1, plan_summary = $2, finished_at = now() WHERE id = $3 AND status = 'running'`,
		status, planSummary, id)
	if err != nil {
		return fmt.Errorf("update planner_round: %w", err)
	}
	return nil
}

// ListPlannerRounds retrieves all planner rounds for a run.
func (s *RunService) ListPlannerRounds(ctx context.Context, runID uuid.UUID) ([]model.PlannerRound, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, round, trigger_reason, session_id, plan_summary, status, created_at, finished_at
		 FROM planner_rounds WHERE run_id = $1 ORDER BY round ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list planner_rounds: %w", err)
	}
	defer rows.Close()

	var rounds []model.PlannerRound
	for rows.Next() {
		var pr model.PlannerRound
		if err := rows.Scan(&pr.ID, &pr.RunID, &pr.Round, &pr.TriggerReason, &pr.SessionID, &pr.PlanSummary,
			&pr.Status, &pr.CreatedAt, &pr.FinishedAt); err != nil {
			return nil, fmt.Errorf("scan planner_round: %w", err)
		}
		rounds = append(rounds, pr)
	}
	if rounds == nil {
		rounds = []model.PlannerRound{}
	}
	return rounds, nil
}

// --- Work Items & Dependencies ---

type PlanItemSpec struct {
	Key                  string                 `json:"key"`
	Title                string                 `json:"title"`
	Objective            string                 `json:"objective"`
	WorkType             string                 `json:"work_type"`
	InputData            map[string]interface{} `json:"input_data"`
	Dependencies         []string               `json:"dependencies"`           // keys of dependencies
	AssetIDs             []uuid.UUID            `json:"asset_ids"`              // target assets
	SourceObservationIDs []uuid.UUID            `json:"source_observation_ids"` // causal source observations
}

// SubmitPlan atomically validates and inserts a batch of planned work items and dependencies.
func (s *RunService) SubmitPlan(ctx context.Context, runID uuid.UUID, planItems []PlanItemSpec) ([]model.WorkItem, error) {
	return s.SubmitPlanWithRound(ctx, runID, nil, planItems)
}

// SubmitPlanWithRound atomically validates and inserts planned work items with planner_round_id attribution.
func (s *RunService) SubmitPlanWithRound(ctx context.Context, runID uuid.UUID, plannerRoundID *uuid.UUID, planItems []PlanItemSpec) ([]model.WorkItem, error) {
	if len(planItems) == 0 {
		return []model.WorkItem{}, nil
	}
	// Preserve the legacy default, but never silently route an unknown type to samples.
	planItems = append([]PlanItemSpec(nil), planItems...)
	for i := range planItems {
		if planItems[i].WorkType == "" {
			planItems[i].WorkType = "analyze_sample"
		}
		if !model.IsSupportedWorkType(planItems[i].WorkType) {
			return nil, fmt.Errorf("%w: item %q requested %q", ErrUnsupportedWorkType, planItems[i].Key, planItems[i].WorkType)
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Check that run is still running
	var runStatus model.RunStatus
	var scopePolicyVersion string
	err = tx.QueryRow(ctx, `SELECT status, scope_policy_version FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus, &scopePolicyVersion)
	if err != nil {
		return nil, fmt.Errorf("check run status: %w", err)
	}
	if runStatus != model.RunStatusRunning {
		return nil, fmt.Errorf("cannot submit plan: run status is %s", runStatus)
	}

	// Load allowed assets for this run
	assetRows, err := tx.Query(ctx,
		`SELECT a.id, a.external_key, a.kind
		 FROM assets a 
		 JOIN run_assets ra ON a.id = ra.asset_id 
		 WHERE ra.run_id = $1`, runID)
	if err != nil {
		return nil, fmt.Errorf("query run assets: %w", err)
	}
	runAssetMap := make(map[uuid.UUID]string)
	runAssetKinds := make(map[uuid.UUID]string)
	for assetRows.Next() {
		var aid uuid.UUID
		var aKey, aKind string
		if err := assetRows.Scan(&aid, &aKey, &aKind); err == nil {
			runAssetMap[aid] = aKey
			runAssetKinds[aid] = aKind
		}
	}
	assetRows.Close()

	// Load existing observations for this run
	obsRows, err := tx.Query(ctx, `SELECT id FROM observations WHERE run_id = $1`, runID)
	if err != nil {
		return nil, fmt.Errorf("query run observations: %w", err)
	}
	runObsMap := make(map[uuid.UUID]bool)
	for obsRows.Next() {
		var oid uuid.UUID
		if err := obsRows.Scan(&oid); err == nil {
			runObsMap[oid] = true
		}
	}
	obsRows.Close()

	// Load existing work items for this run
	existingRows, err := tx.Query(ctx,
		`SELECT id, key, title, objective, work_type, input_data, created_by_planner_round_id 
		 FROM work_items WHERE run_id = $1`, runID)
	if err != nil {
		return nil, fmt.Errorf("query existing work_items: %w", err)
	}
	keyToID := make(map[string]uuid.UUID)
	keyToItem := make(map[string]model.WorkItem)
	for existingRows.Next() {
		var wi model.WorkItem
		if err := existingRows.Scan(&wi.ID, &wi.Key, &wi.Title, &wi.Objective, &wi.WorkType, &wi.InputData, &wi.CreatedByPlannerRoundID); err != nil {
			existingRows.Close()
			return nil, fmt.Errorf("scan existing work_item: %w", err)
		}
		keyToID[wi.Key] = wi.ID
		keyToItem[wi.Key] = wi
	}
	existingRows.Close()

	// Validate keys in this batch for duplicates, asset scope, observation source, and conflict
	batchKeys := make(map[string]bool)
	for _, item := range planItems {
		if item.Key == "" {
			return nil, fmt.Errorf("work item key cannot be empty")
		}
		if batchKeys[item.Key] {
			return nil, fmt.Errorf("duplicate key %s in same submit_plan batch", item.Key)
		}
		batchKeys[item.Key] = true
		if item.WorkType == "analyze_sample" || item.WorkType == "inspect_sample_endpoint" {
			hasSampleAsset := false
			for _, kind := range runAssetKinds {
				if kind == "sample_endpoint" {
					hasSampleAsset = true
					break
				}
			}
			if len(runAssetKinds) > 0 && !hasSampleAsset {
				return nil, fmt.Errorf("sample analysis cannot execute in an HTTP-only run; use inspect_http_endpoint: %w", ErrAssetOutOfScope)
			}
		}

		// Validate asset_ids: must belong to run_assets
		if len(item.AssetIDs) > 0 {
			for _, aid := range item.AssetIDs {
				if _, ok := runAssetMap[aid]; !ok {
					return nil, fmt.Errorf("asset %s is outside run asset scope: %w", aid, ErrAssetOutOfScope)
				}
				if (item.WorkType == "inspect_http_endpoint" || item.WorkType == "run_security_check") && runAssetKinds[aid] != "http_endpoint" {
					return nil, fmt.Errorf("HTTP inspection and security checks require http_endpoint assets: %w", ErrAssetOutOfScope)
				}
				if (item.WorkType == "analyze_sample" || item.WorkType == "inspect_sample_endpoint") && runAssetKinds[aid] != "sample_endpoint" {
					return nil, fmt.Errorf("sample analysis requires sample_endpoint assets; use inspect_http_endpoint for HTTP targets: %w", ErrAssetOutOfScope)
				}
			}
			// Validate sample_id consistency with asset_ids if sample_id provided
			if sampleID, ok := item.InputData["sample_id"].(string); ok && sampleID != "" {
				matched := false
				for _, aid := range item.AssetIDs {
					if runAssetMap[aid] == sampleID {
						matched = true
						break
					}
				}
				if !matched {
					return nil, fmt.Errorf("sample_id '%s' does not match any target asset: %w", sampleID, ErrAssetSampleMismatch)
				}
			}
		} else if scopePolicyVersion != "legacy" && (item.WorkType == "inspect_http_endpoint" || item.WorkType == "inspect_sample_endpoint") {
			return nil, fmt.Errorf("work item '%s' of type '%s' requires a target asset: %w", item.Key, item.WorkType, ErrAssetOutOfScope)
		}
		if item.WorkType == "inspect_http_endpoint" {
			if len(item.AssetIDs) == 0 {
				return nil, fmt.Errorf("inspect_http_endpoint requires asset_ids: %w", ErrAssetOutOfScope)
			}
			rawID, _ := item.InputData["asset_id"].(string)
			targetID, parseErr := uuid.Parse(rawID)
			matched := false
			for _, aid := range item.AssetIDs {
				if aid == targetID {
					matched = true
				}
			}
			if parseErr != nil || !matched {
				return nil, fmt.Errorf("inspect_http_endpoint input_data.asset_id must be a UUID in asset_ids: %w", ErrAssetOutOfScope)
			}
		}
		if item.WorkType == "run_security_check" {
			if len(item.AssetIDs) == 0 {
				return nil, fmt.Errorf("run_security_check requires asset_ids: %w", ErrAssetOutOfScope)
			}
			rawID, _ := item.InputData["asset_id"].(string)
			targetID, parseErr := uuid.Parse(rawID)
			matched := false
			for _, aid := range item.AssetIDs {
				if aid == targetID {
					matched = true
				}
			}
			if parseErr != nil || !matched {
				return nil, fmt.Errorf("run_security_check input_data.asset_id must be a UUID in asset_ids: %w", ErrAssetOutOfScope)
			}
			rawCheckID, _ := item.InputData["check_id"].(string)
			rawCheckID = strings.TrimSpace(rawCheckID)
			if rawCheckID == "" {
				return nil, fmt.Errorf("run_security_check requires input_data.check_id: %w", ErrUnknownCheckID)
			}
			if !checker.IsRegisteredCheck(rawCheckID) {
				return nil, fmt.Errorf("%w: %q", ErrUnknownCheckID, rawCheckID)
			}
		}

		// Validate source_observation_ids: must belong to existing observations in this run
		for _, oid := range item.SourceObservationIDs {
			if !runObsMap[oid] {
				return nil, fmt.Errorf("source observation %s does not exist in run: %w", oid, ErrInvalidObservationSource)
			}
		}

		// Check conflict with existing item: if same key exists, check definition & sets equality
		if existing, exists := keyToItem[item.Key]; exists {
			// Title & Objective match
			if existing.Title != item.Title || existing.Objective != item.Objective {
				return nil, fmt.Errorf("%w: key %s already exists with different definition", ErrWorkItemConflict, item.Key)
			}

			// Asset set equality
			existingAssetRows, err := tx.Query(ctx, `SELECT asset_id FROM work_item_assets WHERE work_item_id = $1`, existing.ID)
			if err != nil {
				return nil, fmt.Errorf("query existing work_item_assets: %w", err)
			}
			var existingAssets []uuid.UUID
			for existingAssetRows.Next() {
				var aid uuid.UUID
				if err := existingAssetRows.Scan(&aid); err == nil {
					existingAssets = append(existingAssets, aid)
				}
			}
			existingAssetRows.Close()
			if !uuidSetsEqual(existingAssets, item.AssetIDs) {
				return nil, fmt.Errorf("%w: key %s already exists with different asset set", ErrWorkItemConflict, item.Key)
			}

			// Source observation set equality
			existingSourceRows, err := tx.Query(ctx, `SELECT source_observation_id FROM work_item_sources WHERE work_item_id = $1`, existing.ID)
			if err != nil {
				return nil, fmt.Errorf("query existing work_item_sources: %w", err)
			}
			var existingSources []uuid.UUID
			for existingSourceRows.Next() {
				var soid uuid.UUID
				if err := existingSourceRows.Scan(&soid); err == nil {
					existingSources = append(existingSources, soid)
				}
			}
			existingSourceRows.Close()
			if !uuidSetsEqual(existingSources, item.SourceObservationIDs) {
				return nil, fmt.Errorf("%w: key %s already exists with different source observations", ErrWorkItemConflict, item.Key)
			}

			// Dependency set equality
			existingDepRows, err := tx.Query(ctx,
				`SELECT w2.key 
				 FROM work_dependencies wd
				 JOIN work_items w2 ON wd.depends_on_work_item_id = w2.id
				 WHERE wd.work_item_id = $1`, existing.ID)
			if err != nil {
				return nil, fmt.Errorf("query existing work_dependencies: %w", err)
			}
			var existingDeps []string
			for existingDepRows.Next() {
				var dKey string
				if err := existingDepRows.Scan(&dKey); err == nil {
					existingDeps = append(existingDeps, dKey)
				}
			}
			existingDepRows.Close()
			if !stringSetsEqual(existingDeps, item.Dependencies) {
				return nil, fmt.Errorf("%w: key %s already exists with different dependencies", ErrWorkItemConflict, item.Key)
			}
		}
	}

	// Validate dependency keys
	for _, item := range planItems {
		for _, depKey := range item.Dependencies {
			if depKey == item.Key {
				return nil, fmt.Errorf("work item %s cannot depend on itself", item.Key)
			}
			if !batchKeys[depKey] && keyToID[depKey] == uuid.Nil {
				return nil, fmt.Errorf("dependency %s for item %s does not exist in run or current batch", depKey, item.Key)
			}
		}
	}

	// Check dependency cycle across all items (existing + new)
	adj := make(map[string][]string)
	// Add existing dependencies
	depRows, err := tx.Query(ctx,
		`SELECT w1.key, w2.key 
		 FROM work_dependencies wd
		 JOIN work_items w1 ON wd.work_item_id = w1.id
		 JOIN work_items w2 ON wd.depends_on_work_item_id = w2.id
		 WHERE wd.run_id = $1`, runID)
	if err != nil {
		return nil, fmt.Errorf("query existing dependencies: %w", err)
	}
	for depRows.Next() {
		var fromKey, toKey string
		if err := depRows.Scan(&fromKey, &toKey); err == nil {
			adj[fromKey] = append(adj[fromKey], toKey)
		}
	}
	depRows.Close()

	// Add batch dependencies
	for _, item := range planItems {
		adj[item.Key] = append(adj[item.Key], item.Dependencies...)
	}

	// Topological cycle check
	visited := make(map[string]int) // 0: unvisited, 1: visiting, 2: visited
	var hasCycle func(string) bool
	hasCycle = func(node string) bool {
		visited[node] = 1
		for _, next := range adj[node] {
			if visited[next] == 1 {
				return true
			}
			if visited[next] == 0 {
				if hasCycle(next) {
					return true
				}
			}
		}
		visited[node] = 2
		return false
	}

	for k := range adj {
		if visited[k] == 0 {
			if hasCycle(k) {
				return nil, ErrDependencyCycle
			}
		}
	}

	// Pass 1: Insert new work items, asset links, and source links
	var resultItems []model.WorkItem
	for _, item := range planItems {
		var itemID uuid.UUID
		if existing, exists := keyToItem[item.Key]; exists {
			itemID = existing.ID
			resultItems = append(resultItems, existing)
		} else {
			inBytes, _ := json.Marshal(item.InputData)
			if inBytes == nil {
				inBytes = json.RawMessage(`{}`)
			}
			workType := item.WorkType
			if workType == "" {
				workType = "analyze_sample"
			}

			var wi model.WorkItem
			err = tx.QueryRow(ctx,
				`INSERT INTO work_items (id, run_id, key, title, objective, work_type, status, input_data, created_by_planner_round_id)
				 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'pending', $6, $7)
				 RETURNING id, run_id, key, title, objective, work_type, status, blocked_reason, input_data,
				           output_data, current_attempt_id, created_by_planner_round_id, created_at, updated_at`,
				runID, item.Key, item.Title, item.Objective, workType, inBytes, plannerRoundID).Scan(
				&wi.ID, &wi.RunID, &wi.Key, &wi.Title, &wi.Objective, &wi.WorkType, &wi.Status,
				&wi.BlockedReason, &wi.InputData, &wi.OutputData, &wi.CurrentAttemptID,
				&wi.CreatedByPlannerRoundID, &wi.CreatedAt, &wi.UpdatedAt)
			if err != nil {
				return nil, fmt.Errorf("insert work_item %s: %w", item.Key, err)
			}
			itemID = wi.ID
			keyToID[item.Key] = itemID
			resultItems = append(resultItems, wi)

			// Insert work_item_assets
			for _, aid := range item.AssetIDs {
				_, err = tx.Exec(ctx,
					`INSERT INTO work_item_assets (id, work_item_id, asset_id, created_at)
					 VALUES (gen_random_uuid(), $1, $2, now())
					 ON CONFLICT (work_item_id, asset_id) DO NOTHING`,
					itemID, aid)
				if err != nil {
					return nil, fmt.Errorf("insert work_item_asset for %s: %w", item.Key, err)
				}
			}

			// Insert work_item_sources
			for _, soid := range item.SourceObservationIDs {
				_, err = tx.Exec(ctx,
					`INSERT INTO work_item_sources (id, work_item_id, source_observation_id, created_at)
					 VALUES (gen_random_uuid(), $1, $2, now())
					 ON CONFLICT (work_item_id, source_observation_id) DO NOTHING`,
					itemID, soid)
				if err != nil {
					return nil, fmt.Errorf("insert work_item_source for %s: %w", item.Key, err)
				}
			}
		}
	}

	// Pass 2: Insert dependencies
	for _, item := range planItems {
		itemID := keyToID[item.Key]
		for _, depKey := range item.Dependencies {
			depID := keyToID[depKey]
			_, err = tx.Exec(ctx,
				`INSERT INTO work_dependencies (id, run_id, work_item_id, depends_on_work_item_id)
				 VALUES (gen_random_uuid(), $1, $2, $3)
				 ON CONFLICT (work_item_id, depends_on_work_item_id) DO NOTHING`,
				runID, itemID, depID)
			if err != nil {
				return nil, fmt.Errorf("insert dependency for %s -> %s: %w", item.Key, depKey, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit submit_plan: %w", err)
	}

	return resultItems, nil
}

func uuidSetsEqual(a, b []uuid.UUID) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[uuid.UUID]bool)
	for _, id := range a {
		m[id] = true
	}
	for _, id := range b {
		if !m[id] {
			return false
		}
	}
	return true
}

func stringSetsEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	m := make(map[string]bool)
	for _, s := range a {
		m[s] = true
	}
	for _, s := range b {
		if !m[s] {
			return false
		}
	}
	return true
}

// ListWorkItemsByRun retrieves all work items for a run.
func (s *RunService) ListWorkItemsByRun(ctx context.Context, runID uuid.UUID) ([]model.WorkItem, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, key, title, objective, work_type, status, blocked_reason, input_data,
		        output_data, current_attempt_id, created_by_planner_round_id, created_at, updated_at
		 FROM work_items WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list work_items: %w", err)
	}
	defer rows.Close()

	var items []model.WorkItem
	for rows.Next() {
		var wi model.WorkItem
		if err := rows.Scan(&wi.ID, &wi.RunID, &wi.Key, &wi.Title, &wi.Objective, &wi.WorkType, &wi.Status,
			&wi.BlockedReason, &wi.InputData, &wi.OutputData, &wi.CurrentAttemptID, &wi.CreatedByPlannerRoundID, &wi.CreatedAt, &wi.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan work_item: %w", err)
		}
		items = append(items, wi)
	}
	if items == nil {
		items = []model.WorkItem{}
	}
	return items, nil
}

// GetWorkItem retrieves a single work item.
func (s *RunService) GetWorkItem(ctx context.Context, workItemID uuid.UUID) (*model.WorkItem, error) {
	var wi model.WorkItem
	err := s.pool.QueryRow(ctx,
		`SELECT id, run_id, key, title, objective, work_type, status, blocked_reason, input_data,
		        output_data, current_attempt_id, created_by_planner_round_id, created_at, updated_at
		 FROM work_items WHERE id = $1`, workItemID).Scan(
		&wi.ID, &wi.RunID, &wi.Key, &wi.Title, &wi.Objective, &wi.WorkType, &wi.Status,
		&wi.BlockedReason, &wi.InputData, &wi.OutputData, &wi.CurrentAttemptID, &wi.CreatedByPlannerRoundID, &wi.CreatedAt, &wi.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get work_item: %w", err)
	}
	return &wi, nil
}

// ListWorkDependenciesByRun retrieves all dependency relationships for a run.
func (s *RunService) ListWorkDependenciesByRun(ctx context.Context, runID uuid.UUID) ([]model.WorkDependency, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, work_item_id, depends_on_work_item_id, created_at
		 FROM work_dependencies WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list work_dependencies: %w", err)
	}
	defer rows.Close()

	var deps []model.WorkDependency
	for rows.Next() {
		var d model.WorkDependency
		if err := rows.Scan(&d.ID, &d.RunID, &d.WorkItemID, &d.DependsOnWorkItemID, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan work_dependency: %w", err)
		}
		deps = append(deps, d)
	}
	if deps == nil {
		deps = []model.WorkDependency{}
	}
	return deps, nil
}

// ListWorkDependenciesByWorkItem retrieves dependencies for a specific work item.
func (s *RunService) ListWorkDependenciesByWorkItem(ctx context.Context, workItemID uuid.UUID) ([]model.WorkDependency, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, work_item_id, depends_on_work_item_id, created_at
		 FROM work_dependencies WHERE work_item_id = $1 ORDER BY created_at ASC`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("list work_dependencies by item: %w", err)
	}
	defer rows.Close()

	var deps []model.WorkDependency
	for rows.Next() {
		var d model.WorkDependency
		if err := rows.Scan(&d.ID, &d.RunID, &d.WorkItemID, &d.DependsOnWorkItemID, &d.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan work_dependency: %w", err)
		}
		deps = append(deps, d)
	}
	if deps == nil {
		deps = []model.WorkDependency{}
	}
	return deps, nil
}

// ClaimNextPendingWorkItem atomically selects and claims the next available WorkItem.
// Returns nil, nil, nil, nil if no work item is currently ready.
func (s *RunService) ClaimNextPendingWorkItem(ctx context.Context, runID uuid.UUID) (*model.WorkItem, *model.Attempt, *model.AgentSession, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// Verify run is still running
	var runStatus model.RunStatus
	err = tx.QueryRow(ctx, `SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("check run status: %w", err)
	}
	if runStatus != model.RunStatusRunning {
		return nil, nil, nil, nil
	}

	// 1. Mark blocked any pending items whose prerequisites failed/cancelled/blocked
	_, err = tx.Exec(ctx,
		`UPDATE work_items wi
		 SET status = 'blocked', blocked_reason = 'prerequisite work item failed or blocked', updated_at = now()
		 WHERE wi.run_id = $1 AND wi.status = 'pending'
		   AND EXISTS (
		     SELECT 1 FROM work_dependencies wd
		     JOIN work_items dep ON wd.depends_on_work_item_id = dep.id
		     WHERE wd.work_item_id = wi.id AND dep.status IN ('failed', 'exhausted', 'cancelled', 'blocked')
		   )`, runID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("update blocked items: %w", err)
	}

	// 2. Select a pending item whose prerequisites are all completed
	var wi model.WorkItem
	err = tx.QueryRow(ctx,
		`SELECT wi.id, wi.run_id, wi.key, wi.title, wi.objective, wi.work_type, wi.status, wi.blocked_reason, wi.input_data,
		        wi.output_data, wi.current_attempt_id, wi.created_at, wi.updated_at
		 FROM work_items wi
		 WHERE wi.run_id = $1 AND wi.status = 'pending'
		   AND NOT EXISTS (
		     SELECT 1 FROM work_dependencies wd
		     JOIN work_items dep ON wd.depends_on_work_item_id = dep.id
		     WHERE wd.work_item_id = wi.id AND dep.status != 'completed'
		   )
		 ORDER BY wi.created_at ASC
		 LIMIT 1
		 FOR UPDATE OF wi SKIP LOCKED`, runID).Scan(
		&wi.ID, &wi.RunID, &wi.Key, &wi.Title, &wi.Objective, &wi.WorkType, &wi.Status,
		&wi.BlockedReason, &wi.InputData, &wi.OutputData, &wi.CurrentAttemptID, &wi.CreatedAt, &wi.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			// Commit blocked items update if any
			if commitErr := tx.Commit(ctx); commitErr != nil {
				return nil, nil, nil, fmt.Errorf("commit blocked items: %w", commitErr)
			}
			return nil, nil, nil, nil
		}
		return nil, nil, nil, fmt.Errorf("claim work_item: %w", err)
	}

	// 3. Create Session for worker
	sessID := uuid.New()
	var sess model.AgentSession
	err = tx.QueryRow(ctx,
		`INSERT INTO agent_sessions (id, run_id, role, work_item_id, status, input_snapshot)
		 VALUES ($1, $2, 'worker', $3, 'running', $4)
		 RETURNING id, run_id, role, round, work_item_id, attempt_id, status, input_snapshot, finish_reason, created_at, updated_at`,
		sessID, runID, wi.ID, wi.InputData).Scan(
		&sess.ID, &sess.RunID, &sess.Role, &sess.Round, &sess.WorkItemID, &sess.AttemptID,
		&sess.Status, &sess.InputSnapshot, &sess.FinishReason, &sess.CreatedAt, &sess.UpdatedAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create worker session: %w", err)
	}

	// 4. Create Attempt
	var nextAttemptNum int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(attempt_number), 0) + 1 FROM attempts WHERE work_item_id = $1`, wi.ID).Scan(&nextAttemptNum)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get next attempt number: %w", err)
	}

	var att model.Attempt
	err = tx.QueryRow(ctx,
		`INSERT INTO attempts (id, work_item_id, attempt_number, session_id, status, started_at)
		 VALUES (gen_random_uuid(), $1, $2, $3, 'running', now())
		 RETURNING id, work_item_id, attempt_number, session_id, status, started_at, finished_at, finish_reason, created_at`,
		wi.ID, nextAttemptNum, sess.ID).Scan(
		&att.ID, &att.WorkItemID, &att.AttemptNumber, &att.SessionID, &att.Status, &att.StartedAt,
		&att.FinishedAt, &att.FinishReason, &att.CreatedAt)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("create attempt: %w", err)
	}

	// Link attempt to session
	_, err = tx.Exec(ctx, `UPDATE agent_sessions SET attempt_id = $1 WHERE id = $2`, att.ID, sess.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("link attempt to session: %w", err)
	}

	// Update work_item status to running
	_, err = tx.Exec(ctx,
		`UPDATE work_items SET status = 'running', current_attempt_id = $1, updated_at = now() WHERE id = $2`,
		att.ID, wi.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("update work_item to running: %w", err)
	}
	wi.Status = model.WorkItemStatusRunning
	wi.CurrentAttemptID = &att.ID

	// 5. Emit RunEvent
	var nextSeq int
	err = tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("get next run_event seq: %w", err)
	}
	claimPayload, _ := json.Marshal(map[string]interface{}{
		"work_item_id": wi.ID,
		"key":          wi.Key,
		"title":        wi.Title,
		"attempt_id":   att.ID,
		"session_id":   sess.ID,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload, session_id, work_item_id, attempt_id)
		 VALUES (gen_random_uuid(), $1, $2, 'work_item_claimed', $3, $4, $5, $6)`,
		runID, nextSeq, claimPayload, sess.ID, wi.ID, att.ID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("insert claim event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, nil, nil, fmt.Errorf("commit claim: %w", err)
	}

	return &wi, &att, &sess, nil
}

// FinishWorkItem updates a work item and its attempt to completed or failed.
func (s *RunService) FinishWorkItem(ctx context.Context, workItemID, attemptID, sessionID uuid.UUID, success bool, outputData json.RawMessage, finishReason string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	// Serialize final worker results with stop acknowledgement. A cancelled attempt
	// cannot be revived by a model/tool response that arrives after its deadline.
	var runStatus model.RunStatus
	var itemStatus model.WorkItemStatus
	var runReason *string
	err = tx.QueryRow(ctx, `SELECT status, finish_reason FROM runs WHERE id=(
		SELECT run_id FROM work_items WHERE id=$1 AND current_attempt_id=$2) FOR UPDATE`, workItemID, attemptID).Scan(&runStatus, &runReason)
	if err != nil {
		return fmt.Errorf("lock work item completion: %w", err)
	}
	if runStatus.IsTerminal() {
		reason := "parent_run_terminal"
		if runReason != nil && *runReason != "" {
			reason = *runReason
		}
		childStatus := "stopped"
		workStatus := "cancelled"
		if runReason != nil && (*runReason == string(model.CauseModelBudgetExhausted) || *runReason == string(model.CauseToolBudgetExhausted)) {
			workStatus = "exhausted"
		}
		if runStatus == model.RunStatusFailed {
			childStatus = "failed"
		}
		if runStatus == model.RunStatusInterrupted {
			childStatus = "interrupted"
			workStatus = "failed"
		}
		if err := closeAttemptAndSession(ctx, tx, attemptID, sessionID, childStatus, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE work_items SET status=$2,updated_at=now() WHERE id=$1 AND status IN ('pending','running','blocked')`, workItemID, workStatus); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	// Read after acquiring the Run lock, using the same lock order as cancellation.
	if err = tx.QueryRow(ctx, `SELECT status FROM work_items WHERE id=$1 AND current_attempt_id=$2 FOR UPDATE`, workItemID, attemptID).Scan(&itemStatus); err != nil {
		return err
	}
	if itemStatus != model.WorkItemStatusRunning {
		if err := closeAttemptAndSession(ctx, tx, attemptID, sessionID, "stopped", "work_item_already_finished"); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}

	targetItemStatus := model.WorkItemStatusCompleted
	targetAttemptStatus := "completed"
	if !success {
		targetItemStatus = model.WorkItemStatusFailed
		targetAttemptStatus = "failed"
	}
	if finishReason == string(model.CauseModelBudgetExhausted) || finishReason == string(model.CauseToolBudgetExhausted) || finishReason == "max_model_calls_exceeded" || finishReason == "max_tool_calls_exceeded" {
		targetItemStatus = model.WorkItemStatusExhausted
		targetAttemptStatus = "exhausted"
	}
	if finishReason == string(model.CauseShutdown) {
		targetAttemptStatus = "interrupted"
	}
	if runStatus == model.RunStatusStopping || finishReason == "user_requested_stop" || finishReason == "timeout" {
		targetItemStatus = model.WorkItemStatusCancelled
		targetAttemptStatus = "stopped"
		if runStatus == model.RunStatusStopping {
			finishReason = "user_requested_stop"
		}
	}

	if outputData == nil {
		outputData = json.RawMessage(`{}`)
	}

	// 1. Update attempt
	_, err = tx.Exec(ctx,
		`UPDATE attempts SET status = $1, finish_reason = $2, finished_at = now() WHERE id = $3 AND status='running'`,
		targetAttemptStatus, finishReason, attemptID)
	if err != nil {
		return fmt.Errorf("update attempt: %w", err)
	}

	// 2. Update session
	_, err = tx.Exec(ctx,
		`UPDATE agent_sessions SET status = $1, finish_reason = $2, updated_at = now() WHERE id = $3 AND status = 'running'`,
		targetAttemptStatus, finishReason, sessionID)
	if err != nil {
		return fmt.Errorf("update session: %w", err)
	}

	// 3. Update work item
	_, err = tx.Exec(ctx,
		`UPDATE work_items SET status = $1, output_data = $2, updated_at = now() WHERE id = $3`,
		targetItemStatus, outputData, workItemID)
	if err != nil {
		return fmt.Errorf("update work_item: %w", err)
	}

	return tx.Commit(ctx)
}

// --- Attempts ---

// GetAttempt retrieves an attempt by ID.
func (s *RunService) GetAttempt(ctx context.Context, attemptID uuid.UUID) (*model.Attempt, error) {
	var att model.Attempt
	err := s.pool.QueryRow(ctx,
		`SELECT id, work_item_id, attempt_number, session_id, status, started_at, finished_at, finish_reason, created_at
		 FROM attempts WHERE id = $1`, attemptID).Scan(
		&att.ID, &att.WorkItemID, &att.AttemptNumber, &att.SessionID, &att.Status, &att.StartedAt,
		&att.FinishedAt, &att.FinishReason, &att.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get attempt: %w", err)
	}
	return &att, nil
}

// ListAttemptsByWorkItem retrieves all attempts for a work item.
func (s *RunService) ListAttemptsByWorkItem(ctx context.Context, workItemID uuid.UUID) ([]model.Attempt, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, work_item_id, attempt_number, session_id, status, started_at, finished_at, finish_reason, created_at
		 FROM attempts WHERE work_item_id = $1 ORDER BY attempt_number ASC`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("list attempts: %w", err)
	}
	defer rows.Close()

	var atts []model.Attempt
	for rows.Next() {
		var att model.Attempt
		if err := rows.Scan(&att.ID, &att.WorkItemID, &att.AttemptNumber, &att.SessionID, &att.Status,
			&att.StartedAt, &att.FinishedAt, &att.FinishReason, &att.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan attempt: %w", err)
		}
		atts = append(atts, att)
	}
	if atts == nil {
		atts = []model.Attempt{}
	}
	return atts, nil
}

// --- Observations ---

// CreateObservation records a structured observation linked to an attempt and a verified source tool call.
func (s *RunService) CreateObservation(ctx context.Context, runID, attemptID uuid.UUID, obsType, content string, data json.RawMessage, sourceToolCallID uuid.UUID) (*model.Observation, error) {
	return s.CreateObservationWithAsset(ctx, runID, attemptID, obsType, content, data, sourceToolCallID, nil)
}

// CreateObservationWithAsset records an observation with strict verification of attempt, session, tool call, sample ID, and asset scope.
func (s *RunService) CreateObservationWithAsset(ctx context.Context, runID, attemptID uuid.UUID, obsType, content string, data json.RawMessage, sourceToolCallID uuid.UUID, assetID *uuid.UUID) (*model.Observation, error) {
	if data == nil {
		data = json.RawMessage(`{}`)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	// 1. Verify run is active with ROW LOCK (FOR UPDATE) to serialize deterministically with Stop/terminal transitions
	var runStatus model.RunStatus
	var scopePolicyVersion string
	err = tx.QueryRow(ctx, `SELECT status, scope_policy_version FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&runStatus, &scopePolicyVersion)
	if err != nil {
		return nil, fmt.Errorf("verify run: %w", err)
	}
	if runStatus.IsTerminal() || runStatus == model.RunStatusStopping {
		return nil, fmt.Errorf("cannot record observation: run status is %s", runStatus)
	}

	source, err := validateObservationSourceTx(ctx, tx, runID, uuid.Nil, attemptID, sourceToolCallID, assetID, scopePolicyVersion)
	if err != nil {
		return nil, err
	}
	finalAssetID, attSessionID, workItemID := source.assetID, source.sessionID, source.workItem

	// 7. Insert observation
	var obs model.Observation
	err = tx.QueryRow(ctx,
		`INSERT INTO observations (id, run_id, attempt_id, asset_id, obs_type, content, data, source_tool_call_id)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, run_id, attempt_id, asset_id, obs_type, content, data, source_tool_call_id, created_at`,
		runID, attemptID, finalAssetID, obsType, content, data, sourceToolCallID).Scan(
		&obs.ID, &obs.RunID, &obs.AttemptID, &obs.AssetID, &obs.ObsType, &obs.Content, &obs.Data, &obs.SourceToolCallID, &obs.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert observation: %w", err)
	}

	// 8. Insert observation event into run_events
	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, fmt.Errorf("get next run_events seq: %w", err)
	}
	eventPayload, _ := json.Marshal(map[string]interface{}{
		"observation_id": obs.ID,
		"obs_type":       obsType,
		"content":        content,
		"asset_id":       finalAssetID,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload, session_id, work_item_id, attempt_id)
		 VALUES (gen_random_uuid(), $1, $2, 'observation', $3, $4, $5, $6)`,
		runID, nextSeq, eventPayload, attSessionID, workItemID, attemptID)
	if err != nil {
		return nil, fmt.Errorf("insert run_events observation: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit observation: %w", err)
	}
	return &obs, nil
}

// ListObservationsByRun retrieves all observations for a run.
func (s *RunService) ListObservationsByRun(ctx context.Context, runID uuid.UUID) ([]model.Observation, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, attempt_id, asset_id, obs_type, content, data, source_tool_call_id, created_at
		 FROM observations WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("list observations: %w", err)
	}
	defer rows.Close()

	var obs []model.Observation
	for rows.Next() {
		var o model.Observation
		if err := rows.Scan(&o.ID, &o.RunID, &o.AttemptID, &o.AssetID, &o.ObsType, &o.Content, &o.Data, &o.SourceToolCallID, &o.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan observation: %w", err)
		}
		obs = append(obs, o)
	}
	if obs == nil {
		obs = []model.Observation{}
	}
	return obs, nil
}

// CompleteRunWithOutput atomically completes a run with its final assistant answer.
// If run is in 'stopping' status, it avoids overwriting with 'completed' and safely converges to 'stopped'.
func (s *RunService) CompleteRunWithOutput(ctx context.Context, runID uuid.UUID, finishReason string, finalOutput string) (*model.Run, error) {
	if finishReason == "" {
		finishReason = "final_answer"
	}
	if err := s.finalizeRunWrapup(ctx, runID, model.TerminationCause(finishReason), &finalOutput, true); err != nil {
		return nil, err
	}
	return s.Get(ctx, runID)
}

func lockRunAdmission(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	var status model.RunStatus
	if err := tx.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&status); err != nil {
		return err
	}
	if status.IsTerminal() || status == model.RunStatusStopping {
		return ErrTerminalState
	}
	return nil
}

func closeAttemptAndSession(ctx context.Context, tx pgx.Tx, attemptID, sessionID uuid.UUID, status, reason string) error {
	if _, err := tx.Exec(ctx, `UPDATE attempts SET status=$1,finish_reason=$2,finished_at=now() WHERE id=$3 AND status='running'`, status, reason, attemptID); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE agent_sessions SET status=$1,finish_reason=$2,updated_at=now() WHERE id=$3 AND status='running'`, status, reason, sessionID)
	return err
}
