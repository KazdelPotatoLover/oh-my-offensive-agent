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

// SetGoalsTool allows setting or decomposing task goals.
type SetGoalsTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewSetGoalsTool(runID uuid.UUID, runService *service.RunService) *SetGoalsTool {
	return &SetGoalsTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *SetGoalsTool) Name() string {
	return "set_goals"
}

func (t *SetGoalsTool) Description() string {
	return "Set or decompose objectives for the pentest run. Each goal represents a discrete independently-verifiable deliverable."
}

func (t *SetGoalsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"goals": {
				"type": "array",
				"description": "List of goals to record",
				"items": {
					"type": "object",
					"properties": {
						"text": {"type": "string", "description": "Clear description of the verifiable goal"},
						"vulnclass": {"type": "string", "description": "Optional vulnerability category (e.g., sqli, xss, idor)"}
					},
					"required": ["text"]
				}
			}
		},
		"required": ["goals"]
	}`)
}

func (t *SetGoalsTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		Goals []model.GoalSpec `json:"goals"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if len(payload.Goals) == 0 {
		return errors.New("goals array cannot be empty")
	}
	for i, g := range payload.Goals {
		if strings.TrimSpace(g.Text) == "" {
			return fmt.Errorf("goal %d text cannot be empty", i)
		}
	}
	return nil
}

func (t *SetGoalsTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Goals []model.GoalSpec `json:"goals"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	nodes, err := t.runService.SetGoals(ctx, t.runID, payload.Goals)
	if err != nil {
		return "", err
	}

	type goalResp struct {
		ID        string `json:"id"`
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass,omitempty"`
		State     string `json:"state"`
	}
	resp := make([]goalResp, 0, len(nodes))
	for _, n := range nodes {
		var p map[string]any
		_ = json.Unmarshal(n.Payload, &p)
		text, _ := p["text"].(string)
		vc, _ := p["vulnclass"].(string)
		resp = append(resp, goalResp{
			ID:        n.ID.String(),
			Text:      text,
			VulnClass: vc,
			State:     n.State,
		})
	}

	out, _ := json.Marshal(map[string]any{
		"status": "goals_set",
		"count":  len(resp),
		"goals":  resp,
	})
	return string(out), nil
}

// ListGoalsTool lists all goals for the run.
type ListGoalsTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewListGoalsTool(runID uuid.UUID, runService *service.RunService) *ListGoalsTool {
	return &ListGoalsTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ListGoalsTool) Name() string {
	return "list_goals"
}

func (t *ListGoalsTool) Description() string {
	return "List all defined goals and their verification states (open, met, abandoned) for this run."
}

func (t *ListGoalsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {}
	}`)
}

func (t *ListGoalsTool) Validate(argumentsJSON []byte) error {
	return nil
}

func (t *ListGoalsTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	goals, err := t.runService.ListGoals(ctx, t.runID)
	if err != nil {
		return "", err
	}

	type goalItem struct {
		ID        string `json:"id"`
		State     string `json:"state"`
		Text      string `json:"text"`
		VulnClass string `json:"vulnclass,omitempty"`
	}
	items := make([]goalItem, 0, len(goals))
	for _, g := range goals {
		var p map[string]any
		_ = json.Unmarshal(g.Payload, &p)
		text, _ := p["text"].(string)
		vc, _ := p["vulnclass"].(string)
		items = append(items, goalItem{
			ID:        g.ID.String(),
			State:     g.State,
			Text:      text,
			VulnClass: vc,
		})
	}

	out, _ := json.Marshal(map[string]any{
		"total": len(items),
		"goals": items,
	})
	return string(out), nil
}

// RecordFactTool allows workers or planners to record observed facts.
type RecordFactTool struct {
	runID          uuid.UUID
	intentID       *uuid.UUID
	runService     *service.RunService
	onFactRecorded func()
}

func NewRecordFactTool(runID uuid.UUID, intentID *uuid.UUID, runService *service.RunService) *RecordFactTool {
	return &RecordFactTool{
		runID:      runID,
		intentID:   intentID,
		runService: runService,
	}
}

func NewRecordFactToolWithCallback(runID uuid.UUID, intentID *uuid.UUID, runService *service.RunService, onFactRecorded func()) *RecordFactTool {
	return &RecordFactTool{
		runID:          runID,
		intentID:       intentID,
		runService:     runService,
		onFactRecorded: onFactRecorded,
	}
}

func (t *RecordFactTool) Name() string {
	return "record_fact"
}

func (t *RecordFactTool) Description() string {
	return "Record an empirical or observed fact/conclusion into the exploration graph, linked to current intent and assets."
}

func (t *RecordFactTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {"type": "string", "description": "Concise summary of the observed conclusion (one sentence)"},
			"detail": {"type": "string", "description": "Supporting details, findings, or features"},
			"confidence": {"type": "string", "enum": ["observed", "inferred"], "description": "observed (directly seen in tool output) or inferred (deduced)"},
			"evidence": {"type": "string", "description": "One line of critical evidence or output snippet"},
			"intent_id": {"type": "string", "description": "UUID of the intent node that produced this fact (defaults to current worker intent if omitted)"},
			"asset_ids": {"type": "array", "items": {"type": "string"}, "description": "UUIDs of related assets"}
		},
		"required": ["summary"]
	}`)
}

