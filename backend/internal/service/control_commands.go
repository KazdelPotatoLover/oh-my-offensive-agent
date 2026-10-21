package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"agent-workbench/backend/internal/provider"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type EpochStartOptions struct {
	ExpectedEpoch  int
	IdempotencyKey string
}

func epochCommandHash(reason, summary string, expected int) string {
	b, _ := json.Marshal([]any{reason, summary, expected})
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
func (s *RunService) LookupEpochCommand(ctx context.Context, runID uuid.UUID, reason, summary string, opt EpochStartOptions) (int, bool, error) {
	if opt.IdempotencyKey == "" {
		return 0, false, nil
	}
	var hash string
	var result struct {
		Epoch int `json:"epoch_number"`
	}
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT request_hash,result FROM control_commands WHERE run_id=$1 AND kind='epoch_start' AND idempotency_key=$2`, runID, opt.IdempotencyKey).Scan(&hash, &raw)
	if err == pgx.ErrNoRows {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if hash != epochCommandHash(reason, summary, opt.ExpectedEpoch) {
		return 0, false, ErrConflict
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		return 0, false, err
	}
	return result.Epoch, true, nil
}

// ConsumeSteerMessages commits both the delivery acknowledgement and the raw
// conversation message. A lost HTTP response cannot cause a second injection.
func (s *RunService) ConsumeSteerMessages(ctx context.Context, runID, workID, sessionID uuid.UUID) ([]provider.ChatMessage, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var epoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&epoch); err != nil {
		return nil, err
	}
	var valid bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM agent_sessions WHERE id=$1 AND run_id=$2 AND work_item_id=$3 AND epoch_number=$4 AND status='running')`, sessionID, runID, workID, epoch).Scan(&valid); err != nil {
		return nil, err
	}
	if !valid {
		return nil, ErrStaleEpoch
	}
	rows, err := tx.Query(ctx, `SELECT id,message FROM work_control_messages WHERE run_id=$1 AND work_item_id=$2 AND delivered_at IS NULL AND epoch_number<=$3 ORDER BY created_at,id FOR UPDATE`, runID, workID, epoch)
	if err != nil {
		return nil, err
	}
	ids := []uuid.UUID{}
	messages := []provider.ChatMessage{}
	for rows.Next() {
		var id uuid.UUID
		var text string
		if err := rows.Scan(&id, &text); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
		messages = append(messages, provider.ChatMessage{Role: provider.RoleUser, Content: fmt.Sprintf("[Operator Steer Guidance]: [%s] %s", id, text)})
	}
	rows.Close()
	for _, m := range messages {
		if _, err = tx.Exec(ctx, `INSERT INTO agent_messages(run_id,session_id,seq,role,content) VALUES($1,$2,(SELECT COALESCE(MAX(seq),0)+1 FROM agent_messages WHERE session_id=$2),'user',$3)`, runID, sessionID, m.Content); err != nil {
			return nil, err
		}
	}
	if len(ids) > 0 {
		if _, err = tx.Exec(ctx, `UPDATE work_control_messages SET delivered_at=now(),delivered_session_id=$1 WHERE id=ANY($2)`, sessionID, ids); err != nil {
			return nil, err
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return messages, nil
}
