package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ClosedToolRecord is a tool call that the session closed without executing.
type ClosedToolRecord struct {
	CallID      string
	Name        string
	Arguments   json.RawMessage
	Result      string
	ModelCallID uuid.UUID
}

// AcknowledgeUndeliveredSteers marks pending steer rows delivered without
// inserting another agent message. Use PersistClosedToolBatch when the
// guidance message is new and must commit with the closed tool results.
func (s *RunService) AcknowledgeUndeliveredSteers(ctx context.Context, runID, workID, sessionID uuid.UUID) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("run service is not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := acknowledgeUndeliveredSteers(ctx, tx, runID, workID, sessionID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// PersistClosedToolBatch writes unexecuted tool results, the single guidance
// message, the not-executed tool records, and steer delivery in one transaction.
func (s *RunService) PersistClosedToolBatch(ctx context.Context, runID, workID, sessionID uuid.UUID, messages []provider.ChatMessage, calls []ClosedToolRecord) error {
	if s == nil || s.pool == nil {
		return fmt.Errorf("run service is not configured")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := lockSessionAdmission(ctx, tx, runID, &sessionID); err != nil {
		return err
	}
	for _, msg := range messages {
		var toolCallID *string
		if msg.ToolCallID != "" {
			id := msg.ToolCallID
			toolCallID = &id
		}
		var nextSeq int
		if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) + 1 FROM agent_messages WHERE session_id = $1`, sessionID).Scan(&nextSeq); err != nil {
			return fmt.Errorf("closed batch seq: %w", err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_messages (id, run_id, session_id, seq, role, content, tool_call_id)
			VALUES (gen_random_uuid(), $1, $2, $3, $4, $5, $6)`,
			runID, sessionID, nextSeq, string(msg.Role), msg.Content, toolCallID); err != nil {
			return fmt.Errorf("closed batch message: %w", err)
		}
	}
	now := time.Now()
	notExecuted := "not executed"
	for _, call := range calls {
		args := call.Arguments
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		var modelCallID *uuid.UUID
		if call.ModelCallID != uuid.Nil {
			id := call.ModelCallID
			modelCallID = &id
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO agent_tool_calls (
				id, run_id, session_id, model_call_id, call_id, tool_name, arguments,
				output, status, error_message, started_at, finished_at, epoch_number
			) VALUES (
				gen_random_uuid(), $1, $2, $3, $4, $5, $6,
				$7, 'rejected', $8, $9, $9, COALESCE((SELECT current_epoch FROM runs WHERE id = $1), 1)
			)`,
			runID, sessionID, modelCallID, call.CallID, call.Name, args, call.Result, notExecuted, now); err != nil {
			return fmt.Errorf("closed batch tool record: %w", err)
		}
	}
	if workID != uuid.Nil {
		if err := acknowledgeUndeliveredSteers(ctx, tx, runID, workID, sessionID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func acknowledgeUndeliveredSteers(ctx context.Context, tx pgx.Tx, runID, workID, sessionID uuid.UUID) error {
	if _, err := tx.Exec(ctx, `
		UPDATE work_control_messages
		SET delivered_at = now(), delivered_session_id = $1
		WHERE run_id = $2 AND work_item_id = $3 AND delivered_at IS NULL`,
		sessionID, runID, workID); err != nil {
		return fmt.Errorf("acknowledge steer: %w", err)
	}
	return nil
}
