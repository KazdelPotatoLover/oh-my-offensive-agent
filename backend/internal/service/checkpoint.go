package service

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-workbench/backend/internal/provider"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func (s *RunService) SaveContextCheckpoint(ctx context.Context, runID, sessionID uuid.UUID, view []provider.ChatMessage, summary string) error {
	var workID *uuid.UUID
	var epoch, current int
	if err := s.pool.QueryRow(ctx, `SELECT s.work_item_id,s.epoch_number,r.current_epoch FROM agent_sessions s JOIN runs r ON r.id=s.run_id WHERE s.id=$1 AND s.run_id=$2`, sessionID, runID).Scan(&workID, &epoch, &current); err != nil {
		return err
	}
	if epoch != current {
		return ErrStaleEpoch
	}
	var raw []provider.ChatMessage
	var err error
	if workID != nil {
		raw, err = s.LoadWorkItemConversation(ctx, *workID)
	} else {
		raw, err = s.LoadSessionConversation(ctx, sessionID)
	}
	if err != nil {
		return err
	}
	encoded, err := provider.EncodeCheckpoint(view)
	if err != nil {
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	// Serialize publication with epoch transitions, after the independent reads
	// so a checkpoint does not hold a Run lock while borrowing another connection.
	if err := lockSessionAdmission(ctx, tx, runID, &sessionID, true); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `INSERT INTO context_checkpoints(run_id,work_item_id,session_id,covered_messages,source_sha256,strategy,summary,view_messages)
		VALUES($1,$2,$3,$4,$5,'extractive_protocol_blocks_v1',$6,$7) ON CONFLICT(session_id,covered_messages,source_sha256) DO NOTHING`, runID, workID, sessionID, len(raw), provider.ConversationHash(raw), summary, json.RawMessage(encoded))
	if err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *RunService) LoadWorkItemContext(ctx context.Context, workID uuid.UUID) ([]provider.ChatMessage, error) {
	raw, err := s.LoadWorkItemConversation(ctx, workID)
	if err != nil {
		return nil, err
	}
	var count int
	var hash string
	var viewBytes []byte
	err = s.pool.QueryRow(ctx, `SELECT covered_messages,source_sha256,view_messages FROM context_checkpoints WHERE work_item_id=$1 ORDER BY created_at DESC,id DESC LIMIT 1`, workID).Scan(&count, &hash, &viewBytes)
	if err == pgx.ErrNoRows {
		return raw, nil
	}
	if err != nil {
		return nil, err
	}
	if count > len(raw) || count < 0 || provider.ConversationHash(raw[:count]) != hash {
		return nil, fmt.Errorf("checkpoint source mismatch: original transcript remains intact")
	}
	view, err := provider.DecodeCheckpoint(viewBytes)
	if err != nil {
		return nil, err
	}
	return append(view, raw[count:]...), nil
}

func (s *RunService) ListContextCheckpoints(ctx context.Context, runID uuid.UUID) ([]map[string]any, error) {
	rows, err := s.pool.Query(ctx, `SELECT jsonb_build_object('id',id,'work_item_id',work_item_id,'session_id',session_id,'version',version,'covered_messages',covered_messages,'source_sha256',source_sha256,'strategy',strategy,'summary',summary,'created_at',created_at) FROM context_checkpoints WHERE run_id=$1 ORDER BY created_at DESC LIMIT 100`, runID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []map[string]any{}
	for rows.Next() {
		var raw []byte
		var row map[string]any
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(raw, &row); err != nil {
			return nil, err
		}
		out = append(out, row)
	}

	return out, rows.Err()
}
