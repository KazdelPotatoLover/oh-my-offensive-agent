package service

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ExplorationGraph is a display snapshot, separate from the planner's compact
// GraphOverview. IDs and relationships always belong to exploration_nodes.
type ExplorationGraph struct {
	Nodes []json.RawMessage `json:"nodes"`
	Edges []json.RawMessage `json:"edges"`
}

func (s *RunService) ExplorationGraph(ctx context.Context, runID uuid.UUID) (*ExplorationGraph, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var exists bool
	if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)`, runID).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, pgx.ErrNoRows
	}
	graph := &ExplorationGraph{Nodes: []json.RawMessage{}, Edges: []json.RawMessage{}}
	// Keep node state and execution state distinct. Findings carry their current
	// business/verification state without substituting their IDs for node IDs.
	rows, err := tx.Query(ctx, `SELECT to_jsonb(n) || jsonb_build_object(
		'work_status',w.status,
		'finding',CASE WHEN f.id IS NULL THEN NULL ELSE jsonb_build_object(
			'id',f.id,'title',f.title,'status',f.status,'source_kind',f.source_kind,
			'verification_state',f.verification_state,'retest_verdict',f.retest_verdict) END)
		FROM exploration_nodes n
		LEFT JOIN work_items w ON w.id=n.work_item_id AND w.run_id=n.run_id
		LEFT JOIN findings f ON f.id=n.finding_id AND f.run_id=n.run_id
		WHERE n.run_id=$1 ORDER BY n.created_at,n.id`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw json.RawMessage
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		graph.Nodes = append(graph.Nodes, raw)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	// Retain all real lineage (including cycles). Only live, version-matching
	// proofs may be drawn as proof; invalidated edges remain in the audit tables.
	rows, err = tx.Query(ctx, `SELECT jsonb_build_object(
		'id',e.id,'src',e.src_id,'dst',e.dst_id,'rel',e.rel,'reason',e.reason)
		FROM exploration_edges e
		JOIN exploration_nodes src ON src.id=e.src_id AND src.run_id=e.run_id
		JOIN exploration_nodes dst ON dst.id=e.dst_id AND dst.run_id=e.run_id
		LEFT JOIN findings f ON f.id=src.finding_id AND f.run_id=e.run_id
		WHERE e.run_id=$1 AND e.valid AND (e.rel!='proves' OR (
			src.kind IN ('fact','finding') AND src.state='verified'
			AND src.delete_reason IS NULL AND src.blocked_reason IS NULL
			AND dst.kind='goal' AND dst.state='met' AND dst.delete_reason IS NULL
			AND e.source_version=src.content_version AND e.target_version=dst.content_version
			AND (src.kind!='finding' OR (f.id IS NOT NULL AND f.status!='false_positive'
				AND f.verification_state IN ('verified','retested') AND COALESCE(f.retest_verdict,'')!='fixed'))))
		ORDER BY e.created_at,e.id`, runID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var raw json.RawMessage
		if err = rows.Scan(&raw); err != nil {
			rows.Close()
			return nil, err
		}
		graph.Edges = append(graph.Edges, raw)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	if err = tx.Commit(ctx); err != nil {
		return nil, err
	}
	return graph, nil
}