func (t *RecordFactTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.Summary) == "" {
		return errors.New("summary cannot be empty")
	}
	return nil
}

func (t *RecordFactTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Summary    string   `json:"summary"`
		Detail     string   `json:"detail"`
		Confidence string   `json:"confidence"`
		Evidence   string   `json:"evidence"`
		IntentID   string   `json:"intent_id"`
		AssetIDs   []string `json:"asset_ids"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	intentUUID := t.intentID
	if strings.TrimSpace(payload.IntentID) != "" {
		parsed, err := uuid.Parse(strings.TrimSpace(payload.IntentID))
		if err == nil {
			intentUUID = &parsed
		}
	}

	var assetUUIDs []uuid.UUID
	for _, s := range payload.AssetIDs {
		if u, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
			assetUUIDs = append(assetUUIDs, u)
		}
	}

	node, err := t.runService.RecordFact(
		ctx, t.runID,
		payload.Summary, payload.Detail, payload.Confidence, payload.Evidence,
		intentUUID, assetUUIDs,
	)
	if err != nil {
		return "", err
	}

	if t.onFactRecorded != nil && node.IsNew {
		t.onFactRecorded()
	}

	out, _ := json.Marshal(map[string]any{
		"status":  "recorded",
		"fact_id": node.ID.String(),
	})
	return string(out), nil
}

// ListFactsTool lists facts recorded for this run.
type ListFactsTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewListFactsTool(runID uuid.UUID, runService *service.RunService) *ListFactsTool {
	return &ListFactsTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ListFactsTool) Name() string {
	return "list_facts"
}

func (t *ListFactsTool) Description() string {
	return "List exploration facts and conclusions recorded in the current run."
}

func (t *ListFactsTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"limit": {"type": "integer", "description": "Maximum number of facts to return (default 20, max 100)"}
		}
	}`)
}

func (t *ListFactsTool) Validate(argumentsJSON []byte) error {
	return nil
}

func (t *ListFactsTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Limit int `json:"limit"`
	}
	_ = json.Unmarshal(argumentsJSON, &payload)
	limit := payload.Limit
	if limit <= 0 {
		limit = 20
	}

	facts, err := t.runService.ListFacts(ctx, t.runID, limit)
	if err != nil {
		return "", err
	}

	type factItem struct {
		ID         string `json:"id"`
		Summary    string `json:"summary"`
		Confidence string `json:"confidence"`
		Evidence   string `json:"evidence"`
	}
	items := make([]factItem, 0, len(facts))
	for _, f := range facts {
		var p map[string]any
		_ = json.Unmarshal(f.Payload, &p)
		items = append(items, factItem{
			ID:         f.ID.String(),
			Summary:    fmt.Sprintf("%v", p["summary"]),
			Confidence: fmt.Sprintf("%v", p["confidence"]),
			Evidence:   fmt.Sprintf("%v", p["evidence"]),
		})
	}

	out, _ := json.Marshal(map[string]any{
		"total": len(items),
		"facts": items,
	})
	return string(out), nil
}

// ProveGoalTool marks a goal as met supported by verified fact or finding evidence.
type ProveGoalTool struct {
	runID      uuid.UUID
	runService *service.RunService
	onGoalMet  func(allGoalsMet bool)
}

func NewProveGoalTool(runID uuid.UUID, runService *service.RunService, onGoalMet func(bool)) *ProveGoalTool {
	return &ProveGoalTool{
		runID:      runID,
		runService: runService,
		onGoalMet:  onGoalMet,
	}
}

func (t *ProveGoalTool) Name() string {
	return "prove_goal"
}

func (t *ProveGoalTool) Description() string {
	return "When you verify that an observed fact or finding satisfies a defined goal, link the evidence to the goal and mark the goal as met."
}

func (t *ProveGoalTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"goal_id": {"type": "string", "description": "UUID of the target goal node"},
			"evidence_id": {"type": "string", "description": "UUID of the supporting fact or finding node that proves this goal"},
			"reason": {"type": "string", "description": "Explanation of how this evidence proves the goal"}
		},
		"required": ["goal_id", "evidence_id"]
	}`)
}

func (t *ProveGoalTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		GoalID     string `json:"goal_id"`
		EvidenceID string `json:"evidence_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if _, err := uuid.Parse(strings.TrimSpace(payload.GoalID)); err != nil {
		return fmt.Errorf("invalid goal_id: must be a valid UUID")
	}
	if _, err := uuid.Parse(strings.TrimSpace(payload.EvidenceID)); err != nil {
		return fmt.Errorf("invalid evidence_id: must be a valid UUID")
	}
	return nil
}

