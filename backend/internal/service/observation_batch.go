package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/model"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// MaxObservationBatch is intentionally small: a batch is a write-efficiency
// primitive for one exploration, not an unbounded import API.
const MaxObservationBatch = 16

// ObservationInput is one verified observation request. The source tool call,
// attempt and run are checked together inside the transaction before anything
// is inserted.
type ObservationInput struct {
	ObsType          string
	Content          string
	Data             json.RawMessage
	SourceToolCallID uuid.UUID
	AssetID          *uuid.UUID
}

type validatedObservation struct {
	assetID   *uuid.UUID
	sessionID uuid.UUID
	workItem  uuid.UUID
}

// CreateObservationsBatchWithAssets validates every row first and then inserts
// all rows and events atomically. A source, scope, session, attempt or asset
// error rolls the entire batch back; callers never receive a partial success.
func (s *RunService) CreateObservationsBatchWithAssets(ctx context.Context, runID, attemptID uuid.UUID, inputs []ObservationInput) ([]model.Observation, error) {
	return s.createObservationsBatchWithSession(ctx, runID, uuid.Nil, attemptID, inputs)
}

// CreateObservationsBatchWithSession is the worker-facing form. In addition to
// the run and attempt checks, it binds the batch to the session that owns the
// worker tool instance. The compatibility method above keeps older callers
// source-compatible while still enforcing the attempt/source session relation.
func (s *RunService) CreateObservationsBatchWithSession(ctx context.Context, runID, sessionID, attemptID uuid.UUID, inputs []ObservationInput) ([]model.Observation, error) {
	return s.createObservationsBatchWithSession(ctx, runID, sessionID, attemptID, inputs)
}

