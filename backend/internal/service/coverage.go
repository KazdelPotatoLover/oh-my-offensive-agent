package service

import (
	"agent-workbench/backend/internal/netcap"
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// AssetCoverage describes persisted execution, independently of observations or
// work completion. Repeated reads count as one visited asset. A successful HTTP
// tool result (including an HTTP error response) is a visit, not a security audit.
type AssetCoverage struct {
	AssetID     uuid.UUID
	ExternalKey string
	Planned     bool
	Visited     bool
}

func (s *RunService) ListAssetCoverage(ctx context.Context, runID uuid.UUID) ([]AssetCoverage, error) {
	rows, err := s.pool.Query(ctx, `SELECT a.id,a.external_key,
 EXISTS(SELECT 1 FROM work_item_assets wa JOIN work_items wi ON wi.id=wa.work_item_id
        WHERE wi.run_id=ra.run_id AND wa.asset_id=a.id),
 EXISTS(SELECT 1 FROM agent_tool_calls tc WHERE tc.run_id=ra.run_id AND tc.status='success' AND
        ((tc.tool_name='get_endpoint' AND a.kind='http_endpoint' AND lower(btrim(tc.arguments->>'asset_id'))=a.id::text)
         OR (tc.tool_name='read_sample' AND a.namespace='builtin-http-v1' AND a.kind='sample_endpoint'
             AND tc.arguments->>'sample_id'=a.external_key)))
 FROM run_assets ra JOIN assets a ON a.id=ra.asset_id WHERE ra.run_id=$1
 ORDER BY a.external_key`, runID)
	if err != nil {
		return nil, fmt.Errorf("list asset coverage: %w", err)
	}
	defer rows.Close()
	result := make([]AssetCoverage, 0)
	for rows.Next() {
		var a AssetCoverage
		if err := rows.Scan(&a.AssetID, &a.ExternalKey, &a.Planned, &a.Visited); err != nil {
			return nil, fmt.Errorf("scan asset coverage: %w", err)
		}
		result = append(result, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	calls, err := s.pool.Query(ctx, `SELECT tc.id,tc.epoch_number FROM agent_tool_calls tc JOIN runs r ON r.id=tc.run_id WHERE tc.run_id=$1 AND tc.epoch_number BETWEEN 1 AND r.current_epoch AND tc.status='success' AND tc.tool_name='http_request' ORDER BY tc.created_at`, runID)
	if err != nil {
		return nil, err
	}
	type ref struct {
		id    uuid.UUID
		epoch int
	}
	var refs []ref
	for calls.Next() {
		var r ref
		if err := calls.Scan(&r.id, &r.epoch); err != nil {
			calls.Close()
			return nil, err
		}
		refs = append(refs, r)
	}
	err = calls.Err()
	calls.Close()
	if err != nil {
		return nil, err
	}
	visited := map[uuid.UUID]bool{}
	for _, r := range refs {
		e, err := netcap.ReadToolExchange(ctx, s.pool, runID, r.id, r.epoch, false)
		if errors.Is(err, netcap.ErrNotEmpirical) {
			continue
		}
		if err != nil {
			return nil, err
		}
		visited[e.AssetID] = true
	}
	for i := range result {
		result[i].Visited = result[i].Visited || visited[result[i].AssetID]
	}
	return result, nil
}