func (t *ProveGoalTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		GoalID     string `json:"goal_id"`
		EvidenceID string `json:"evidence_id"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	goalUUID := uuid.MustParse(strings.TrimSpace(payload.GoalID))
	evidenceUUID := uuid.MustParse(strings.TrimSpace(payload.EvidenceID))

	allMet, err := t.runService.ProveGoal(ctx, t.runID, goalUUID, evidenceUUID, payload.Reason)
	if err != nil {
		return "", err
	}

	if t.onGoalMet != nil {
		t.onGoalMet(allMet)
	}

	msg := fmt.Sprintf("goal %s marked met", goalUUID)
	if allMet {
		msg += " - all goals in this run are now met"
	}

	out, _ := json.Marshal(map[string]any{
		"status":        "proved",
		"goal_id":       goalUUID.String(),
		"evidence_id":   evidenceUUID.String(),
		"all_goals_met": allMet,
		"message":       msg,
	})
	return string(out), nil
}

// RecordHintTool records strategic hints into the exploration graph.
type RecordHintTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewRecordHintTool(runID uuid.UUID, runService *service.RunService) *RecordHintTool {
	return &RecordHintTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *RecordHintTool) Name() string {
	return "record_hint"
}

func (t *RecordHintTool) Description() string {
	return "Record a strategic hint or instruction into the exploration graph."
}

func (t *RecordHintTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"text": {"type": "string", "description": "Hint or guidance text"},
			"asset_ids": {"type": "array", "items": {"type": "string"}, "description": "Optional asset UUIDs"}
		},
		"required": ["text"]
	}`)
}

func (t *RecordHintTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if strings.TrimSpace(payload.Text) == "" {
		return errors.New("text cannot be empty")
	}
	return nil
}

func (t *RecordHintTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		Text     string   `json:"text"`
		AssetIDs []string `json:"asset_ids"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}

	var assetUUIDs []uuid.UUID
	for _, s := range payload.AssetIDs {
		if u, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
			assetUUIDs = append(assetUUIDs, u)
		}
	}

	node, err := t.runService.RecordHint(ctx, t.runID, payload.Text, assetUUIDs)
	if err != nil {
		return "", err
	}

	out, _ := json.Marshal(map[string]any{
		"status":  "recorded",
		"hint_id": node.ID.String(),
	})
	return string(out), nil
}

// GraphOverviewTool retrieves the current snapshot of goals, facts, hints, and frontier intents.
type GraphOverviewTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewGraphOverviewTool(runID uuid.UUID, runService *service.RunService) *GraphOverviewTool {
	return &GraphOverviewTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *GraphOverviewTool) Name() string {
	return "graph_overview"
}

func (t *GraphOverviewTool) Description() string {
	return "Get the current situational overview of the exploration graph (goals, recent facts, strategic hints, and open intents)."
}

func (t *GraphOverviewTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {}
	}`)
}

func (t *GraphOverviewTool) Validate(argumentsJSON []byte) error {
	return nil
}

func (t *GraphOverviewTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	overview, err := t.runService.GraphOverview(ctx, t.runID)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(overview)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// NodeDetailTool returns the complete payload, edges, and anchors of an exploration node.
type NodeDetailTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewNodeDetailTool(runID uuid.UUID, runService *service.RunService) *NodeDetailTool {
	return &NodeDetailTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *NodeDetailTool) Name() string {
	return "node_detail"
}

func (t *NodeDetailTool) Description() string {
	return "Retrieve the full details, parsed payload, incoming/outgoing edges, and anchors of an exploration node by its UUID."
}

func (t *NodeDetailTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "UUID of the exploration node"}
		},
		"required": ["id"]
	}`)
}

func (t *NodeDetailTool) Validate(argumentsJSON []byte) error {
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return err
	}
	if _, err := uuid.Parse(strings.TrimSpace(payload.ID)); err != nil {
		return fmt.Errorf("invalid id: must be a valid UUID")
	}
	return nil
}

func (t *NodeDetailTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var payload struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(argumentsJSON, &payload); err != nil {
		return "", err
	}
	nodeUUID := uuid.MustParse(strings.TrimSpace(payload.ID))

	detail, err := t.runService.GetExplorationNodeDetail(ctx, t.runID, nodeUUID)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(detail)
	if err != nil {
		return "", err
	}
	return string(out), nil
}
