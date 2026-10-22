package service

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/netcap"
	"github.com/google/uuid"
)

// HTTPDiscoverySource resolves a session-local call ID (or stable call UUID).
// New HTTP sources always use gateway evidence; legacy GET envelopes remain readable.
func (s *RunService) HTTPDiscoverySource(ctx context.Context, runID, sessionID, workID uuid.UUID, ref string) (*netcap.ToolExchange, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	var id uuid.UUID
	var name, output string
	var epoch, current int
	var owner *uuid.UUID
	err = tx.QueryRow(ctx, `SELECT tc.id,tc.tool_name,COALESCE(tc.output,''),tc.epoch_number,r.current_epoch,s.work_item_id
 FROM agent_tool_calls tc JOIN agent_sessions s ON s.id=tc.session_id AND s.run_id=tc.run_id JOIN runs r ON r.id=tc.run_id
 WHERE tc.run_id=$1 AND tc.session_id=$2 AND (tc.id::text=$3 OR tc.call_id=$3) AND tc.status='success'`, runID, sessionID, strings.TrimSpace(ref)).Scan(&id, &name, &output, &epoch, &current, &owner)
	if err != nil {
		return nil, fmt.Errorf("%w: source unavailable in this session", ErrInvalidEvidence)
	}
	if epoch != current {
		return nil, ErrStaleEpoch
	}
	if workID != uuid.Nil && (owner == nil || *owner != workID) {
		return nil, ErrAssetOutOfScope
	}
	if name == "http_request" {
		return netcap.ReadToolExchange(ctx, tx, runID, id, current, true)
	}
	if name != "get_endpoint" {
		return nil, ErrInvalidEvidence
	}
	var in struct {
		Request struct {
			AssetID uuid.UUID `json:"asset_id"`
			URL     string    `json:"url"`
		} `json:"request"`
	}
	if json.Unmarshal([]byte(output), &in) != nil || in.Request.AssetID == uuid.Nil {
		return nil, ErrInvalidEvidence
	}
	response, err := targetEvidence(ctx, tx, runID, in.Request.AssetID, id, current, &sessionID)
	if err != nil {
		return nil, err
	}
	var assigned bool
	err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM run_assets WHERE run_id=$1 AND asset_id=$2) AND ($3::uuid IS NULL OR EXISTS(SELECT 1 FROM work_item_assets WHERE work_item_id=$3 AND asset_id=$2))`, runID, in.Request.AssetID, owner).Scan(&assigned)
	if err != nil {
		return nil, err
	}
	if !assigned {
		return nil, ErrAssetOutOfScope
	}
	return &netcap.ToolExchange{CallID: id, AssetID: in.Request.AssetID, Epoch: epoch, URL: in.Request.URL, FinalURL: in.Request.URL, Method: "GET", Status: response.StatusCode, Headers: response.Headers, Body: response.Body}, nil
}
