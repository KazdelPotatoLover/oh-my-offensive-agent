package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// InsertAgentMessage inserts a new message in the conversation history for a run.
func (s *RunService) InsertAgentMessage(ctx context.Context, runID uuid.UUID, role string, content string, toolCallID *string, toolCalls json.RawMessage) (*model.AgentMessage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_messages WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, fmt.Errorf("get next seq for agent_messages: %w", err)
	}

	var msg model.AgentMessage
	err = tx.QueryRow(ctx,
		`INSERT INTO agent_messages (id, run_id, seq, role, content, tool_call_id, tool_calls)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)
		 RETURNING id, run_id, seq, role, content, tool_call_id, tool_calls, created_at`,
		runID, nextSeq, role, content, toolCallID, toolCalls).Scan(
		&msg.ID, &msg.RunID, &msg.Seq, &msg.Role, &msg.Content, &msg.ToolCallID, &msg.ToolCalls, &msg.CreatedAt)
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
		`SELECT id, run_id, seq, role, content, tool_call_id, tool_calls, created_at
		 FROM agent_messages WHERE run_id = $1 ORDER BY seq ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query agent_messages: %w", err)
	}
	defer rows.Close()

	var msgs []model.AgentMessage
	for rows.Next() {
		var m model.AgentMessage
		if err := rows.Scan(&m.ID, &m.RunID, &m.Seq, &m.Role, &m.Content, &m.ToolCallID, &m.ToolCalls, &m.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan agent_message: %w", err)
		}
		msgs = append(msgs, m)
	}
	if msgs == nil {
		msgs = []model.AgentMessage{}
	}
	return msgs, nil
}

// CreateModelCall records the start of a model invocation.
func (s *RunService) CreateModelCall(ctx context.Context, runID uuid.UUID, round int) (*model.ModelCall, error) {
	var call model.ModelCall
	err := s.pool.QueryRow(ctx,
		`INSERT INTO model_calls (id, run_id, round, status, started_at)
		 VALUES (gen_random_uuid(), $1, $2, 'started', now())
		 RETURNING id, run_id, round, status, finish_reason, request_id, prompt_tokens, completion_tokens,
		           total_tokens, error_message, started_at, finished_at, created_at`,
		runID, round).Scan(
		&call.ID, &call.RunID, &call.Round, &call.Status, &call.FinishReason, &call.RequestID,
		&call.PromptTokens, &call.CompletionTokens, &call.TotalTokens, &call.ErrorMessage,
		&call.StartedAt, &call.FinishedAt, &call.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert model_call: %w", err)
	}
	return &call, nil
}

// UpdateModelCall updates a model call record upon completion or failure.
func (s *RunService) UpdateModelCall(ctx context.Context, id uuid.UUID, status string, finishReason *string, requestID *string, promptTokens, completionTokens, totalTokens int, errMsg *string) error {
	_, err := s.pool.Exec(ctx,
		`UPDATE model_calls
		 SET status = $1, finish_reason = $2, request_id = $3, prompt_tokens = $4,
		     completion_tokens = $5, total_tokens = $6, error_message = $7, finished_at = now()
		 WHERE id = $8`,
		status, finishReason, requestID, promptTokens, completionTokens, totalTokens, errMsg, id)
	if err != nil {
		return fmt.Errorf("update model_call: %w", err)
	}
	return nil
}

// ListModelCalls retrieves all model calls for a run.
func (s *RunService) ListModelCalls(ctx context.Context, runID uuid.UUID) ([]model.ModelCall, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, round, status, finish_reason, request_id, prompt_tokens, completion_tokens,
		        total_tokens, error_message, started_at, finished_at, created_at
		 FROM model_calls WHERE run_id = $1 ORDER BY round ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query model_calls: %w", err)
	}
	defer rows.Close()

	var calls []model.ModelCall
	for rows.Next() {
		var c model.ModelCall
		if err := rows.Scan(&c.ID, &c.RunID, &c.Round, &c.Status, &c.FinishReason, &c.RequestID,
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

// CreateAgentToolCall records a tool request from the model.
func (s *RunService) CreateAgentToolCall(ctx context.Context, runID uuid.UUID, modelCallID *uuid.UUID, callID string, toolName string, arguments json.RawMessage) (*model.AgentToolCall, error) {
	var tc model.AgentToolCall
	err := s.pool.QueryRow(ctx,
		`INSERT INTO agent_tool_calls (id, run_id, model_call_id, call_id, tool_name, arguments, status)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, 'pending')
		 RETURNING id, run_id, model_call_id, call_id, tool_name, arguments, output, status,
		           error_message, started_at, finished_at, created_at`,
		runID, modelCallID, callID, toolName, arguments).Scan(
		&tc.ID, &tc.RunID, &tc.ModelCallID, &tc.CallID, &tc.ToolName, &tc.Arguments, &tc.Output,
		&tc.Status, &tc.ErrorMessage, &tc.StartedAt, &tc.FinishedAt, &tc.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert agent_tool_call: %w", err)
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
		 WHERE id = $6`,
		status, output, errMsg, startedAt, finishedAt, id)
	if err != nil {
		return fmt.Errorf("update agent_tool_call: %w", err)
	}
	return nil
}

