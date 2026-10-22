package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

var (
	ErrNodeNotFound     = errors.New("exploration node not found")
	ErrCrossRunBoundary = errors.New("cross-run boundary violation")
	ErrInvalidRel       = errors.New("invalid exploration relation")
	ErrInvalidEvidence  = errors.New("invalid evidence for goal proof")
)

// SetGoals sets or decomposes goals for a run.
func (s *RunService) SetGoals(ctx context.Context, runID uuid.UUID, specs []model.GoalSpec) ([]*model.ExplorationNode, error) {
	if len(specs) == 0 {
		return nil, fmt.Errorf("goals list cannot be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var knowledgeEpoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&knowledgeEpoch); err != nil {
		return nil, err
	}
	if err := validateContextEpoch(ctx, knowledgeEpoch); err != nil {
		return nil, err
	}

	var nodes []*model.ExplorationNode
	for _, spec := range specs {
		if strings.TrimSpace(spec.Text) == "" {
			continue
		}
		payloadBytes, _ := json.Marshal(map[string]any{
			"text":      spec.Text,
			"vulnclass": spec.VulnClass,
		})

		var n model.ExplorationNode
		err := tx.QueryRow(ctx,
			`INSERT INTO exploration_nodes (run_id, kind, payload, state, priority,epoch_number)
			 VALUES ($1, 'goal', $2, 'open', 0,$3)
			 RETURNING id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at`,
			runID, payloadBytes, knowledgeEpoch,
		).Scan(
			&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
			&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
			&n.WorkItemID, &n.FindingID, &n.ContentVersion,
			&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("insert goal node: %w", err)
		}
		nodes = append(nodes, &n)
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit goals: %w", err)
	}
	return nodes, nil
}

// ListGoals returns all goal nodes for a run.
func (s *RunService) ListGoals(ctx context.Context, runID uuid.UUID) ([]*model.ExplorationNode, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at
		 FROM exploration_nodes
		 WHERE run_id = $1 AND kind = 'goal'
		 ORDER BY created_at ASC`,
		runID,
	)
	if err != nil {
		return nil, fmt.Errorf("query goals: %w", err)
	}
	defer rows.Close()

	var nodes []*model.ExplorationNode
	for rows.Next() {
		var n model.ExplorationNode
		if err := rows.Scan(
			&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
			&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
			&n.WorkItemID, &n.FindingID, &n.ContentVersion,
			&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan goal node: %w", err)
		}
		nodes = append(nodes, &n)
	}
	if nodes == nil {
		nodes = []*model.ExplorationNode{}
	}
	return nodes, nil
}

var uuidRegex = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)

func (s *RunService) verifyFactEvidenceSource(ctx context.Context, tx pgx.Tx, runID uuid.UUID, evidence string, assetIDs []uuid.UUID) (*uuid.UUID, *uuid.UUID, bool) {
	for _, raw := range uuidRegex.FindAllString(evidence, -1) {
		id, err := uuid.Parse(raw)
		if err != nil {
			continue
		}
		if validFactTool(ctx, tx, runID, id, assetIDs) {
			return &id, nil, true
		}
		var sourceID uuid.UUID
		var assetID *uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT source_tool_call_id,asset_id FROM observations WHERE id=$1 AND run_id=$2`, id, runID).Scan(&sourceID, &assetID); err == nil {
			anchors := assetIDs
			if len(anchors) == 0 && assetID != nil {
				anchors = []uuid.UUID{*assetID}
			}
			if validFactTool(ctx, tx, runID, sourceID, anchors) {
				return &sourceID, &id, true
			}
		}
	}
	return nil, nil, false
}

