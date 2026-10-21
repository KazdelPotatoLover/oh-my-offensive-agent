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

// SubmitPlanTool allows the Planner to submit planned work items and dependencies.
type SubmitPlanTool struct {
	runID      uuid.UUID
	roundID    *uuid.UUID
	runService *service.RunService
	onPlan     func(items []model.WorkItem)
}

func NewSubmitPlanTool(runID uuid.UUID, runService *service.RunService, onPlan func([]model.WorkItem)) *SubmitPlanTool {
	return &SubmitPlanTool{
		runID:      runID,
		runService: runService,
		onPlan:     onPlan,
	}
}

func NewSubmitPlanToolWithRound(runID uuid.UUID, roundID *uuid.UUID, runService *service.RunService, onPlan func([]model.WorkItem)) *SubmitPlanTool {
	return &SubmitPlanTool{
		runID:      runID,
		roundID:    roundID,
		runService: runService,
		onPlan:     onPlan,
	}
}

func (t *SubmitPlanTool) Name() string {
	return "submit_plan"
}

func (t *SubmitPlanTool) Description() string {
	return "Submit work directions. Use intent with an authorized input_data.profile, a concrete objective, scoped asset_ids and source_fact_ids for open exploration; the Worker chooses available tools. list_capability_profiles lists authorized capabilities. Legacy inspect_http_endpoint/analyze_sample/run_security_check adapters remain available. Never invent tools or expand scope."
}

func (t *SubmitPlanTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {"type": "string", "description": "Brief summary of the plan"},
			"items": {
				"type": "array",
				"description": "List of work items to be scheduled and executed",
				"items": {
					"type": "object",
					"properties": {
						"key": {"type": "string", "description": "Unique stable identifier for the work item (e.g. analyze_home)"},
						"title": {"type": "string", "description": "Human-readable title"},
						"objective": {"type": "string", "description": "Detailed goal of this work item"},
						"work_type": {"type": "string", "enum": ["inspect_http_endpoint", "analyze_sample", "inspect_sample_endpoint", "aggregate_summary", "run_security_check", "intent"], "description": "Exact supported executor type. HTTP assets require inspect_http_endpoint; security configuration reviews require run_security_check; open exploration directions use intent."},
						"input_data": {"type": "object", "description": "inspect_http_endpoint: {asset_id: UUID from asset_ids}; run_security_check: {asset_id: UUID from asset_ids, check_id: string}; analyze_sample: {sample_id: scoped sample key}; aggregate_summary: {}. Dependencies control order only; they do not inject worklist artifacts.", "properties": {"asset_id": {"type": "string"}, "sample_id": {"type": "string"}, "check_id": {"type": "string"}}},
						"dependencies": {
							"type": "array",
							"items": {"type": "string"},
							"description": "Keys of prerequisite work items that must be completed before this item can start"
						},
						"asset_ids": {
							"type": "array",
							"items": {"type": "string"},
							"description": "UUIDs of target assets in scope for this work item"
						},
						"source_fact_ids": {"type":"array","items":{"type":"string"},"description":"Fact node IDs in this Run that jointly motivated this direction"},
						"source_observation_ids": {
							"type": "array",
							"items": {"type": "string"},
							"description": "UUIDs of prior observations that triggered this work item"
						}
					},
					"required": ["key", "title", "objective", "work_type"]
				}
			}
		},
		"required": ["items"]
	}`)
}

func (t *SubmitPlanTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		Items []service.PlanItemSpec `json:"items"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return fmt.Errorf("invalid json payload: %w", err)
	}
	if len(payload.Items) == 0 {
		return errors.New("plan must contain at least one item")
	}
	for _, item := range payload.Items {
		if item.Key == "" || item.Title == "" || item.Objective == "" {
			return errors.New("each item must specify non-empty key, title, and objective")
		}
		if !model.IsSupportedWorkType(item.WorkType) {
			return fmt.Errorf("%w: %q", service.ErrUnsupportedWorkType, item.WorkType)
		}
		if item.WorkType == "run_security_check" {
			checkID, _ := item.InputData["check_id"].(string)
			checkID = strings.TrimSpace(checkID)
			if checkID == "" {
				return errors.New("run_security_check requires non-empty input_data.check_id")
			}
			if !checker.IsRegisteredCheck(checkID) {
				return fmt.Errorf("unknown check_id %q: planner can only schedule registered checks in catalog", checkID)
			}
			assetIDStr, _ := item.InputData["asset_id"].(string)
			if strings.TrimSpace(assetIDStr) == "" {
				return errors.New("run_security_check requires non-empty input_data.asset_id")
			}
			if _, err := uuid.Parse(strings.TrimSpace(assetIDStr)); err != nil {
				return fmt.Errorf("run_security_check input_data.asset_id must be a valid UUID: %w", err)
			}
		}
	}
	return nil
}

