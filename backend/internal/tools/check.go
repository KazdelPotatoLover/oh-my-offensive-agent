package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/checker"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
)

// RunSecurityCheckTool executes registered deterministic checks against persisted HTTP evidence.
type RunSecurityCheckTool struct {
	runID      uuid.UUID
	workItemID uuid.UUID
	attemptID  uuid.UUID
	sessionID  uuid.UUID
	runService *service.RunService
}

func NewRunSecurityCheckTool(runID, workItemID, attemptID, sessionID uuid.UUID, runService *service.RunService) *RunSecurityCheckTool {
	return &RunSecurityCheckTool{
		runID:      runID,
		workItemID: workItemID,
		attemptID:  attemptID,
		sessionID:  sessionID,
		runService: runService,
	}
}

func (t *RunSecurityCheckTool) Name() string {
	return "run_security_check"
}

func (t *RunSecurityCheckTool) Description() string {
	return "Execute registered deterministic security checks against persisted HTTP evidence for an asset. Reads previously stored get_endpoint results."
}

func (t *RunSecurityCheckTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"asset_id": {
				"type": "string",
				"description": "The UUID of the target HTTP endpoint asset"
			},
			"check_id": {
				"type": "string",
				"description": "Identifier of the registered check rule or suite (e.g. http_security_headers, http_x_content_type_options, http_content_security_policy, http_strict_transport_security)"
			}
		},
		"required": ["asset_id", "check_id"]
	}`)
}

func (t *RunSecurityCheckTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		AssetID string `json:"asset_id"`
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return fmt.Errorf("invalid arguments JSON: %w", err)
	}
	if strings.TrimSpace(payload.AssetID) == "" {
		return errors.New("asset_id is required")
	}
	if _, err := uuid.Parse(strings.TrimSpace(payload.AssetID)); err != nil {
		return fmt.Errorf("invalid asset_id: must be a valid UUID: %w", err)
	}
	if strings.TrimSpace(payload.CheckID) == "" {
		return errors.New("check_id is required")
	}
	if !checker.IsRegisteredCheck(payload.CheckID) {
		return fmt.Errorf("unknown check_id %q: must be one of registered checks in catalog", payload.CheckID)
	}
	return nil
}

func (t *RunSecurityCheckTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		AssetID string `json:"asset_id"`
		CheckID string `json:"check_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", fmt.Errorf("invalid arguments JSON: %w", err)
	}

	assetID, err := uuid.Parse(strings.TrimSpace(payload.AssetID))
	if err != nil {
		return "", fmt.Errorf("invalid asset_id: %w", err)
	}
	checkID := strings.TrimSpace(payload.CheckID)

	pool := t.runService.Pool()

	// 1. Verify asset belongs to Run scope
	var inRunScope bool
	err = pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_assets WHERE run_id = $1 AND asset_id = $2)`, t.runID, assetID).Scan(&inRunScope)
	if err != nil {
		return "", fmt.Errorf("query run asset scope: %w", err)
	}
	if !inRunScope {
		return "", fmt.Errorf("asset %s is outside run scope: %w", assetID, service.ErrAssetOutOfScope)
	}

	// 2. Verify asset belongs to WorkItem scope if workItemID is set
	if t.workItemID != uuid.Nil {
		var plannedAsset, plannedCheck string
		err = pool.QueryRow(ctx, `SELECT wi.input_data->>'asset_id',wi.input_data->>'check_id' FROM work_item_assets wa JOIN work_items wi ON wi.id=wa.work_item_id WHERE wi.id=$1 AND wa.asset_id=$2 AND wi.run_id=$3`, t.workItemID, assetID, t.runID).Scan(&plannedAsset, &plannedCheck)
		if err != nil {
			return "", fmt.Errorf("query work item asset scope: %w", err)
		}
		if !strings.EqualFold(plannedAsset, assetID.String()) || plannedCheck != checkID {
			return "", fmt.Errorf("asset %s is not associated with work item: %w", assetID, service.ErrAssetOutOfScope)
		}
	}

	// 3. Retrieve latest persisted get_endpoint evidence for this asset in this Run
	latestEvidence, err := t.runService.GetLatestHTTPEvidence(ctx, t.runID, assetID)
	if err != nil {
		return "", fmt.Errorf("retrieve latest http evidence: %w", err)
	}

	var evidenceBytes []byte
	var sourceToolCallID *uuid.UUID
	if latestEvidence != nil {
		evidenceBytes = []byte(latestEvidence.Output)
		sourceToolCallID = &latestEvidence.ID
	}

	// 4. Run deterministic check logic
	ruleResults := checker.EvaluateHTTPResponseChecks(checkID, evidenceBytes)

	// 5. Convert to CheckResultInput
	inputs := make([]service.CheckResultInput, len(ruleResults))
	for i, r := range ruleResults {
		inputs[i] = service.CheckResultInput{
			AssetID:            assetID,
			CheckID:            r.CheckID,
			CheckName:          r.CheckName,
			CheckVersion:       r.CheckVersion,
			Status:             r.Status,
			Explanation:        r.Explanation,
			SourceToolCallID:   sourceToolCallID,
			EvidenceSnapshot:   r.EvidenceSnapshot,
			Details:            r.Details,
			GenerateFinding:    r.GenerateFinding,
			FindingTitle:       r.FindingTitle,
			FindingCategory:    r.FindingCategory,
			FindingSeverity:    r.FindingSeverity,
			FindingDescription: r.FindingDescription,
			FindingRemediation: r.FindingRemediation,
		}
	}

	// 6. Record in database with lock & terminal state protection
	var attIDPtr, sessIDPtr, wiIDPtr *uuid.UUID
	if t.attemptID != uuid.Nil {
		attIDPtr = &t.attemptID
	}
	if t.sessionID != uuid.Nil {
		sessIDPtr = &t.sessionID
	}
	if t.workItemID != uuid.Nil {
		wiIDPtr = &t.workItemID
	}

	recOut, err := t.runService.RecordCheckResults(ctx, t.runID, attIDPtr, sessIDPtr, wiIDPtr, inputs)
	if err != nil {
		return "", fmt.Errorf("record check results: %w", err)
	}

	// 7. Format structured response
	type crSummary struct {
		ID          uuid.UUID         `json:"id"`
		CheckID     string            `json:"check_id"`
		CheckName   string            `json:"check_name"`
		Status      model.CheckStatus `json:"status"`
		Explanation string            `json:"explanation"`
	}
	summaries := make([]crSummary, len(recOut.CheckResults))
	for i, cr := range recOut.CheckResults {
		summaries[i] = crSummary{
			ID:          cr.ID,
			CheckID:     cr.CheckID,
			CheckName:   cr.CheckName,
			Status:      cr.Status,
			Explanation: cr.Explanation,
		}
	}

	outMap := map[string]interface{}{
		"status":             "recorded",
		"asset_id":           assetID.String(),
		"recorded_count":     recOut.RecordedCount,
		"deduplicated_count": recOut.DeduplicatedCount,
		"check_results":      summaries,
		"findings_count":     len(recOut.Findings),
	}
	outJSON, _ := json.Marshal(outMap)
	return string(outJSON), nil
}
