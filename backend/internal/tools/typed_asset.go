package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

type TypedAssetTool struct{ scope AdapterContext }

func (t *TypedAssetTool) Name() string { return "register_asset" }
func (t *TypedAssetTool) Description() string {
	return "Register the canonical app/host/IP/service identity of an already authorized endpoint; this does not probe hosts or expand execution scope."
}
func (t *TypedAssetTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"parent_asset_id":{"type":"string"},"kind":{"type":"string","enum":["app","host","ip","service"]},"identity":{"type":"string"}},"required":["parent_asset_id","kind"]}`)
}
func (t *TypedAssetTool) Validate(args []byte) error {
	var in map[string]any
	return json.Unmarshal(args, &in)
}
func (t *TypedAssetTool) Execute(ctx context.Context, args []byte) (string, error) {
	var in struct {
		ParentID       uuid.UUID `json:"parent_asset_id"`
		Kind, Identity string
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", err
	}
	if t.scope.WorkItemID != uuid.Nil {
		var allowed bool
		if err := t.scope.Service.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM work_item_assets wa JOIN work_items w ON w.id=wa.work_item_id WHERE wa.work_item_id=$1 AND wa.asset_id=$2 AND w.run_id=$3)`, t.scope.WorkItemID, in.ParentID, t.scope.RunID).Scan(&allowed); err != nil {
			return "", err
		}
		if !allowed {
			return "", fmt.Errorf("parent is outside work item scope: %w", service.ErrAssetOutOfScope)
		}
	}
	out, err := t.scope.Service.DeriveTypedAsset(ctx, t.scope.RunID, in.ParentID, in.Kind, in.Identity)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(out)
	return string(b), err
}