func (t *SubmitPlanTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Summary string                 `json:"summary"`
		Items   []service.PlanItemSpec `json:"items"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	var created []model.WorkItem
	var err error
	if t.roundID != nil {
		created, err = t.runService.SubmitPlanWithRound(ctx, t.runID, t.roundID, payload.Items)
	} else {
		created, err = t.runService.SubmitPlan(ctx, t.runID, payload.Items)
	}
	if err != nil {
		return "", err
	}

	if t.onPlan != nil {
		t.onPlan(created)
	}

	res := map[string]interface{}{
		"status":        "success",
		"created_count": len(created),
		"message":       fmt.Sprintf("Plan submitted successfully with %d work items", len(created)),
	}
	b, _ := json.Marshal(res)
	return string(b), nil
}

// SuggestCompletionTool allows the Planner to suggest completing the Run once all work items are done.
type SuggestCompletionTool struct {
	runID        uuid.UUID
	runService   *service.RunService
	onCompletion func(summary, conclusion string)
}

func NewSuggestCompletionTool(runID uuid.UUID, runService *service.RunService, onCompletion func(summary, conclusion string)) *SuggestCompletionTool {
	return &SuggestCompletionTool{
		runID:        runID,
		runService:   runService,
		onCompletion: onCompletion,
	}
}

func (t *SuggestCompletionTool) Name() string {
	return "suggest_completion"
}

func (t *SuggestCompletionTool) Description() string {
	return "Suggest completing the run when all work items are finished, providing the final findings summary."
}

func (t *SuggestCompletionTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"final_summary": {"type": "string", "description": "Comprehensive final findings and summary report"},
			"conclusion": {
				"type": "string",
				"enum": ["complete", "partial", "blocked"],
				"description": "Business conclusion of the run"
			}
		},
		"required": ["final_summary"]
	}`)
}