func validFactTool(ctx context.Context, tx pgx.Tx, runID, callID uuid.UUID, assetIDs []uuid.UUID) bool {
	var name, status, output string
	var callRun uuid.UUID
	var epoch, current int
	var args []byte
	if err := tx.QueryRow(ctx, `SELECT tc.tool_name,tc.status,tc.run_id,tc.epoch_number,r.current_epoch,tc.arguments,COALESCE(tc.output,'') FROM agent_tool_calls tc JOIN runs r ON r.id=tc.run_id WHERE tc.id=$1`, callID).Scan(&name, &status, &callRun, &epoch, &current, &args, &output); err != nil {
		return false
	}
	if callRun != runID || status != "success" || epoch != current {
		return false
	}
	var in struct {
		AssetID    uuid.UUID `json:"asset_id"`
		SampleID   string    `json:"sample_id"`
		ArtifactID uuid.UUID `json:"artifact_id"`
	}
	if json.Unmarshal(args, &in) != nil {
		return false
	}
	var assetID uuid.UUID
	switch name {
	case "get_endpoint":
		assetID = in.AssetID
		if _, err := targetEvidence(ctx, tx, runID, assetID, callID, current, nil); err != nil {
			return false
		}
	case "http_request":
		if err := tx.QueryRow(ctx, `SELECT asset_id FROM http_receipts WHERE source_tool_call_id=$1 AND run_id=$2 AND epoch_number=$3 AND minted_by='gateway' AND asset_id IS NOT NULL`, callID, runID, current).Scan(&assetID); err != nil {
			return false
		}
		if _, err := targetEvidence(ctx, tx, runID, assetID, callID, current, nil); err != nil {
			return false
		}
	case "read_sample":
		var response targetResponse
		if json.Unmarshal([]byte(output), &response) != nil || response.StatusCode < 100 || response.StatusCode > 599 {
			return false
		}
		if in.SampleID == "" {
			return false
		}
		if err := tx.QueryRow(ctx, `SELECT a.id FROM assets a JOIN run_assets ra ON ra.asset_id=a.id WHERE ra.run_id=$1 AND a.kind='sample_endpoint' AND a.external_key=$2`, runID, in.SampleID).Scan(&assetID); err != nil {
			return false
		}
	case "inspect_artifact":
		var sourceID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT a.source_tool_call_id FROM artifacts a JOIN agent_tool_calls tc ON tc.id=a.source_tool_call_id WHERE a.id=$1 AND a.run_id=$2 AND tc.tool_name IN ('get_endpoint','read_sample')`, in.ArtifactID, runID).Scan(&sourceID); err != nil {
			return false
		}
		return validFactTool(ctx, tx, runID, sourceID, assetIDs)
	default:
		return false
	}
	var scoped bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2)`, runID, assetID).Scan(&scoped); err != nil || !scoped {
		return false
	}
	for _, expected := range assetIDs {
		if expected != assetID {
			return false
		}
	}
	return true
}

