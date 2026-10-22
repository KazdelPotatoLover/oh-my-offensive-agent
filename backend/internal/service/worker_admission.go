package service

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Called while Continue/Retry holds the Run lock. Reject an impossible
// continuation before mutating the direction or creating another Attempt.
// Runtime admission remains authoritative for concurrent, in-flight requests.
func checkWorkerContinuationBudget(ctx context.Context, tx pgx.Tx, runID uuid.UUID) error {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT execution_input FROM runs WHERE id=$1`, runID).Scan(&raw); err != nil {
		return err
	}
	var in struct {
		Budget model.BudgetConfig `json:"budget"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return err
	}
	if in.Budget.MaxModelCalls <= 0 {
		return nil // legacy records without an explicit limit keep their meaning
	}
	var used, granted int
	if err := tx.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM model_calls m LEFT JOIN agent_sessions s ON s.id=m.session_id WHERE m.run_id=$1 AND COALESCE(s.role,'')!='verifier'),
		(SELECT COALESCE(sum(additional_model_calls),0) FROM budget_grants WHERE run_id=$1)`, runID).Scan(&used, &granted); err != nil {
		return err
	}
	total := in.Budget.MaxModelCalls + granted
	keep := in.Budget.PlannerReservedModelCalls + in.Budget.MainReservedModelCalls
	if total > 1 {
		keep = max(keep, 1)
	}
	if total-used <= keep {
		return &contract.Error{Code: contract.CodeRunBudgetExhausted, Scope: contract.ScopeRun,
			Message:         fmt.Sprintf("worker model allowance exhausted: used=%d, authorized=%d, reserved=%d. No continuation was queued. Preserve results; explicit user budget authorization is required before continuing", used, total, keep),
			SuggestedAction: contract.ActionGrantBudget}
	}
	return nil
}

// WorkItemAssetBindings supplies server-owned UUID/URL pairs to the Worker.
// It lists only this direction's assigned assets, using immutable Run snapshots.
func (s *RunService) WorkItemAssetBindings(ctx context.Context, runID, workItemID uuid.UUID) (json.RawMessage, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object(
		'asset_id',a.id,'kind',a.kind,'external_key',a.external_key,
		'url',ra.snapshot->'metadata'->>'url') ORDER BY a.id),'[]'::jsonb)
		FROM work_item_assets wa JOIN assets a ON a.id=wa.asset_id
		JOIN run_assets ra ON ra.asset_id=a.id AND ra.run_id=$1
		WHERE wa.work_item_id=$2 AND EXISTS(SELECT 1 FROM work_items w WHERE w.id=$2 AND w.run_id=$1)`, runID, workItemID).Scan(&raw)
	return json.RawMessage(raw), err
}