func (t *SuggestCompletionTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		FinalSummary string `json:"final_summary"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if payload.FinalSummary == "" {
		return errors.New("final_summary cannot be empty")
	}
	return nil
}

func (t *SuggestCompletionTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		FinalSummary string `json:"final_summary"`
		Conclusion   string `json:"conclusion"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	if payload.Conclusion == "" || payload.Conclusion == "ok" || payload.Conclusion == "success" {
		payload.Conclusion = "complete"
	}
	if payload.Conclusion != "complete" && payload.Conclusion != "partial" && payload.Conclusion != "blocked" && payload.Conclusion != "needs_input" {
		return "", fmt.Errorf("unsupported completion conclusion")
	}

	// Verify that there are no active/pending items
	items, err := t.runService.ListWorkItemsByRun(ctx, t.runID)
	if err != nil {
		return "", err
	}
	for _, it := range items {
		if it.Status == model.WorkItemStatusPending || it.Status == model.WorkItemStatusRunning {
			return "", fmt.Errorf("cannot complete run: work item '%s' is still in status %s", it.Key, it.Status)
		}
	}

	// If goals exist in exploration graph, verify all goals are met before allowing 'complete'
	if payload.Conclusion == "complete" {
		goals, err := t.runService.ListGoals(ctx, t.runID)
		if err != nil {
			return "", fmt.Errorf("list goals for completion check: %w", err)
		}
		if len(goals) > 0 {
			var unmetGoals []string
			for _, g := range goals {
				if g.State != "met" {
					unmetGoals = append(unmetGoals, g.ID.String())
				}
			}
			if len(unmetGoals) > 0 {
				return "", fmt.Errorf("cannot conclude 'complete': %d of %d goals are not met (%s). Goals must be proved using prove_goal before marking complete (or conclude 'partial')", len(unmetGoals), len(goals), strings.Join(unmetGoals, ", "))
			}
		} else {
			// In exploration mode (or if exploration nodes exist), completing without any goals is guarded
			var hasExpNodes bool
			if t.runService.Pool() != nil {
				_ = t.runService.Pool().QueryRow(ctx,
					`SELECT EXISTS (SELECT 1 FROM exploration_nodes WHERE run_id = $1)`, t.runID).Scan(&hasExpNodes)
			}
			if hasExpNodes {
				return "", fmt.Errorf("cannot conclude 'complete' in exploration mode without defining and proving goals; set goals with set_goals or conclude 'partial'")
			}
		}
	}

	if payload.Conclusion == "complete" {
		cause, err := t.runService.CompletionCause(ctx, t.runID, "complete")
		if err != nil {
			return "", err
		}
		if cause != model.CauseCompleted {
			return "", fmt.Errorf("cannot complete: goals are not met by current valid proofs or work remains; use partial/blocked when appropriate")
		}
	}

	if t.onCompletion != nil {
		t.onCompletion(payload.FinalSummary, payload.Conclusion)
	}

	return fmt.Sprintf(`{"status":"accepted","conclusion":"%s"}`, payload.Conclusion), nil
}

// RecordObservationTool allows workers to record verified observations backed by tool calls.
type RecordObservationTool struct {
	runID      uuid.UUID
	attemptID  uuid.UUID
	sessionID  uuid.UUID
	runService *service.RunService
}

func NewRecordObservationTool(runID, attemptID uuid.UUID, runService *service.RunService) *RecordObservationTool {
	return &RecordObservationTool{
		runID:      runID,
		attemptID:  attemptID,
		runService: runService,
	}
}

func NewRecordObservationToolWithSession(runID, attemptID, sessionID uuid.UUID, runService *service.RunService) *RecordObservationTool {
	return &RecordObservationTool{
		runID:      runID,
		attemptID:  attemptID,
		sessionID:  sessionID,
		runService: runService,
	}
}

func (t *RecordObservationTool) Name() string {
	return "record_observation"
}

func (t *RecordObservationTool) Description() string {
	return "Record one structured observation derived from a successful tool execution. For one explored page, combine HTTP status, title, response headers/features, and content summary into one page_observation with structured data; use record_observation_batch only for genuinely different conclusions or evidence sources."
}

// RecordObservationBatchTool writes several independently evidenced
// observations in one atomic transaction. It is deliberately bounded and does
// not weaken the per-row source/session/attempt/asset checks.
type RecordObservationBatchTool struct {
	runID, attemptID, sessionID uuid.UUID
	runService                  *service.RunService
}

func NewRecordObservationBatchTool(runID, attemptID, sessionID uuid.UUID, runService *service.RunService) *RecordObservationBatchTool {
	return &RecordObservationBatchTool{runID: runID, attemptID: attemptID, sessionID: sessionID, runService: runService}
}

