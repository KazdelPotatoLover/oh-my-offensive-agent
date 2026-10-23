package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// invalidateProofs retains the original relation and its versions for audit.
func invalidateProofs(ctx context.Context, tx pgx.Tx, runID, nodeID uuid.UUID, reason string) error {
	_, err := tx.Exec(ctx, `UPDATE exploration_edges SET valid=false,invalid_reason=$3,invalidated_at=now() WHERE run_id=$1 AND rel='proves' AND (src_id=$2 OR dst_id=$2) AND valid`, runID, nodeID, reason)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE exploration_nodes g SET state='open',completed_at=NULL,updated_at=now()
		WHERE g.run_id=$1 AND g.kind='goal' AND g.state='met' AND NOT EXISTS(
		SELECT 1 FROM exploration_edges e JOIN exploration_nodes n ON n.id=e.src_id WHERE e.dst_id=g.id AND e.rel='proves' AND e.valid AND n.state='verified' AND n.delete_reason IS NULL AND e.source_version=n.content_version AND e.target_version=g.content_version)`, runID)
	return err
}

func invalidateFindingProofs(ctx context.Context, tx pgx.Tx, runID, findingID uuid.UUID, reason string) error {
	rows, err := tx.Query(ctx, `SELECT id FROM exploration_nodes WHERE run_id=$1 AND finding_id=$2`, runID, findingID)
	if err != nil {
		return err
	}
	ids := []uuid.UUID{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if _, err := tx.Exec(ctx, `INSERT INTO exploration_revisions(run_id,node_id,version,payload,state,reason,actor) SELECT run_id,id,content_version,payload,state,$3,'finding_lifecycle' FROM exploration_nodes WHERE id=$2 AND run_id=$1 ON CONFLICT(node_id,version) DO NOTHING`, runID, id, reason); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE exploration_nodes SET state='invalidated',content_version=content_version+1,updated_at=now() WHERE id=$1`, id); err != nil {
			return err
		}
		if err := invalidateProofs(ctx, tx, runID, id, reason); err != nil {
			return err
		}
	}
	return nil
}

func (s *RunService) ReviseExplorationNode(ctx context.Context, runID, nodeID uuid.UUID, expectedVersion int, payload json.RawMessage, state, reason, actor string) error {
	if strings.TrimSpace(reason) == "" || strings.TrimSpace(actor) == "" {
		return fmt.Errorf("reason and actor are required")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var epoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&epoch); err != nil {
		return err
	}
	if err := validateContextEpoch(ctx, epoch); err != nil {
		return err
	}
	var kind, currentState string
	var version int
	var oldPayload []byte
	if err = tx.QueryRow(ctx, `SELECT kind,state,content_version,payload FROM exploration_nodes WHERE id=$1 AND run_id=$2 FOR UPDATE`, nodeID, runID).Scan(&kind, &currentState, &version, &oldPayload); err != nil {
		return err
	}
	if version != expectedVersion {
		return ErrConflict
	}
	if kind != "goal" && kind != "fact" && kind != "hint" {
		return fmt.Errorf("node kind cannot be edited")
	}
	if payload == nil {
		payload = oldPayload
	}
	var object map[string]any
	if json.Unmarshal(payload, &object) != nil {
		return fmt.Errorf("invalid node payload")
	}
	if kind == "goal" {
		text, _ := object["text"].(string)
		if strings.TrimSpace(text) == "" {
			return fmt.Errorf("goal text is required")
		}
		if state == "" {
			state = "open"
		}
		if state != "open" && state != "abandoned" {
			return fmt.Errorf("goal changes require re-proof")
		}
	}
	if kind == "fact" && currentState == "origin" {
		return fmt.Errorf("task origin is not editable evidence")
	}
	if kind == "fact" {
		if state == "" {
			state = "unverified"
		}
		if state != "retracted" && state != "invalidated" && state != "unverified" {
			return fmt.Errorf("edited facts require fresh evidence")
		}
		object["verified_source"] = false
		payload, _ = json.Marshal(object)
	}
	if kind == "hint" && state == "" {
		state = "open"
	}
	if _, err = tx.Exec(ctx, `INSERT INTO exploration_revisions(run_id,node_id,version,payload,state,reason,actor) VALUES($1,$2,$3,$4,$5,$6,$7)`, runID, nodeID, version, json.RawMessage(oldPayload), currentState, reason, actor); err != nil {
		return err
	}
	if _, err = tx.Exec(ctx, `UPDATE exploration_nodes SET payload=$1,state=$2,content_version=content_version+1,completed_at=NULL,updated_at=now() WHERE id=$3`, payload, state, nodeID); err != nil {
		return err
	}
	if err = invalidateProofs(ctx, tx, runID, nodeID, reason); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *RunService) LatestExplorationChange(ctx context.Context, runID uuid.UUID, epoch int) (int64, error) {
	var seq int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0) FROM exploration_changes WHERE run_id=$1 AND epoch_number=$2`, runID, epoch).Scan(&seq)
	return seq, err
}
func (s *RunService) AckPlannerChanges(ctx context.Context, runID uuid.UUID, epoch int, seq int64) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO planner_cursors(run_id,epoch_number,change_seq) SELECT id,$2,$3 FROM runs WHERE id=$1 AND current_epoch=$2 ON CONFLICT(run_id,epoch_number) DO UPDATE SET change_seq=GREATEST(planner_cursors.change_seq,EXCLUDED.change_seq),updated_at=now()`, runID, epoch, seq)
	return err
}

func (s *RunService) GraphNodePage(ctx context.Context, runID uuid.UUID, offset, limit int) ([]map[string]any, int, error) {
	if offset < 0 {
		return nil, 0, fmt.Errorf("invalid offset")
	}
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	var total int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1`, runID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.pool.Query(ctx, `SELECT to_jsonb(n)||jsonb_build_object('state',COALESCE(w.status,n.state)) FROM exploration_nodes n LEFT JOIN work_items w ON w.id=n.work_item_id WHERE n.run_id=$1 ORDER BY n.created_at,n.id OFFSET $2 LIMIT $3`, runID, offset, limit)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	nodes := []map[string]any{}
	for rows.Next() {
		var b []byte
		var n map[string]any
		if err := rows.Scan(&b); err != nil {
			return nil, 0, err
		}
		if err := json.Unmarshal(b, &n); err != nil {
			return nil, 0, err
		}
		nodes = append(nodes, n)
	}
	return nodes, total, rows.Err()
}
