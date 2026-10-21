package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

type ReportFindingTool struct {
	runID      uuid.UUID
	runService *service.RunService
}

func NewReportFindingTool(runID uuid.UUID, runService *service.RunService) *ReportFindingTool {
	return &ReportFindingTool{
		runID:      runID,
		runService: runService,
	}
}

func (t *ReportFindingTool) Name() string {
	return "report_finding"
}

func (t *ReportFindingTool) Description() string {
	return "Report an empirical or speculative security finding with verified evidence and reproduce steps."
}

func (t *ReportFindingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {
				"type": "string",
				"description": "Short, clear title of the vulnerability"
			},
			"category": {
				"type": "string",
				"description": "Vulnerability category (e.g. 'injection', 'auth_bypass', 'csrf')"
			},
			"severity": {
				"type": "string",
				"enum": ["info", "low", "medium", "high", "critical"],
				"description": "Impact severity rating"
			},
			"description": {
				"type": "string",
				"description": "Comprehensive description of the flaw and potential consequences"
			},
			"remediation": {
				"type": "string",
				"description": "Recommended defensive actions or patches"
			},
			"source_kind": {
				"type": "string",
				"enum": ["empirical", "speculative"],
				"description": "'empirical' for proven, reproducible exploits; 'speculative' for suspected weaknesses"
			},
			"asset_id": {
				"type": "string",
				"description": "Target asset UUID where the vulnerability exists"
			},
			"trigger_condition": {
				"type": "string",
				"description": "Specific trigger condition or request payload"
			},
			"reproduce_steps": {
				"type": "string",
				"description": "Step-by-step reproducible instructions"
			},
			"source_tool_call_id": {
				"type": "string",
				"description": "UUID of the successful tool call (_execution.source_tool_call_id) providing empirical evidence"
			},
			"verification": {"type":"object","description":"Optional HTTP predicates saved with the original finding. vulnerable and fixed each require status_code and body_contains or header_name/header_value. The initial response must uniquely match vulnerable."},
			"evidence": {
				"type": "object",
				"description": "Structured proof evidence (HTTP request/response excerpts, payload output, etc.)"
			}
		},
		"required": ["title", "category", "severity", "description", "asset_id"]
	}`)
}

func (t *ReportFindingTool) Validate(argumentsJSON []byte) error {
	var in struct {
		Title       string `json:"title"`
		Category    string `json:"category"`
		Severity    string `json:"severity"`
		Description string `json:"description"`
		AssetID     string `json:"asset_id"`
	}
	if err := json.Unmarshal(argumentsJSON, &in); err != nil {
		return fmt.Errorf("invalid report_finding arguments JSON: %w", err)
	}
	if in.Title == "" || in.Category == "" || in.Severity == "" || in.Description == "" || in.AssetID == "" {
		return fmt.Errorf("title, category, severity, description, and asset_id are required")
	}
	return nil
}

func (t *ReportFindingTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	var in struct {
		Title            string                       `json:"title"`
		Category         string                       `json:"category"`
		Severity         string                       `json:"severity"`
		Description      string                       `json:"description"`
		Remediation      string                       `json:"remediation"`
		SourceKind       string                       `json:"source_kind"`
		AssetID          string                       `json:"asset_id"`
		TriggerCondition string                       `json:"trigger_condition"`
		ReproduceSteps   string                       `json:"reproduce_steps"`
		SourceToolCallID string                       `json:"source_tool_call_id"`
		Evidence         json.RawMessage              `json:"evidence"`
		Verification     *service.FindingVerification `json:"verification"`
	}
	if err := json.Unmarshal(argumentsJSON, &in); err != nil {
		return "", fmt.Errorf("unmarshal report_finding: %w", err)
	}

	assetUUID, err := uuid.Parse(in.AssetID)
	if err != nil {
		return "", fmt.Errorf("invalid asset_id UUID: %w", err)
	}

	var sourceToolCallUUID *uuid.UUID
	if in.SourceToolCallID != "" {
		stcID, err := uuid.Parse(in.SourceToolCallID)
		if err == nil {
			sourceToolCallUUID = &stcID
		}
	}

	finding, err := t.runService.ReportFinding(ctx, t.runID, service.ReportFindingInput{
		AssetID:          assetUUID,
		Title:            in.Title,
		Category:         in.Category,
		Severity:         in.Severity,
		Description:      in.Description,
		Remediation:      in.Remediation,
		SourceKind:       in.SourceKind,
		SourceToolCallID: sourceToolCallUUID,
		EvidenceRef:      in.Evidence,
		Verification:     in.Verification,
		TriggerCondition: in.TriggerCondition,
		ReproduceSteps:   in.ReproduceSteps,
	})
	if err != nil {
		return "", err
	}

	out, _ := json.Marshal(map[string]interface{}{
		"finding_id":         finding.ID,
		"status":             finding.Status,
		"source_kind":        finding.SourceKind,
		"verification_state": finding.VerificationState,
		"message":            fmt.Sprintf("Finding '%s' recorded with verification state '%s'", finding.Title, finding.VerificationState),
	})
	return string(out), nil
}