// RecordFact records an empirical or observed fact in the exploration graph.
func (s *RunService) RecordFact(
	ctx context.Context,
	runID uuid.UUID,
	summary, detail, confidence, evidence string,
	intentNodeID *uuid.UUID,
	assetIDs []uuid.UUID,
) (*model.ExplorationNode, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, fmt.Errorf("summary cannot be empty")
	}
	if confidence == "" {
		confidence = "observed"
	}
	if confidence != "observed" && confidence != "inferred" {
		return nil, fmt.Errorf("confidence must be observed or inferred")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var knowledgeEpoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&knowledgeEpoch); err != nil {
		return nil, err
	}
	if err := validateContextEpoch(ctx, knowledgeEpoch); err != nil {
		return nil, err
	}
	for _, assetID := range assetIDs {
		var inScope bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2)`, runID, assetID).Scan(&inScope); err != nil {
			return nil, err
		}
		if !inScope {
			return nil, ErrAssetOutOfScope
		}
	}

	// Resolve intentNodeID if it's a work item ID
	var resolvedIntentID *uuid.UUID
	if intentNodeID != nil && *intentNodeID != uuid.Nil {
		var existingID uuid.UUID
		var existingRunID uuid.UUID
		var existingKind string
		err := tx.QueryRow(ctx,
			`SELECT id, run_id, kind FROM exploration_nodes WHERE id = $1`,
			*intentNodeID,
		).Scan(&existingID, &existingRunID, &existingKind)
		if err == nil {
			if existingRunID != runID {
				return nil, fmt.Errorf("%w: intent node belongs to another run", ErrCrossRunBoundary)
			}
			if existingKind != "intent" {
				return nil, ErrInvalidEvidence
			}
			resolvedIntentID = &existingID
		} else if errors.Is(err, pgx.ErrNoRows) {
			// Check if intentNodeID matches a work_item_id
			var nodeID uuid.UUID
			err2 := tx.QueryRow(ctx,
				`SELECT id FROM exploration_nodes WHERE work_item_id = $1 AND run_id = $2`,
				*intentNodeID, runID,
			).Scan(&nodeID)
			if err2 == nil {
				resolvedIntentID = &nodeID
			} else {
				var workObjective string
				if err := tx.QueryRow(ctx, `SELECT objective FROM work_items WHERE id=$1 AND run_id=$2`, *intentNodeID, runID).Scan(&workObjective); err != nil {
					return nil, ErrCrossRunBoundary
				}
				payload, _ := json.Marshal(map[string]any{"summary": workObjective, "adapter": "legacy"})
				if err := tx.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state,work_item_id,epoch_number) VALUES($1,'intent',$2,'open',$3,$4) RETURNING id`, runID, payload, *intentNodeID, knowledgeEpoch).Scan(&nodeID); err != nil {
					return nil, err
				}
				resolvedIntentID = &nodeID
			}
		} else {
			return nil, fmt.Errorf("query intent node: %w", err)
		}
	}

	factState := "unverified"
	var sourceTCID, sourceObsID *uuid.UUID
	var isSourceVerified bool

	// Only mark verified if confidence is observed AND evidence references a real successful tool call or observation
	if confidence == "observed" && strings.TrimSpace(evidence) != "" {
		sourceTCID, sourceObsID, isSourceVerified = s.verifyFactEvidenceSource(ctx, tx, runID, evidence, assetIDs)
		if isSourceVerified {
			factState = "verified"
		}
	}

	payloadMap := map[string]any{
		"summary":         summary,
		"detail":          detail,
		"confidence":      confidence,
		"evidence":        evidence,
		"verified_source": isSourceVerified,
	}
	if sourceTCID != nil {
		payloadMap["source_tool_call_id"] = sourceTCID.String()
	}
	if sourceObsID != nil {
		payloadMap["source_observation_id"] = sourceObsID.String()
	}
	payloadBytes, _ := json.Marshal(payloadMap)
	identityAssets := []string{}
	for _, id := range assetIDs {
		identityAssets = append(identityAssets, id.String())
	}
	sort.Strings(identityAssets)
	identity, _ := json.Marshal([]any{strings.Join(strings.Fields(strings.ToLower(summary)), " "), strings.TrimSpace(detail), confidence, sourceTCID, sourceObsID, identityAssets})
	digest := sha256.Sum256(identity)
	fingerprint := hex.EncodeToString(digest[:])

	var n model.ExplorationNode
	err = tx.QueryRow(ctx,
		`INSERT INTO exploration_nodes (run_id, kind, payload, state, priority,epoch_number,fact_fingerprint)
		 VALUES ($1, 'fact', $2, $3, 0,$4,$5)
		 ON CONFLICT(run_id,fact_fingerprint) WHERE fact_fingerprint IS NOT NULL DO UPDATE SET run_id=EXCLUDED.run_id
		 RETURNING id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at,(xmax=0)`,
		runID, payloadBytes, factState, knowledgeEpoch, fingerprint,
	).Scan(
		&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
		&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
		&n.WorkItemID, &n.FindingID, &n.ContentVersion,
		&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt, &n.IsNew,
	)
	if err != nil {
		return nil, fmt.Errorf("insert fact node: %w", err)
	}

	// Link intent --yields--> fact
	if resolvedIntentID != nil {
		_, err = tx.Exec(ctx,
			`INSERT INTO exploration_edges (run_id, src_id, dst_id, rel)
			 VALUES ($1, $2, $3, 'yields')
			 ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING`,
			runID, *resolvedIntentID, n.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("insert yields edge: %w", err)
		}
	}

	// Link anchors
	for _, aid := range assetIDs {
		if aid == uuid.Nil {
			continue
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO exploration_anchors (run_id, node_id, asset_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (node_id, asset_id) DO NOTHING`,
			runID, n.ID, aid,
		)
		if err != nil {
			return nil, fmt.Errorf("insert anchor: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit fact: %w", err)
	}
	return &n, nil
}

// ListFacts returns facts for a run.
func (s *RunService) ListFacts(ctx context.Context, runID uuid.UUID, limit int) ([]*model.ExplorationNode, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.pool.Query(ctx,
		`SELECT id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at
		 FROM exploration_nodes
		 WHERE run_id = $1 AND kind = 'fact'
		 ORDER BY created_at DESC
		 LIMIT $2`,
		runID, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("query facts: %w", err)
	}
	defer rows.Close()

	var nodes []*model.ExplorationNode
	for rows.Next() {
		var n model.ExplorationNode
		if err := rows.Scan(
			&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
			&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
			&n.WorkItemID, &n.FindingID, &n.ContentVersion,
			&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt,
		); err != nil {
			return nil, fmt.Errorf("scan fact node: %w", err)
		}
		nodes = append(nodes, &n)
	}
	if nodes == nil {
		nodes = []*model.ExplorationNode{}
	}
	return nodes, nil
}

// RecordHint adds a strategic or human hint node to the exploration graph.
func (s *RunService) RecordHint(ctx context.Context, runID uuid.UUID, text string, assetIDs []uuid.UUID) (*model.ExplorationNode, error) {
	if strings.TrimSpace(text) == "" {
		return nil, fmt.Errorf("hint text cannot be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var knowledgeEpoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&knowledgeEpoch); err != nil {
		return nil, err
	}
	if err := validateContextEpoch(ctx, knowledgeEpoch); err != nil {
		return nil, err
	}
	for _, assetID := range assetIDs {
		var inScope bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2)`, runID, assetID).Scan(&inScope); err != nil {
			return nil, err
		}
		if !inScope {
			return nil, ErrAssetOutOfScope
		}
	}

	payloadBytes, _ := json.Marshal(map[string]any{"text": text})

	var n model.ExplorationNode
	err = tx.QueryRow(ctx,
		`INSERT INTO exploration_nodes (run_id, kind, payload, state, priority,epoch_number)
		 VALUES ($1, 'hint', $2, 'open', 0,$3)
		 RETURNING id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at`,
		runID, payloadBytes, knowledgeEpoch,
	).Scan(
		&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
		&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
		&n.WorkItemID, &n.FindingID, &n.ContentVersion,
		&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert hint node: %w", err)
	}

	for _, aid := range assetIDs {
		if aid == uuid.Nil {
			continue
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO exploration_anchors (run_id, node_id, asset_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (node_id, asset_id) DO NOTHING`,
			runID, n.ID, aid,
		)
		if err != nil {
			return nil, fmt.Errorf("insert anchor: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit hint: %w", err)
	}
	return &n, nil
}

// AddIntentNode creates an intent exploration node linked to upstream parents (facts/findings).
func (s *RunService) AddIntentNode(
	ctx context.Context,
	runID uuid.UUID,
	summary string,
	parentIDs []uuid.UUID,
	assetIDs []uuid.UUID,
	priority int,
	workItemID *uuid.UUID,
) (*model.ExplorationNode, error) {
	if strings.TrimSpace(summary) == "" {
		return nil, fmt.Errorf("intent summary cannot be empty")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)
	var knowledgeEpoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&knowledgeEpoch); err != nil {
		return nil, err
	}
	if err := validateContextEpoch(ctx, knowledgeEpoch); err != nil {
		return nil, err
	}
	for _, assetID := range assetIDs {
		var inScope bool
		if err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2)`, runID, assetID).Scan(&inScope); err != nil {
			return nil, err
		}
		if !inScope {
			return nil, ErrAssetOutOfScope
		}
	}

	// Validate parent IDs
	for _, pid := range parentIDs {
		if pid == uuid.Nil {
			continue
		}
		var pRunID uuid.UUID
		var pKind string
		err := tx.QueryRow(ctx,
			`SELECT run_id, kind FROM exploration_nodes WHERE id = $1`,
			pid,
		).Scan(&pRunID, &pKind)
		if err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return nil, fmt.Errorf("%w: parent node %s not found", ErrNodeNotFound, pid)
			}
			return nil, fmt.Errorf("validate parent node %s: %w", pid, err)
		}
		if pRunID != runID {
			return nil, fmt.Errorf("%w: parent node %s belongs to another run", ErrCrossRunBoundary, pid)
		}
		if pKind != "fact" && pKind != "finding" && pKind != "hint" && pKind != "begin" {
			return nil, fmt.Errorf("parent node %s must be fact, finding, hint, or begin (got %s)", pid, pKind)
		}
	}

	payloadBytes, _ := json.Marshal(map[string]any{"summary": summary})

	var n model.ExplorationNode
	err = tx.QueryRow(ctx,
		`INSERT INTO exploration_nodes (run_id, kind, payload, state, priority, work_item_id,epoch_number)
		 VALUES ($1, 'intent', $2, 'open', $3, $4,$5)
		 ON CONFLICT(work_item_id) DO UPDATE SET work_item_id=EXCLUDED.work_item_id
		 RETURNING id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at`,
		runID, payloadBytes, priority, workItemID, knowledgeEpoch,
	).Scan(
		&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
		&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
		&n.WorkItemID, &n.FindingID, &n.ContentVersion,
		&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("insert intent node: %w", err)
	}

	// Link parent --derived_from--> intent
	for _, pid := range parentIDs {
		if pid == uuid.Nil {
			continue
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO exploration_edges (run_id, src_id, dst_id, rel)
			 VALUES ($1, $2, $3, 'derived_from')
			 ON CONFLICT (run_id, src_id, dst_id, rel) DO NOTHING`,
			runID, pid, n.ID,
		)
		if err != nil {
			return nil, fmt.Errorf("insert derived_from edge: %w", err)
		}
	}

	// Link anchors
	for _, aid := range assetIDs {
		if aid == uuid.Nil {
			continue
		}
		_, err = tx.Exec(ctx,
			`INSERT INTO exploration_anchors (run_id, node_id, asset_id)
			 VALUES ($1, $2, $3)
			 ON CONFLICT (node_id, asset_id) DO NOTHING`,
			runID, n.ID, aid,
		)
		if err != nil {
			return nil, fmt.Errorf("insert anchor: %w", err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit intent: %w", err)
	}
	return &n, nil
}

// ProveGoal marks a goal as met supported by verified fact or finding evidence.
// Returns (allGoalsMet, error).
func (s *RunService) ProveGoal(
	ctx context.Context,
	runID, goalID, evidenceID uuid.UUID,
	reason string,
) (bool, error) {
	if goalID == uuid.Nil || evidenceID == uuid.Nil {
		return false, fmt.Errorf("goal_id and evidence_id are required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, fmt.Errorf("begin transaction: %w", err)
	}
	defer tx.Rollback(ctx)

	var currentEpoch int
	if err = tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&currentEpoch); err != nil {
		return false, err
	}
	if err := validateContextEpoch(ctx, currentEpoch); err != nil {
		return false, err
	}

	if strings.TrimSpace(reason) == "" {
		return false, fmt.Errorf("proof reason is required")
	}
	// 1. Verify Goal Node
	var gRunID uuid.UUID
	var gKind, gState string
	var goalVersion, evidenceVersion int
	var gDeleteReason, gBlockedReason *string
	err = tx.QueryRow(ctx,
		`SELECT run_id, kind, state, delete_reason, blocked_reason,content_version FROM exploration_nodes WHERE id = $1`,
		goalID,
	).Scan(&gRunID, &gKind, &gState, &gDeleteReason, &gBlockedReason, &goalVersion)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false, fmt.Errorf("%w: goal %s not found", ErrNodeNotFound, goalID)
		}
		return false, fmt.Errorf("query goal: %w", err)
	}
	if gRunID != runID {
		return false, fmt.Errorf("%w: goal belongs to another run", ErrCrossRunBoundary)
	}
	if gKind != "goal" {
		return false, fmt.Errorf("node %s is kind '%s', not 'goal'", goalID, gKind)
	}
	if gState == "abandoned" {
		return false, fmt.Errorf("%w: cannot prove abandoned goal %s", ErrInvalidEvidence, goalID)
	}
	if gDeleteReason != nil {
		return false, fmt.Errorf("%w: goal %s has been deleted (%s)", ErrInvalidEvidence, goalID, *gDeleteReason)
	}

	// 2. Verify Evidence Node
	var actualEvidenceNodeID uuid.UUID
	var evRunID uuid.UUID
	var evKind, evState string
	var evPayload json.RawMessage
	var evDeleteReason, evBlockedReason *string
	err = tx.QueryRow(ctx,
		`SELECT id, run_id, kind, state, payload, delete_reason, blocked_reason,content_version FROM exploration_nodes WHERE id = $1`,
		evidenceID,
	).Scan(&actualEvidenceNodeID, &evRunID, &evKind, &evState, &evPayload, &evDeleteReason, &evBlockedReason, &evidenceVersion)
	if err == nil {
		if evRunID != runID {
			return false, fmt.Errorf("%w: evidence node %s belongs to run %s (current %s)", ErrCrossRunBoundary, evidenceID, evRunID, runID)
		}
		if evKind != "fact" && evKind != "finding" {
			return false, fmt.Errorf("%w: evidence node must be fact or finding, got %s", ErrInvalidEvidence, evKind)
		}
		if evDeleteReason != nil {
			return false, fmt.Errorf("%w: evidence node %s is invalidated (%s)", ErrInvalidEvidence, evidenceID, *evDeleteReason)
		}
		if evBlockedReason != nil {
			return false, fmt.Errorf("%w: evidence node %s is blocked (%s)", ErrInvalidEvidence, evidenceID, *evBlockedReason)
		}
		if evState != "verified" {
			return false, fmt.Errorf("%w: evidence node %s has state '%s', must be 'verified' to prove a goal", ErrInvalidEvidence, evidenceID, evState)
		}
		if evKind == "fact" {
			var pMap map[string]any
			_ = json.Unmarshal(evPayload, &pMap)
			if vs, ok := pMap["verified_source"].(bool); !ok || !vs {
				return false, fmt.Errorf("%w: fact node %s has no verified tool or observation source backing it", ErrInvalidEvidence, evidenceID)
			}
		}
	} else if errors.Is(err, pgx.ErrNoRows) {
		// Check if evidenceID points to a finding in findings table
		var fRunID uuid.UUID
		var fVerificationState, fStatus string
		err2 := tx.QueryRow(ctx,
			`SELECT run_id, verification_state, status FROM findings WHERE id = $1`,
			evidenceID,
		).Scan(&fRunID, &fVerificationState, &fStatus)
		if err2 != nil {
			if errors.Is(err2, pgx.ErrNoRows) {
				return false, fmt.Errorf("%w: evidence %s not found in exploration nodes or findings", ErrInvalidEvidence, evidenceID)
			}
			return false, fmt.Errorf("query finding evidence: %w", err2)
		}
		if fRunID != runID {
			return false, fmt.Errorf("%w: finding evidence belongs to another run", ErrCrossRunBoundary)
		}
		if fStatus == "false_positive" || fStatus == "deleted" {
			return false, fmt.Errorf("%w: finding %s has status '%s', cannot prove a goal", ErrInvalidEvidence, evidenceID, fStatus)
		}
		if fVerificationState != "verified" {
			return false, fmt.Errorf("%w: finding %s has verification_state '%s', must be 'verified' to prove a goal", ErrInvalidEvidence, evidenceID, fVerificationState)
		}
		evKind = "finding"

		// Check or create exploration node for this finding
		var existingExpID uuid.UUID
		err3 := tx.QueryRow(ctx,
			`SELECT id FROM exploration_nodes WHERE finding_id = $1 AND run_id = $2`,
			evidenceID, runID,
		).Scan(&existingExpID)
		if err3 == nil {
			actualEvidenceNodeID = existingExpID
		} else if errors.Is(err3, pgx.ErrNoRows) {
			// Create exploration node for the finding
			findingPayload := json.RawMessage(`{"summary": "Finding evidence"}`)
			err4 := tx.QueryRow(ctx,
				`INSERT INTO exploration_nodes (run_id, kind, payload, state, finding_id,epoch_number)
				 VALUES ($1, 'finding', $2, 'verified', $3,$4)
				 RETURNING id`,
				runID, findingPayload, evidenceID, currentEpoch,
			).Scan(&actualEvidenceNodeID)
			if err4 != nil {
				return false, fmt.Errorf("create finding node for evidence: %w", err4)
			}
		} else {
			return false, fmt.Errorf("query finding node: %w", err3)
		}
	} else {
		return false, fmt.Errorf("query evidence node: %w", err)
	}

	if evidenceVersion == 0 {
		if err := tx.QueryRow(ctx, `SELECT content_version FROM exploration_nodes WHERE id=$1`, actualEvidenceNodeID).Scan(&evidenceVersion); err != nil {
			return false, err
		}
	}
	if evKind == "finding" {
		var valid bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM exploration_nodes n JOIN findings f ON f.id=n.finding_id WHERE n.id=$1 AND f.status NOT IN ('false_positive','deleted') AND COALESCE(f.retest_verdict,'')!='fixed')`, actualEvidenceNodeID).Scan(&valid); err != nil {
			return false, err
		}
		if !valid {
			return false, ErrInvalidEvidence
		}
	}
	// 3. Link evidence --proves--> goal
	var reasonPtr *string
	if strings.TrimSpace(reason) != "" {
		reasonPtr = &reason
	}
	_, err = tx.Exec(ctx,
		`INSERT INTO exploration_edges (run_id, src_id, dst_id, rel, reason,source_version,target_version)
		 VALUES ($1, $2, $3, 'proves', $4,$5,$6)
		 ON CONFLICT (run_id, src_id, dst_id, rel) DO UPDATE SET reason = EXCLUDED.reason,source_version=EXCLUDED.source_version,target_version=EXCLUDED.target_version,valid=true,invalid_reason=NULL,invalidated_at=NULL`,
		runID, actualEvidenceNodeID, goalID, reasonPtr, evidenceVersion, goalVersion,
	)
	if err != nil {
		return false, fmt.Errorf("insert proves edge: %w", err)
	}

	// 4. Update goal state to met
	now := time.Now()
	_, err = tx.Exec(ctx,
		`UPDATE exploration_nodes
		 SET state = 'met', completed_at = $1, updated_at = $1
		 WHERE id = $2`,
		now, goalID,
	)
	if err != nil {
		return false, fmt.Errorf("update goal state: %w", err)
	}

	// 5. Check if all goals in run are met
	var totalGoals, metGoals int
	err = tx.QueryRow(ctx,
		`SELECT count(*), count(*) FILTER (WHERE state = 'met')
		 FROM exploration_nodes
		 WHERE run_id = $1 AND kind = 'goal'`,
		runID,
	).Scan(&totalGoals, &metGoals)
	if err != nil {
		return false, fmt.Errorf("check all goals met: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("commit prove goal: %w", err)
	}

	allMet := totalGoals > 0 && metGoals == totalGoals
	return allMet, nil
}

