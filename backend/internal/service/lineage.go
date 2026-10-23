package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ensureOriginTx returns the single task start for a run. An existing origin
// fact is reused. A legacy begin root is reused so a run does not grow a second
// start. A missing start is inserted as kind=fact, state=origin.
func ensureOriginTx(ctx context.Context, tx pgx.Tx, runID uuid.UUID, objective string, scope json.RawMessage, source string) (uuid.UUID, error) {
	var id uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM exploration_nodes
		WHERE run_id=$1 AND kind='fact' AND state='origin' AND delete_reason IS NULL
		ORDER BY created_at LIMIT 1`, runID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM exploration_nodes
		WHERE run_id=$1 AND kind='begin' AND delete_reason IS NULL
		ORDER BY created_at LIMIT 1`, runID).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	if len(scope) == 0 || string(scope) == "null" {
		var stored []byte
		if err := tx.QueryRow(ctx, `SELECT execution_input FROM runs WHERE id=$1`, runID).Scan(&stored); err == nil {
			var input map[string]json.RawMessage
			if json.Unmarshal(stored, &input) == nil {
				if objective == "" {
					_ = json.Unmarshal(input["objective"], &objective)
				}
				if raw, ok := input["scope"]; ok {
					scope = raw
				}
			}
		}
	}
	if scope == nil {
		scope = json.RawMessage(`{}`)
	}
	label := source
	if label == "" {
		label = "user"
	}
	payload, _ := json.Marshal(map[string]any{
		"summary":         objective,
		"objective":       objective,
		"scope":           json.RawMessage(scope),
		"source":          label,
		"verified_source": false,
	})
	err = tx.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state,origin,epoch_number)
		VALUES($1,'fact',$2,'origin','run_snapshot',COALESCE((SELECT current_epoch FROM runs WHERE id=$1),1))
		ON CONFLICT (run_id) WHERE kind='fact' AND state='origin' AND delete_reason IS NULL
		DO NOTHING
		RETURNING id`, runID, payload).Scan(&id)
	if err == nil {
		return id, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, err
	}
	err = tx.QueryRow(ctx, `SELECT id FROM exploration_nodes
		WHERE run_id=$1 AND kind='fact' AND state='origin' AND delete_reason IS NULL
		ORDER BY created_at LIMIT 1`, runID).Scan(&id)
	return id, err
}

func insertExplorationEdgeTx(ctx context.Context, tx pgx.Tx, runID, src, dst uuid.UUID, rel, reason string) error {
	if src == uuid.Nil || dst == uuid.Nil {
		return fmt.Errorf("exploration edge requires endpoints")
	}
	_, err := tx.Exec(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel,reason)
		VALUES($1,$2,$3,$4,$5)
		ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING`, runID, src, dst, rel, reason)
	return err
}

func linkOriginDerivedTx(ctx context.Context, tx pgx.Tx, runID, intentID uuid.UUID) error {
	originID, err := ensureOriginTx(ctx, tx, runID, "", nil, "user")
	if err != nil {
		return err
	}
	return insertExplorationEdgeTx(ctx, tx, runID, originID, intentID, "derived_from", "no_explicit_source")
}

func linkOriginSpawnsTx(ctx context.Context, tx pgx.Tx, runID, goalID uuid.UUID) error {
	originID, err := ensureOriginTx(ctx, tx, runID, "", nil, "user")
	if err != nil {
		return err
	}
	return insertExplorationEdgeTx(ctx, tx, runID, originID, goalID, "spawns", "set_goals")
}

// validateKnowledgeParent rejects missing, cross-run, deleted, or non-knowledge
// parents. It does not substitute the task origin.
func validateKnowledgeParent(ctx context.Context, tx pgx.Tx, runID, id uuid.UUID) error {
	var kind, state string
	var deleted *string
	err := tx.QueryRow(ctx, `SELECT kind, state, delete_reason FROM exploration_nodes WHERE id=$1 AND run_id=$2`, id, runID).Scan(&kind, &state, &deleted)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: source %s is not a fact or finding in this run", ErrInvalidEvidence, id)
	}
	if err != nil {
		return err
	}
	if kind != "fact" && kind != "finding" {
		return fmt.Errorf("%w: source %s must be fact or finding", ErrInvalidEvidence, id)
	}
	if deleted != nil || state == "retracted" || state == "invalidated" {
		return fmt.Errorf("%w: source %s is not usable", ErrInvalidEvidence, id)
	}
	return nil
}
