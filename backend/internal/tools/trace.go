package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

// ListWorkerTracesTool allows workers and planners to see available works and their progress.
type ListWorkerTracesTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewListWorkerTracesTool(runID uuid.UUID, runService *service.RunService) *ListWorkerTracesTool {
	return &ListWorkerTracesTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ListWorkerTracesTool) Name() string {
	return "list_worker_traces"
}

func (t *ListWorkerTracesTool) Description() string {
	return "List executed and running work items/intents in this run with their keys, titles, status, and step counts."
}

func (t *ListWorkerTracesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"q": {"type": "string", "description": "Optional keyword filter for title or objective"},
			"limit": {"type": "integer", "description": "Max items to return (default 50)"}
		}
	}`)
}

func (t *ListWorkerTracesTool) Validate(argumentsJSON []byte) error {
	return nil
}

func (t *ListWorkerTracesTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Q     string `json:"q"`
		Limit int    `json:"limit"`
	}
	_ = json.Unmarshal(argumentsJSON, &payload)

	traces, err := t.runService.ListWorkerTraces(ctx, t.runID, payload.Q, payload.Limit)
	if err != nil {
		return "", err
	}

	out, err := json.Marshal(map[string]any{
		"total":  len(traces),
		"traces": traces,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// SearchAllWorkerTracesTool allows workers to search other workers' execution logs for unrecorded clues.
type SearchAllWorkerTracesTool struct {
	runID             uuid.UUID
	currentWorkItemID *uuid.UUID
	runService        *service.RunService
}

func NewSearchAllWorkerTracesTool(runID uuid.UUID, currentWorkItemID *uuid.UUID, runService *service.RunService) *SearchAllWorkerTracesTool {
	return &SearchAllWorkerTracesTool{
		runID:             runID,
		currentWorkItemID: currentWorkItemID,
		runService:        runService,
	}
}

func (t *SearchAllWorkerTracesTool) Name() string {
	return "search_all_worker_traces"
}

func (t *SearchAllWorkerTracesTool) Description() string {
	return "Search across execution steps and tool outputs of other workers in the current run for keywords (tokens, parameters, paths, error messages). Returns step summaries and step_ids for drill-down."
}

func (t *SearchAllWorkerTracesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"q": {"type": "string", "description": "Keyword to search across all worker outputs, arguments, and tools"},
			"limit": {"type": "integer", "description": "Max hits to return (default 50)"}
		},
		"required": ["q"]
	}`)
}

func (t *SearchAllWorkerTracesTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		Q string `json:"q"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.Q) == "" {
		return errors.New("search query q cannot be empty")
	}
	return nil
}

func (t *SearchAllWorkerTracesTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Q     string `json:"q"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	hits, err := t.runService.SearchAllWorkerTraces(ctx, t.runID, t.currentWorkItemID, payload.Q, payload.Limit)
	if err != nil {
		return "", err
	}

	out, err := json.Marshal(map[string]any{
		"query": payload.Q,
		"total": len(hits),
		"hits":  hits,
	})
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// GetWorkerTraceTool retrieves the step stream or full details of specific steps.
type GetWorkerTraceTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewGetWorkerTraceTool(runID uuid.UUID, runService *service.RunService) *GetWorkerTraceTool {
	return &GetWorkerTraceTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *GetWorkerTraceTool) Name() string {
	return "get_worker_trace"
}

func (t *GetWorkerTraceTool) Description() string {
	return "View execution trace for a specific work item/intent. Pass work_item_id alone to get step summaries; or pass work_item_id + step_ids to get full outputs and arguments (max 5 steps per call)."
}

func (t *GetWorkerTraceTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"work_item_id": {"type": "string", "description": "UUID of the work item or intent to inspect"},
			"q": {"type": "string", "description": "Optional keyword filter for steps in this work item"},
			"step_ids": {"type": "array", "items": {"type": "string"}, "description": "Specific step UUIDs to retrieve full content for (max 5)"},
			"limit": {"type": "integer", "description": "Max steps to return (default 50)"}
		},
		"required": ["work_item_id"]
	}`)
}

func (t *GetWorkerTraceTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		WorkItemID string `json:"work_item_id"`
		IntentID   string `json:"intent_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	target := payload.WorkItemID
	if target == "" {
		target = payload.IntentID
	}
	if strings.TrimSpace(target) == "" {
		return errors.New("work_item_id is required")
	}
	if _, err := uuid.Parse(strings.TrimSpace(target)); err != nil {
		return fmt.Errorf("invalid work_item_id: must be a valid UUID")
	}
	return nil
}

func (t *GetWorkerTraceTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		WorkItemID string   `json:"work_item_id"`
		IntentID   string   `json:"intent_id"`
		Q          string   `json:"q"`
		StepIDs    []string `json:"step_ids"`
		Limit      int      `json:"limit"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	target := payload.WorkItemID
	if target == "" {
		target = payload.IntentID
	}
	targetUUID := uuid.MustParse(strings.TrimSpace(target))

	var parsedStepIDs []uuid.UUID
	for _, s := range payload.StepIDs {
		if u, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
			parsedStepIDs = append(parsedStepIDs, u)
		}
	}

	res, err := t.runService.GetWorkerTrace(ctx, t.runID, targetUUID, payload.Q, parsedStepIDs, payload.Limit)
	if err != nil {
		return "", err
	}

	out, err := json.Marshal(res)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
