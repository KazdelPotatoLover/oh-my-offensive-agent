package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"agent-workbench/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func prepareIntent(ctx context.Context, tx pgx.Tx, runID uuid.UUID, item *PlanItemSpec, input []byte) (string, error) {
	if item.WorkType != "intent" {
		return "", nil
	}
	if strings.TrimSpace(item.Objective) == "" {
		return "", fmt.Errorf("intent objective is required")
	}
	if item.InputData == nil {
		item.InputData = map[string]any{}
	}
	name, _ := item.InputData["profile"].(string)
	p, err := model.ProfileByName(input, name)
	if err != nil {
		return "", err
	}
	item.InputData["profile"], item.InputData["profile_hash"] = p.Name, p.Fingerprint()
	assets := []string{}
	for _, id := range item.AssetIDs {
		assets = append(assets, id.String())
	}
	sort.Strings(assets)
	sources := []string{}
	seen := map[uuid.UUID]bool{}
	for _, id := range item.SourceFactIDs {
		if seen[id] {
			return "", fmt.Errorf("duplicate source fact")
		}
		seen[id] = true
		var version int
		var state, kind string
		if err := tx.QueryRow(ctx, `SELECT content_version,state,kind FROM exploration_nodes WHERE id=$1 AND run_id=$2 AND kind IN ('fact','finding') AND delete_reason IS NULL`, id, runID).Scan(&version, &state, &kind); err != nil {
			return "", fmt.Errorf("invalid source fact: %w", err)
		}
		if state == "invalidated" || state == "retracted" {
			return "", ErrInvalidEvidence
		}
		_ = kind
		sources = append(sources, fmt.Sprintf("%s@%d", id, version))
	}
	sort.Strings(sources)
	item.InputData["source_fact_versions"] = sources
	identity, _ := json.Marshal(map[string]any{"objective": strings.Join(strings.Fields(strings.ToLower(item.Objective)), " "), "assets": assets, "profile": p.Fingerprint(), "sources": sources, "direction": item.InputData["direction"], "condition": item.InputData["condition"]})
	hash := sha256.Sum256(identity)
	return hex.EncodeToString(hash[:]), nil
}

func linkPlannedIntent(ctx context.Context, tx pgx.Tx, runID, workID uuid.UUID, item PlanItemSpec) error {
	payload, _ := json.Marshal(map[string]any{"summary": item.Objective, "text": item.Objective, "profile": item.InputData["profile"], "profile_hash": item.InputData["profile_hash"], "key": item.Key, "source_fact_ids": item.SourceFactIDs})
	var nodeID uuid.UUID
	err := tx.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state,work_item_id,epoch_number) VALUES($1,'intent',$2,'open',$3,(SELECT current_epoch FROM runs WHERE id=$1)) RETURNING id`, runID, payload, workID).Scan(&nodeID)
	if err != nil {
		return err
	}
	for _, assetID := range item.AssetIDs {
		if _, err = tx.Exec(ctx, `INSERT INTO exploration_anchors(run_id,node_id,asset_id) VALUES($1,$2,$3)`, runID, nodeID, assetID); err != nil {
			return err
		}
	}
	if len(item.SourceFactIDs) == 0 {
		return linkOriginDerivedTx(ctx, tx, runID, nodeID)
	}
	for _, factID := range item.SourceFactIDs {
		tag, err := tx.Exec(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel,source_version,target_version)
			SELECT $1,id,$3,'derived_from',content_version,1 FROM exploration_nodes
			WHERE id=$2 AND run_id=$1 AND kind IN ('fact','finding') AND delete_reason IS NULL
			  AND state NOT IN ('retracted','invalidated')
			ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING`, runID, factID, nodeID)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='derived_from')`, runID, factID, nodeID).Scan(&exists); err != nil {
				return err
			}
			if !exists {
				return fmt.Errorf("%w: source %s is not a usable fact or finding", ErrInvalidEvidence, factID)
			}
		}
	}
	return nil
}