// ListAgentToolCalls retrieves all tool calls for a run.
func (s *RunService) ListAgentToolCalls(ctx context.Context, runID uuid.UUID) ([]model.AgentToolCall, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, model_call_id, call_id, tool_name, arguments, output, status,
		        error_message, started_at, finished_at, created_at
		 FROM agent_tool_calls WHERE run_id = $1 ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, fmt.Errorf("query agent_tool_calls: %w", err)
	}
	defer rows.Close()

	var calls []model.AgentToolCall
	for rows.Next() {
		var c model.AgentToolCall
		if err := rows.Scan(&c.ID, &c.RunID, &c.ModelCallID, &c.CallID, &c.ToolName, &c.Arguments,
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

// CompleteRunWithOutput atomically completes a run with its final assistant answer.
// If run is in 'stopping' status, it avoids overwriting with 'completed' and safely converges to 'stopped'.
func (s *RunService) CompleteRunWithOutput(ctx context.Context, runID uuid.UUID, finishReason string, finalOutput string) (*model.Run, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var currentStatus model.RunStatus
	err = tx.QueryRow(ctx,
		`SELECT status FROM runs WHERE id = $1 FOR UPDATE`, runID).Scan(&currentStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get run for update: %w", err)
	}

	if currentStatus.IsTerminal() {
		return nil, fmt.Errorf("%w: run is already in terminal status %s", ErrTerminalState, currentStatus)
	}

	now := time.Now()
	var r model.Run

	// Crucial invariant: if user requested stop and run is currently stopping, converge to stopped!
	if currentStatus == model.RunStatusStopping {
		stopReason := "user_requested_stop"
		err = tx.QueryRow(ctx,
			`UPDATE runs SET status = $1, finish_reason = $2, finished_at = $3, updated_at = $3 
			 WHERE id = $4 
			 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
			          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
			model.RunStatusStopped, stopReason, now, runID).Scan(
			&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
			&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
			&r.CreatedAt, &r.UpdatedAt)
		if err != nil {
			return nil, fmt.Errorf("update run to stopped: %w", err)
		}

		var nextSeq int
		err = tx.QueryRow(ctx,
			`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
		if err != nil {
			return nil, fmt.Errorf("get next seq: %w", err)
		}

		payload, _ := json.Marshal(map[string]interface{}{
			"from": string(model.RunStatusStopping),
			"to":   string(model.RunStatusStopped),
		})
		_, err = tx.Exec(ctx,
			`INSERT INTO run_events (id, run_id, seq, type, payload) 
			 VALUES (gen_random_uuid(), $1, $2, 'state_change', $3)`,
			runID, nextSeq, payload)
		if err != nil {
			return nil, fmt.Errorf("insert state_change event: %w", err)
		}

		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit stopped transition: %w", err)
		}
		return &r, nil
	}

	// Normal completion with final answer
	err = tx.QueryRow(ctx,
		`UPDATE runs SET status = $1, finish_reason = $2, final_output = $3, finished_at = $4, updated_at = $4 
		 WHERE id = $5 
		 RETURNING id, task_id, idempotency_key, config_snapshot, execution_input, status, finish_reason, 
		          fail_at_step, final_output, started_at, finished_at, created_at, updated_at`,
		model.RunStatusCompleted, finishReason, finalOutput, now, runID).Scan(
		&r.ID, &r.TaskID, &r.IdempotencyKey, &r.ConfigSnapshot, &r.ExecutionInput,
		&r.Status, &r.FinishReason, &r.FailAtStep, &r.FinalOutput, &r.StartedAt, &r.FinishedAt,
		&r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return nil, fmt.Errorf("update run to completed: %w", err)
	}

	var nextSeq int
	err = tx.QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, runID).Scan(&nextSeq)
	if err != nil {
		return nil, fmt.Errorf("get next seq: %w", err)
	}

	// 1. state_change event
	payload, _ := json.Marshal(map[string]interface{}{
		"from": string(currentStatus),
		"to":   string(model.RunStatusCompleted),
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, $2, 'state_change', $3)`,
		runID, nextSeq, payload)
	if err != nil {
		return nil, fmt.Errorf("insert state_change event: %w", err)
	}

	// 2. final_answer event
	answerPayload, _ := json.Marshal(map[string]interface{}{
		"output": finalOutput,
	})
	_, err = tx.Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload) 
		 VALUES (gen_random_uuid(), $1, $2, 'final_answer', $3)`,
		runID, nextSeq+1, answerPayload)
	if err != nil {
		return nil, fmt.Errorf("insert final_answer event: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit completed transition: %w", err)
	}

	return &r, nil
}
