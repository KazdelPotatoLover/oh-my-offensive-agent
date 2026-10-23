package netcap

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"strings"

	"agent-workbench/backend/internal/contract"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BindCapturedEndpoint gives a completed URL-only request its actual endpoint
// identity. It records an already authorized exchange, never a new scope grant
// or a new request. Only the current pending worker call may bind its receipt.
func (s *Store) BindCapturedEndpoint(ctx context.Context, scope contract.ExecutionScope, receipt *contract.HTTPReceipt) error {
	if receipt == nil || receipt.AssetID != nil || scope.WorkItemID == nil || scope.SessionID == nil || scope.AttemptID == nil {
		return nil
	}
	var boundID uuid.UUID
	err := s.withTx(ctx, func(tx pgx.Tx) error {
		var epoch int
		var status string
		var raw []byte
		if err := tx.QueryRow(ctx, `SELECT current_epoch,status,execution_input FROM runs WHERE id=$1 FOR UPDATE`, scope.RunID).Scan(&epoch, &status, &raw); err != nil {
			return err
		}
		if epoch != scope.EpochNumber {
			return contractError(contract.CodeStaleEpoch, "capture belongs to an older epoch")
		}
		if status != "running" && status != "paused" {
			return contractError(contract.CodeScopeDenied, "run no longer accepts endpoint registration")
		}
		var rawURL, method string
		var callID uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT h.url,h.method,tc.id FROM http_receipts h
 JOIN agent_tool_calls tc ON tc.id=h.source_tool_call_id AND tc.run_id=h.run_id AND tc.epoch_number=h.epoch_number
 JOIN agent_sessions s ON s.id=tc.session_id AND s.run_id=h.run_id AND s.work_item_id=h.work_item_id
 JOIN attempts a ON a.id=h.attempt_id AND a.work_item_id=h.work_item_id AND a.session_id=s.id
 JOIN work_items w ON w.id=a.work_item_id AND w.run_id=h.run_id AND w.current_attempt_id=a.id
 WHERE h.id=$1 AND h.run_id=$2 AND h.epoch_number=$3 AND h.work_item_id=$4 AND h.session_id=$5 AND h.attempt_id=$6
 AND h.minted_by='gateway' AND h.outcome='completed' AND h.capture_mode IN ('direct','tls_intercept')
 AND tc.tool_name='http_request' AND tc.status='pending' AND s.role='worker'
 AND s.epoch_number=h.epoch_number AND s.status='running' AND a.status='running' AND w.status='running'`,
			receipt.ReceiptID, scope.RunID, epoch, scope.WorkItemID, scope.SessionID, scope.AttemptID).Scan(&rawURL, &method, &callID); err != nil {
			return err
		}
		policy, err := PolicyFromExecutionInput(raw)
		if err != nil {
			return err
		}
		u, err := url.Parse(rawURL)
		if err != nil {
			return err
		}
		if _, err := policy.Match(u); err != nil {
			return err
		}
		if err := policy.AllowsMethod(method); err != nil {
			return err
		}
		// Query values are tests of the endpoint, not new endpoint identities.
		u.RawQuery, u.Fragment, u.RawFragment, u.ForceQuery = "", "", "", false
		if u.Path == "" {
			u.Path = "/"
		}
		baseURL := u.String()
		var assetID uuid.UUID
		err = tx.QueryRow(ctx, `SELECT ra.asset_id FROM run_assets ra JOIN assets a ON a.id=ra.asset_id
 WHERE ra.run_id=$1 AND a.kind='http_endpoint'
 AND split_part(COALESCE(ra.snapshot->'metadata'->>'url',a.metadata->>'url'),'?',1)=$2
 ORDER BY ra.asset_id LIMIT 1`, scope.RunID, baseURL).Scan(&assetID)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if errors.Is(err, pgx.ErrNoRows) {
			// The gateway already admitted this page under the per-origin limit.
			// Binding its persisted receipt consumes no further network allowance.
			namespace := "target-range"
			if err := tx.QueryRow(ctx, `SELECT a.namespace FROM run_assets ra JOIN assets a ON a.id=ra.asset_id WHERE ra.run_id=$1 AND a.kind='http_endpoint' ORDER BY ra.created_at,a.id LIMIT 1`, scope.RunID).Scan(&namespace); err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			key := strings.ToUpper(method) + " " + baseURL
			metadata, _ := json.Marshal(map[string]any{"url": baseURL, "path": u.Path, "port": u.Port(), "method": method})
			if err := tx.QueryRow(ctx, `INSERT INTO assets(namespace,kind,external_key,name,description,metadata)
 VALUES($1,'http_endpoint',$2,$2,'Endpoint observed in an authorized HTTP capture',$3)
 ON CONFLICT(namespace,kind,external_key) DO UPDATE SET updated_at=now() RETURNING id`, namespace, key, metadata).Scan(&assetID); err != nil {
				return err
			}
			snapshot, _ := json.Marshal(map[string]any{"namespace": namespace, "kind": "http_endpoint", "external_key": key, "metadata": json.RawMessage(metadata), "origin": "http_capture", "source_tool_call_id": callID})
			if _, err := tx.Exec(ctx, `INSERT INTO run_assets(run_id,asset_id,snapshot) VALUES($1,$2,$3) ON CONFLICT(run_id,asset_id) DO NOTHING`, scope.RunID, assetID, snapshot); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `INSERT INTO work_item_assets(work_item_id,asset_id) VALUES($1,$2) ON CONFLICT(work_item_id,asset_id) DO NOTHING`, scope.WorkItemID, assetID); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `UPDATE http_receipts SET asset_id=$2 WHERE id=$1 AND asset_id IS NULL`, receipt.ReceiptID, assetID); err != nil {
			return err
		}
		boundID = assetID
		return nil
	})
	if err == nil {
		receipt.AssetID = &boundID
	}
	return err
}