func (t *RecordObservationBatchTool) Name() string { return "record_observation_batch" }
func (t *RecordObservationBatchTool) Description() string {
	return fmt.Sprintf("Atomically record 1-%d observations. Prefer one page_observation per explored page, combining status/title/response features/content summary. Use multiple entries only for different conclusions or source tool calls. Every entry must cite its own successful source_tool_call_id; any invalid entry rejects the entire batch. Returns {status:recorded,count,observation_ids} with IDs in input order; no partial success.", service.MaxObservationBatch)
}
func (t *RecordObservationBatchTool) Parameters() json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","properties":{"observations":{"type":"array","minItems":1,"maxItems":%d,"items":{"type":"object","properties":{"obs_type":{"type":"string"},"content":{"type":"string"},"data":{"type":"object","description":"For page_observation: status_code, title, response_features and content_summary from this source"},"source_tool_call_id":{"type":"string","description":"_execution.source_tool_call_id or the exact original call_id"},"asset_id":{"type":"string"}},"required":["obs_type","content","source_tool_call_id"]}}},"required":["observations"]}`, service.MaxObservationBatch))
}
func (t *RecordObservationBatchTool) Validate(in []byte) error {
	var p struct {
		Observations []struct {
			ObsType string `json:"obs_type"`
			Content string `json:"content"`
			Source  string `json:"source_tool_call_id"`
			Asset   string `json:"asset_id"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(in, &p); err != nil {
		return err
	}
	if len(p.Observations) == 0 || len(p.Observations) > service.MaxObservationBatch {
		return fmt.Errorf("observations must contain 1-%d items", service.MaxObservationBatch)
	}
	for i, o := range p.Observations {
		if strings.TrimSpace(o.ObsType) == "" || strings.TrimSpace(o.Content) == "" || strings.TrimSpace(o.Source) == "" {
			return fmt.Errorf("observation %d requires obs_type, content and source_tool_call_id", i)
		}
		if o.Asset != "" {
			if _, err := uuid.Parse(strings.TrimSpace(o.Asset)); err != nil {
				return fmt.Errorf("observation %d invalid asset_id: %w", i, err)
			}
		}
	}
	return nil
}
func (t *RecordObservationBatchTool) Execute(ctx context.Context, in []byte) (string, error) {
	var p struct {
		Observations []struct {
			ObsType string         `json:"obs_type"`
			Content string         `json:"content"`
			Data    map[string]any `json:"data"`
			Source  string         `json:"source_tool_call_id"`
			Asset   string         `json:"asset_id"`
		} `json:"observations"`
	}
	if err := json.Unmarshal(in, &p); err != nil {
		return "", err
	}
	inputs := make([]service.ObservationInput, 0, len(p.Observations))
	for i, o := range p.Observations {
		var source uuid.UUID
		if parsed, err := uuid.Parse(strings.TrimSpace(o.Source)); err == nil {
			source = parsed
		} else {
			tc, lookupErr := t.runService.GetAgentToolCallBySessionAndCallID(ctx, t.sessionID, strings.TrimSpace(o.Source))
			if lookupErr != nil {
				return "", fmt.Errorf("observation %d source: %w", i, service.ErrInvalidObservationSource)
			}
			source = tc.ID
		}
		var asset *uuid.UUID
		if strings.TrimSpace(o.Asset) != "" {
			parsed, _ := uuid.Parse(strings.TrimSpace(o.Asset))
			asset = &parsed
		}
		data := json.RawMessage(`{}`)
		if o.Data != nil {
			data, _ = json.Marshal(o.Data)
		}
		inputs = append(inputs, service.ObservationInput{ObsType: o.ObsType, Content: o.Content, Data: data, SourceToolCallID: source, AssetID: asset})
	}
	obs, err := t.runService.CreateObservationsBatchWithSession(ctx, t.runID, t.sessionID, t.attemptID, inputs)
	if err != nil {
		return "", err
	}
	ids := make([]string, 0, len(obs))
	for _, o := range obs {
		ids = append(ids, o.ID.String())
	}
	out, _ := json.Marshal(map[string]any{"status": "recorded", "count": len(ids), "observation_ids": ids})
	return string(out), nil
}

func (t *RecordObservationTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"obs_type": {"type": "string", "description": "Use page_observation for a page exploration; legacy observation types are still accepted"},
			"content": {"type": "string", "description": "Text description of the observation"},
			"data": {"type": "object", "description": "For page_observation combine status_code, title, response_features (headers/content type/length), and content_summary from the same source; retain all relevant queryable fields"},
			"source_tool_call_id": {"type": "string", "description": "Copy _execution.source_tool_call_id from a successful get_endpoint/read_sample result. This evidence record UUID is different from asset_id. The exact original call_id is also accepted."},
			"asset_id": {"type": "string", "description": "Optional UUID of the target asset"}
		},
		"required": ["obs_type", "content", "source_tool_call_id"]
	}`)
}

func (t *RecordObservationTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		ObsType          string `json:"obs_type"`
		Content          string `json:"content"`
		SourceToolCallID string `json:"source_tool_call_id"`
		AssetID          string `json:"asset_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if payload.ObsType == "" || payload.Content == "" || payload.SourceToolCallID == "" {
		return errors.New("obs_type, content, and source_tool_call_id are required")
	}
	if strings.TrimSpace(payload.AssetID) != "" {
		if _, err := uuid.Parse(strings.TrimSpace(payload.AssetID)); err != nil {
			return fmt.Errorf("invalid asset_id: must be a valid UUID: %w", err)
		}
	}
	return nil
}

func (t *RecordObservationTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		ObsType          string                 `json:"obs_type"`
		Content          string                 `json:"content"`
		Data             map[string]interface{} `json:"data"`
		SourceToolCallID string                 `json:"source_tool_call_id"`
		AssetID          string                 `json:"asset_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	// Resolve tool call ID: could be UUID or call_id string
	var tcUUID uuid.UUID
	parsed, err := uuid.Parse(payload.SourceToolCallID)
	if err == nil {
		tcUUID = parsed
	} else {
		// Look up by call_id for THIS ATTEMPT'S SESSION first!
		if t.sessionID != uuid.Nil {
			tc, listErr := t.runService.GetAgentToolCallBySessionAndCallID(ctx, t.sessionID, payload.SourceToolCallID)
			if listErr != nil {
				return "", service.ErrInvalidObservationSource
			}
			tcUUID = tc.ID
		} else {
			// Fallback: look up by call_id for this run
			tools, listErr := t.runService.ListAgentToolCalls(ctx, t.runID)
			if listErr != nil {
				return "", listErr
			}
			found := false
			for _, tc := range tools {
				if tc.CallID == payload.SourceToolCallID {
					tcUUID = tc.ID
					found = true
					break
				}
			}
			if !found {
				return "", service.ErrInvalidObservationSource
			}
		}
	}

	var parsedAssetID *uuid.UUID
	trimmedAssetID := strings.TrimSpace(payload.AssetID)
	if trimmedAssetID != "" {
		aid, err := uuid.Parse(trimmedAssetID)
		if err != nil {
			return "", fmt.Errorf("invalid asset_id: must be a valid UUID: %w", err)
		}
		parsedAssetID = &aid
	}

	dataBytes := json.RawMessage(`{}`)
	if payload.Data != nil {
		dataBytes, _ = json.Marshal(payload.Data)
	}
	obs, err := t.runService.CreateObservationWithAsset(ctx, t.runID, t.attemptID, payload.ObsType, payload.Content, dataBytes, tcUUID, parsedAssetID)
	if err != nil {
		return "", err
	}

	return fmt.Sprintf(`{"status":"recorded","observation_id":"%s"}`, obs.ID), nil
}

// ListRunAssetsTool allows the Planner to query all assets in scope for this run.
type ListRunAssetsTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewListRunAssetsTool(runID uuid.UUID, runService *service.RunService) *ListRunAssetsTool {
	return &ListRunAssetsTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ListRunAssetsTool) Name() string {
	return "list_run_assets"
}

func (t *ListRunAssetsTool) Description() string {
	return "List all assets in scope for the current run, including their IDs, keys, and descriptions."
}

func (t *ListRunAssetsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

func (t *ListRunAssetsTool) Validate(argumentsJSON []byte) error {
	return nil
}

func (t *ListRunAssetsTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	assets, err := t.runService.ListAssetsByRun(ctx, t.runID)
	if err != nil {
		return "", err
	}
	type assetItem struct {
		ID          string `json:"id"`
		Namespace   string `json:"namespace"`
		Kind        string `json:"kind"`
		ExternalKey string `json:"external_key"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	items := make([]assetItem, 0, len(assets))
	for _, a := range assets {
		items = append(items, assetItem{
			ID:          a.ID.String(),
			Namespace:   a.Namespace,
			Kind:        a.Kind,
			ExternalKey: a.ExternalKey,
			Name:        a.Name,
			Description: a.Description,
		})
	}
	b, _ := json.Marshal(map[string]interface{}{
		"assets": items,
		"count":  len(items),
	})
	return string(b), nil
}

// ReadObservationsTool allows aggregation workers to inspect observations from previous items.
type ReadObservationsTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewReadObservationsTool(runID uuid.UUID, runService *service.RunService) *ReadObservationsTool {
	return &ReadObservationsTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ReadObservationsTool) Name() string {
	return "read_observations"
}

func (t *ReadObservationsTool) Description() string {
	return "Read all recorded observations and evidence gathered by prerequisite work items."
}

func (t *ReadObservationsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {}
	}`)
}

func (t *ReadObservationsTool) Validate(argumentsJSON []byte) error {
	return nil
}

func (t *ReadObservationsTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	obs, err := t.runService.ListObservationsByRun(ctx, t.runID)
	if err != nil {
		return "", err
	}

	out, err := json.Marshal(map[string]interface{}{
		"total_observations": len(obs),
		"observations":       obs,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// ContinueWorkItemTool allows Planner to transition an exhausted work item back to pending for continuation.
type ContinueWorkItemTool struct {
	runID      uuid.UUID
	runService *service.RunService
	onContinue func(key string)
}

func NewContinueWorkItemTool(runID uuid.UUID, runService *service.RunService, onContinue func(key string)) *ContinueWorkItemTool {
	return &ContinueWorkItemTool{
		runID:      runID,
		runService: runService,
		onContinue: onContinue,
	}
}

func (t *ContinueWorkItemTool) Name() string {
	return "continue_work_item"
}

func (t *ContinueWorkItemTool) Description() string {
	return "Continue an exhausted work item for a new attempt, preserving historical conversation and avoiding duplicate network operations."
}

func (t *ContinueWorkItemTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"work_item_key": {"type": "string", "description": "Key of the exhausted work item to continue"},
			"reason": {"type": "string", "description": "Justification for continuation based on partial findings"}
		},
		"required": ["work_item_key", "reason"]
	}`)
}

func (t *ContinueWorkItemTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		WorkItemKey string `json:"work_item_key"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if payload.WorkItemKey == "" || payload.Reason == "" {
		return errors.New("work_item_key and reason are required")
	}
	return nil
}

func (t *ContinueWorkItemTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		WorkItemKey string `json:"work_item_key"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}
	wi, err := t.runService.ContinueWorkItemByKey(ctx, t.runID, payload.WorkItemKey, payload.Reason)
	if err != nil {
		return "", fmt.Errorf("continue work item failed: %w", err)
	}
	if t.onContinue != nil {
		t.onContinue(payload.WorkItemKey)
	}
	res, _ := json.Marshal(map[string]any{
		"status":       "success",
		"work_item_id": wi.ID,
		"key":          wi.Key,
		"item_status":  wi.Status,
		"message":      "Work item successfully re-queued for next attempt",
	})
	return string(res), nil
}

// RetryWorkItemTool allows Planner to transition a failed or blocked work item back to pending.
type RetryWorkItemTool struct {
	runID      uuid.UUID
	runService *service.RunService
	onRetry    func(key string)
}

func NewRetryWorkItemTool(runID uuid.UUID, runService *service.RunService, onRetry func(key string)) *RetryWorkItemTool {
	return &RetryWorkItemTool{
		runID:      runID,
		runService: runService,
		onRetry:    onRetry,
	}
}

func (t *RetryWorkItemTool) Name() string {
	return "retry_work_item"
}

func (t *RetryWorkItemTool) Description() string {
	return "Retry a failed or blocked work item for a new attempt."
}

func (t *RetryWorkItemTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"work_item_key": {"type": "string", "description": "Key of the failed/blocked work item to retry"},
			"reason": {"type": "string", "description": "Justification for retry"}
		},
		"required": ["work_item_key", "reason"]
	}`)
}

func (t *RetryWorkItemTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		WorkItemKey string `json:"work_item_key"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if payload.WorkItemKey == "" || payload.Reason == "" {
		return errors.New("work_item_key and reason are required")
	}
	return nil
}

func (t *RetryWorkItemTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		WorkItemKey string `json:"work_item_key"`
		Reason      string `json:"reason"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}
	wi, err := t.runService.RetryWorkItemByKey(ctx, t.runID, payload.WorkItemKey, payload.Reason)
	if err != nil {
		return "", fmt.Errorf("retry work item failed: %w", err)
	}
	if t.onRetry != nil {
		t.onRetry(payload.WorkItemKey)
	}
	res, _ := json.Marshal(map[string]any{
		"status":       "success",
		"work_item_id": wi.ID,
		"key":          wi.Key,
		"item_status":  wi.Status,
		"message":      "Work item successfully re-queued for retry",
	})
	return string(res), nil
}

// ManagePlannerTodosTool allows Planner to manage cross-round persistent todos (ARTEX TodoStore).
type ManagePlannerTodosTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewManagePlannerTodosTool(runID uuid.UUID, runService *service.RunService) *ManagePlannerTodosTool {
	return &ManagePlannerTodosTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ManagePlannerTodosTool) Name() string {
	return "manage_planner_todos"
}

func (t *ManagePlannerTodosTool) Description() string {
	return "Manage persistent cross-round planner todo items (ARTEX TodoStore), tracking planned objectives, dependencies, and hypotheses across rounds."
}

func (t *ManagePlannerTodosTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"action": {"type": "string", "enum": ["list", "add_or_update"], "description": "Action to perform"},
			"item_key": {"type": "string", "description": "Unique key of the todo item (required for add_or_update)"},
			"description": {"type": "string", "description": "Description of the task or hypothesis"},
			"status": {"type": "string", "enum": ["pending", "in_progress", "completed", "cancelled"], "description": "Todo status"},
			"depends_on": {
				"type": "array",
				"items": {"type": "string"},
				"description": "Keys of prerequisite todos"
			}
		},
		"required": ["action"]
	}`)
}

func (t *ManagePlannerTodosTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		Action  string `json:"action"`
		ItemKey string `json:"item_key"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if payload.Action == "" {
		return errors.New("action is required")
	}
	if payload.Action == "add_or_update" && payload.ItemKey == "" {
		return errors.New("item_key is required for add_or_update")
	}
	return nil
}

func (t *ManagePlannerTodosTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Action      string   `json:"action"`
		ItemKey     string   `json:"item_key"`
		Description string   `json:"description"`
		Status      string   `json:"status"`
		DependsOn   []string `json:"depends_on"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}
	switch payload.Action {
	case "list":
		todos, err := t.runService.ListPlannerTodos(ctx, t.runID)
		if err != nil {
			return "", err
		}
		res, _ := json.Marshal(map[string]any{
			"total": len(todos),
			"todos": todos,
		})
		return string(res), nil
	case "add_or_update":
		todo, err := t.runService.AddOrUpdatePlannerTodo(ctx, t.runID, payload.ItemKey, payload.Description, payload.Status, payload.DependsOn)
		if err != nil {
			return "", err
		}
		res, _ := json.Marshal(map[string]any{
			"status": "success",
			"todo":   todo,
		})
		return string(res), nil
	default:
		return "", fmt.Errorf("unknown action: %s", payload.Action)
	}
}