func (s *RunService) createObservationsBatchWithSession(ctx context.Context, runID, sessionID, attemptID uuid.UUID, inputs []ObservationInput) ([]model.Observation, error) {
	if len(inputs) == 0 {
		return nil, fmt.Errorf("observation batch must contain at least one item")
	}
	if len(inputs) > MaxObservationBatch {
		return nil, fmt.Errorf("observation batch exceeds maximum of %d items", MaxObservationBatch)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin observation batch: %w", err)
	}
	defer tx.Rollback(ctx)

	var status model.RunStatus
	var scopePolicy string
	if err := tx.QueryRow(ctx, `SELECT status, scope_policy_version FROM runs WHERE id=$1 FOR UPDATE`, runID).Scan(&status, &scopePolicy); err != nil {
		return nil, fmt.Errorf("verify run: %w", err)
	}
	if status.IsTerminal() || status == model.RunStatusStopping {
		return nil, fmt.Errorf("cannot record observation: run status is %s", status)
	}

	validated := make([]validatedObservation, len(inputs))
	for i, in := range inputs {
		if strings.TrimSpace(in.ObsType) == "" || strings.TrimSpace(in.Content) == "" || in.SourceToolCallID == uuid.Nil {
			return nil, fmt.Errorf("observation %d: obs_type, content and source_tool_call_id are required", i)
		}
		v, err := validateObservationSourceTx(ctx, tx, runID, sessionID, attemptID, in.SourceToolCallID, in.AssetID, scopePolicy)
		if err != nil {
			return nil, fmt.Errorf("observation %d: %w", i, err)
		}
		validated[i] = v
	}

	var seq int
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq),0) FROM run_events WHERE run_id=$1`, runID).Scan(&seq); err != nil {
		return nil, fmt.Errorf("get observation event sequence: %w", err)
	}
	result := make([]model.Observation, 0, len(inputs))
	for i, in := range inputs {
		data := in.Data
		if len(data) == 0 {
			data = json.RawMessage(`{}`)
		}
		var obs model.Observation
		if err := tx.QueryRow(ctx, `INSERT INTO observations (id,run_id,attempt_id,asset_id,obs_type,content,data,source_tool_call_id)
 VALUES (gen_random_uuid(),$1,$2,$3,$4,$5,$6,$7)
 RETURNING id,run_id,attempt_id,asset_id,obs_type,content,data,source_tool_call_id,created_at`,
			runID, attemptID, validated[i].assetID, in.ObsType, in.Content, data, in.SourceToolCallID).Scan(
			&obs.ID, &obs.RunID, &obs.AttemptID, &obs.AssetID, &obs.ObsType, &obs.Content, &obs.Data, &obs.SourceToolCallID, &obs.CreatedAt); err != nil {
			return nil, fmt.Errorf("insert observation %d: %w", i, err)
		}
		seq++
		payload, _ := json.Marshal(map[string]any{
			"observation_id": obs.ID, "obs_type": obs.ObsType, "content": obs.Content, "asset_id": obs.AssetID,
		})
		if _, err := tx.Exec(ctx, `INSERT INTO run_events(id,run_id,seq,type,payload,session_id,work_item_id,attempt_id)
 VALUES (gen_random_uuid(),$1,$2,'observation',$3,$4,$5,$6)`, runID, seq, payload, validated[i].sessionID, validated[i].workItem, attemptID); err != nil {
			return nil, fmt.Errorf("insert observation event %d: %w", i, err)
		}
		result = append(result, obs)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("commit observation batch: %w", err)
	}
	return result, nil
}

func validateObservationSourceTx(ctx context.Context, tx pgx.Tx, runID, sessionID, attemptID, sourceID uuid.UUID, assetID *uuid.UUID, scopePolicy string) (validatedObservation, error) {
	var v validatedObservation
	var attemptSession uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT a.session_id,a.work_item_id FROM attempts a JOIN work_items wi ON wi.id=a.work_item_id JOIN agent_sessions sess ON sess.id=a.session_id AND sess.run_id=wi.run_id WHERE a.id=$1 AND wi.run_id=$2`, attemptID, runID).Scan(&attemptSession, &v.workItem); err != nil {
		return v, fmt.Errorf("verify attempt: %w", ErrInvalidObservationSource)
	}
	if sessionID != uuid.Nil && attemptSession != sessionID {
		return v, fmt.Errorf("attempt session does not match worker session: %w", ErrInvalidObservationSource)
	}
	var tcStatus string
	var tcSession *uuid.UUID
	var toolName string
	var args json.RawMessage
	if err := tx.QueryRow(ctx, `SELECT status,session_id,tool_name,arguments FROM agent_tool_calls WHERE id=$1 AND run_id=$2`, sourceID, runID).Scan(&tcStatus, &tcSession, &toolName, &args); err != nil {
		return v, fmt.Errorf("verify source tool call: %w", ErrInvalidObservationSource)
	}
	if tcStatus != "success" || tcSession == nil || *tcSession != attemptSession {
		return v, ErrInvalidObservationSource
	}
	var resolved uuid.UUID
	var targetKey string
	switch toolName {
	case "read_sample":
		var a struct {
			SampleID string `json:"sample_id"`
		}
		if json.Unmarshal(args, &a) != nil || strings.TrimSpace(a.SampleID) == "" {
			return v, fmt.Errorf("read_sample arguments missing sample_id: %w", ErrInvalidObservationSource)
		}
		sampleID := strings.TrimSpace(a.SampleID)
		err := tx.QueryRow(ctx, `SELECT id FROM assets WHERE namespace='builtin-http-v1' AND kind='sample_endpoint' AND external_key=$1`, sampleID).Scan(&resolved)
		if err != nil {
			spec, ok := BuiltinAssetSpecs[sampleID]
			if !ok {
				return v, fmt.Errorf("unknown sample asset %q: %w", sampleID, ErrInvalidObservationSource)
			}
			meta, _ := json.Marshal(spec.Metadata)
			if err = tx.QueryRow(ctx, `INSERT INTO assets(id,namespace,kind,external_key,name,description,metadata,created_at,updated_at)
 VALUES(gen_random_uuid(),$1,$2,$3,$4,$5,$6,now(),now()) ON CONFLICT(namespace,kind,external_key) DO UPDATE SET updated_at=now() RETURNING id`, spec.Namespace, spec.Kind, spec.ExternalKey, spec.Name, spec.Description, meta).Scan(&resolved); err != nil {
				return v, fmt.Errorf("resolve sample asset: %w", err)
			}
		}
		targetKey = sampleID
	case "get_endpoint":
		var a struct {
			AssetID string `json:"asset_id"`
		}
		if json.Unmarshal(args, &a) != nil {
			return v, fmt.Errorf("invalid get_endpoint arguments: %w", ErrInvalidObservationSource)
		}
		parsed, err := uuid.Parse(strings.TrimSpace(a.AssetID))
		if err != nil || parsed == uuid.Nil {
			return v, fmt.Errorf("invalid endpoint asset_id: %w", ErrInvalidObservationSource)
		}
		if err = tx.QueryRow(ctx, `SELECT id FROM assets WHERE id=$1 AND kind='http_endpoint'`, parsed).Scan(&resolved); err != nil {
			return v, fmt.Errorf("unknown http asset: %w", ErrInvalidObservationSource)
		}
		targetKey = parsed.String()
	case "http_request":
		var epoch int
		if err := tx.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1`, runID).Scan(&epoch); err != nil {
			return v, err
		}
		if err := tx.QueryRow(ctx, `SELECT asset_id FROM http_receipts WHERE source_tool_call_id=$1 AND run_id=$2 AND epoch_number=$3 AND minted_by='gateway' AND asset_id IS NOT NULL`, sourceID, runID, epoch).Scan(&resolved); err != nil {
			return v, fmt.Errorf("HTTP observation requires an asset-bound capture: %w", ErrInvalidObservationSource)
		}
		if _, err := targetEvidence(ctx, tx, runID, resolved, sourceID, epoch, &attemptSession); err != nil {
			return v, err
		}
		targetKey = resolved.String()
	default:
		return v, fmt.Errorf("source tool call must be read_sample, get_endpoint or captured http_request, got %q: %w", toolName, ErrInvalidObservationSource)
	}
	if assetID != nil && *assetID != resolved {
		return v, fmt.Errorf("asset_id does not match source tool call: %w", ErrAssetSampleMismatch)
	}
	v.assetID = &resolved
	var inRun bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2)`, runID, resolved).Scan(&inRun); err != nil {
		return v, fmt.Errorf("check run asset scope: %w", err)
	}
	if !inRun && scopePolicy != "legacy" {
		return v, fmt.Errorf("asset %q is outside run scope: %w", targetKey, ErrAssetOutOfScope)
	}
	var inWork bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_item_assets WHERE work_item_id=$1 AND asset_id=$2)`, v.workItem, resolved).Scan(&inWork); err != nil {
		return v, fmt.Errorf("check work item scope: %w", err)
	}
	if !inWork && scopePolicy != "legacy" {
		return v, fmt.Errorf("asset %q is not associated with work item: %w", targetKey, ErrAssetOutOfScope)
	}
	v.sessionID = attemptSession
	return v, nil
}