// GetExplorationNode returns a single exploration node by ID.
func (s *RunService) GetExplorationNode(ctx context.Context, nodeID uuid.UUID) (*model.ExplorationNode, error) {
	var n model.ExplorationNode
	err := s.pool.QueryRow(ctx,
		`SELECT id, run_id, kind, payload, priority, state, origin, owner, blocked_reason, delete_reason, work_item_id, finding_id, content_version, created_at, updated_at, completed_at
		 FROM exploration_nodes
		 WHERE id = $1`,
		nodeID,
	).Scan(
		&n.ID, &n.RunID, &n.Kind, &n.Payload, &n.Priority, &n.State,
		&n.Origin, &n.Owner, &n.BlockedReason, &n.DeleteReason,
		&n.WorkItemID, &n.FindingID, &n.ContentVersion,
		&n.CreatedAt, &n.UpdatedAt, &n.CompletedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNodeNotFound
		}
		return nil, fmt.Errorf("query exploration node: %w", err)
	}
	return &n, nil
}

// GetExplorationNodeDetail returns full node details including anchors and edges.
func (s *RunService) GetExplorationNodeDetail(ctx context.Context, runID, nodeID uuid.UUID) (map[string]any, error) {
	node, err := s.GetExplorationNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	if node.RunID != runID {
		return nil, fmt.Errorf("%w: node belongs to another run", ErrCrossRunBoundary)
	}

	// Incoming edges
	inRows, err := s.pool.Query(ctx,
		`SELECT id, src_id, dst_id, rel, reason, created_at,valid,invalid_reason,source_version,target_version
		 FROM exploration_edges
		 WHERE dst_id = $1`,
		nodeID,
	)
	if err != nil {
		return nil, fmt.Errorf("query incoming edges: %w", err)
	}
	defer inRows.Close()

	var incoming []map[string]any
	for inRows.Next() {
		var id, srcID, dstID uuid.UUID
		var rel string
		var reason *string
		var ca time.Time
		var valid bool
		var invalidReason *string
		var sourceVersion, targetVersion *int
		if err := inRows.Scan(&id, &srcID, &dstID, &rel, &reason, &ca, &valid, &invalidReason, &sourceVersion, &targetVersion); err != nil {
			return nil, err
		}
		incoming = append(incoming, map[string]any{
			"id":         id,
			"src_id":     srcID,
			"dst_id":     dstID,
			"rel":        rel,
			"reason":     reason,
			"created_at": ca, "valid": valid, "invalid_reason": invalidReason, "source_version": sourceVersion, "target_version": targetVersion,
		})
	}

	// Outgoing edges
	outRows, err := s.pool.Query(ctx,
		`SELECT id, src_id, dst_id, rel, reason, created_at,valid,invalid_reason,source_version,target_version
		 FROM exploration_edges
		 WHERE src_id = $1`,
		nodeID,
	)
	if err != nil {
		return nil, fmt.Errorf("query outgoing edges: %w", err)
	}
	defer outRows.Close()

	var outgoing []map[string]any
	for outRows.Next() {
		var id, srcID, dstID uuid.UUID
		var rel string
		var reason *string
		var ca time.Time
		var valid bool
		var invalidReason *string
		var sourceVersion, targetVersion *int
		if err := outRows.Scan(&id, &srcID, &dstID, &rel, &reason, &ca, &valid, &invalidReason, &sourceVersion, &targetVersion); err != nil {
			return nil, err
		}
		outgoing = append(outgoing, map[string]any{
			"id":         id,
			"src_id":     srcID,
			"dst_id":     dstID,
			"rel":        rel,
			"reason":     reason,
			"created_at": ca, "valid": valid, "invalid_reason": invalidReason, "source_version": sourceVersion, "target_version": targetVersion,
		})
	}

	// Anchors
	anchorRows, err := s.pool.Query(ctx,
		`SELECT asset_id FROM exploration_anchors WHERE node_id = $1`,
		nodeID,
	)
	if err != nil {
		return nil, fmt.Errorf("query anchors: %w", err)
	}
	defer anchorRows.Close()

	var anchors []uuid.UUID
	for anchorRows.Next() {
		var aid uuid.UUID
		if err := anchorRows.Scan(&aid); err != nil {
			return nil, err
		}
		anchors = append(anchors, aid)
	}

	var payloadMap map[string]any
	_ = json.Unmarshal(node.Payload, &payloadMap)

	return map[string]any{
		"node":           node,
		"payload_parsed": payloadMap,
		"incoming_edges": incoming,
		"outgoing_edges": outgoing,
		"anchors":        anchors,
	}, nil
}

