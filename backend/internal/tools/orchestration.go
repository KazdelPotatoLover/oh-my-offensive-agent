package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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
	return "Submit executable work items. HTTP targets MUST use inspect_http_endpoint with input_data.asset_id and asset_ids from list_run_assets; this worker fetches one asset and extracts links. Samples use analyze_sample with input_data.sample_id. aggregate_summary reads existing observations. Never invent work types, URLs, asset IDs, worklist references, or executable pipeline stages."
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
						"work_type": {"type": "string", "enum": ["inspect_http_endpoint", "analyze_sample", "inspect_sample_endpoint", "aggregate_summary"], "description": "Exact supported executor type. HTTP assets require inspect_http_endpoint; crawl_page and frontier_reconcile do not exist."},
						"input_data": {"type": "object", "description": "inspect_http_endpoint: {asset_id: UUID from asset_ids}; analyze_sample: {sample_id: scoped sample key}; aggregate_summary: {}. Dependencies control order only; they do not inject worklist artifacts.", "properties": {"asset_id": {"type": "string"}, "sample_id": {"type": "string"}}},
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

	if payload.Conclusion == "" {
		payload.Conclusion = "complete"
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
	return "Record a structured factual observation derived from a successful tool execution."
}

func (t *RecordObservationTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"obs_type": {"type": "string", "description": "Type of observation, e.g. http_status, page_title, server_header"},
			"content": {"type": "string", "description": "Text description of the observation"},
			"data": {"type": "object", "description": "Structured data extracted from tool output"},
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

	dataBytes, _ := json.Marshal(payload.Data)
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