// GraphOverview provides the situational overview shared by graph_overview tool and planner prompt.
func (s *RunService) GraphOverview(ctx context.Context, runID uuid.UUID) (map[string]any, error) {
	goals, err := s.ListGoals(ctx, runID)
	if err != nil {
		return nil, err
	}

	goalsSummary := make([]map[string]any, 0, len(goals))
	totalGoals := len(goals)
	metGoals := 0
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		goalsSummary = append(goalsSummary, map[string]any{
			"id":        g.ID,
			"state":     g.State,
			"text":      p["text"],
			"vulnclass": p["vulnclass"],
		})
		if g.State == "met" {
			metGoals++
		}
	}

	// Hints
	hintRows, err := s.pool.Query(ctx,
		`SELECT id, payload, state FROM exploration_nodes WHERE run_id = $1 AND kind = 'hint' ORDER BY created_at DESC LIMIT 50`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer hintRows.Close()

	hintsSummary := make([]map[string]any, 0)
	for hintRows.Next() {
		var hid uuid.UUID
		var payload json.RawMessage
		var state string
		if err := hintRows.Scan(&hid, &payload, &state); err == nil {
			var p map[string]any
			_ = json.Unmarshal(payload, &p)
			hintsSummary = append(hintsSummary, map[string]any{
				"id":    hid,
				"state": state,
				"text":  p["text"],
			})
		}
	}

	// Recent Facts
	facts, err := s.ListFacts(ctx, runID, 20)
	if err != nil {
		return nil, err
	}
	factsSummary := make([]map[string]any, 0, len(facts))
	for _, f := range facts {
		var p map[string]any
		_ = json.Unmarshal(f.Payload, &p)
		factsSummary = append(factsSummary, map[string]any{
			"id":         f.ID,
			"summary":    p["summary"],
			"confidence": p["confidence"],
			"evidence":   p["evidence"],
		})
	}

	// Open Intents & Running Intents
	intentRows, err := s.pool.Query(ctx,
		`SELECT n.id,n.payload,COALESCE(w.status,n.state),n.priority,n.work_item_id
		 FROM exploration_nodes n LEFT JOIN work_items w ON w.id=n.work_item_id
		 WHERE n.run_id=$1 AND n.kind='intent' ORDER BY n.priority DESC,n.created_at ASC`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer intentRows.Close()

	var openIntents []map[string]any
	var runningIntents []map[string]any
	doneCount := 0

	for intentRows.Next() {
		var iid uuid.UUID
		var payload json.RawMessage
		var state string
		var prio int
		var wiID *uuid.UUID
		if err := intentRows.Scan(&iid, &payload, &state, &prio, &wiID); err == nil {
			var p map[string]any
			_ = json.Unmarshal(payload, &p)
			item := map[string]any{
				"id":           iid,
				"summary":      p["summary"],
				"priority":     prio,
				"state":        state,
				"work_item_id": wiID,
			}
			switch state {
			case "open", "pending":
				openIntents = append(openIntents, item)
			case "running":
				runningIntents = append(runningIntents, item)
			case "done", "completed", "failed", "cancelled", "blocked", "exhausted":
				doneCount++
			}
		}
	}

	edgeRows, err := s.pool.Query(ctx,
		`SELECT id, src_id, dst_id, rel FROM exploration_edges WHERE run_id = $1 AND valid = true ORDER BY created_at ASC`,
		runID,
	)
	edgesSummary := make([]map[string]any, 0)
	if err == nil {
		defer edgeRows.Close()
		for edgeRows.Next() {
			var eid, srcID, dstID uuid.UUID
			var rel string
			if err := edgeRows.Scan(&eid, &srcID, &dstID, &rel); err == nil {
				edgesSummary = append(edgesSummary, map[string]any{
					"id":  eid.String(),
					"src": srcID.String(),
					"dst": dstID.String(),
					"rel": rel,
				})
			}
		}
	}

	allGoalsMet := totalGoals > 0 && metGoals == totalGoals

	return map[string]any{
		"goals":              goalsSummary,
		"hints":              hintsSummary,
		"facts":              factsSummary,
		"open_intents":       openIntents,
		"running_intents":    runningIntents,
		"edges":              edgesSummary,
		"done_intents_total": doneCount,
		"total_goals":        totalGoals,
		"met_goals":          metGoals,
		"all_goals_met":      allGoalsMet,
	}, nil
}

// AddAssetRelation establishes a typed relationship between two assets in the asset graph.
func (s *RunService) AddAssetRelation(
	ctx context.Context,
	sourceAssetID, targetAssetID uuid.UUID,
	relationType string,
	metadata json.RawMessage,
) (*model.AssetRelation, error) {
	if sourceAssetID == uuid.Nil || targetAssetID == uuid.Nil {
		return nil, fmt.Errorf("source and target asset IDs are required")
	}
	if relationType == "" {
		return nil, fmt.Errorf("relation_type cannot be empty")
	}
	if metadata == nil {
		metadata = json.RawMessage(`{}`)
	}

	var rel model.AssetRelation
	err := s.pool.QueryRow(ctx,
		`INSERT INTO asset_relations (source_asset_id, target_asset_id, relation_type, metadata)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (source_asset_id, target_asset_id, relation_type)
		 DO UPDATE SET metadata = EXCLUDED.metadata
		 RETURNING id, source_asset_id, target_asset_id, relation_type, metadata, created_at`,
		sourceAssetID, targetAssetID, relationType, metadata,
	).Scan(&rel.ID, &rel.SourceAssetID, &rel.TargetAssetID, &rel.RelationType, &rel.Metadata, &rel.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert asset relation: %w", err)
	}
	return &rel, nil
}

// ListAssetRelations returns all incoming and outgoing relations for an asset.
func (s *RunService) ListAssetRelations(ctx context.Context, assetID uuid.UUID) ([]model.AssetRelation, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, source_asset_id, target_asset_id, relation_type, metadata, created_at
		 FROM asset_relations
		 WHERE source_asset_id = $1 OR target_asset_id = $1
		 ORDER BY created_at ASC`,
		assetID,
	)
	if err != nil {
		return nil, fmt.Errorf("query asset relations: %w", err)
	}
	defer rows.Close()

	var list []model.AssetRelation
	for rows.Next() {
		var r model.AssetRelation
		if err := rows.Scan(&r.ID, &r.SourceAssetID, &r.TargetAssetID, &r.RelationType, &r.Metadata, &r.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, r)
	}
	if list == nil {
		list = []model.AssetRelation{}
	}
	return list, nil
}
