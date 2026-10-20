package checker

import (
	"encoding/json"
	"fmt"
	"strings"

	"agent-workbench/backend/internal/model"
)

// CheckDefinition describes a registered security check rule or suite.
type CheckDefinition struct {
	ID               string          `json:"id"`
	Name             string          `json:"name"`
	Version          string          `json:"version"`
	Category         string          `json:"category"`
	Description      string          `json:"description"`
	ParametersSchema json.RawMessage `json:"parameters_schema"`
}

// RuleResult represents the deterministic outcome of a single rule evaluation.
type RuleResult struct {
	CheckID            string            `json:"check_id"`
	CheckName          string            `json:"check_name"`
	CheckVersion       string            `json:"check_version"`
	Status             model.CheckStatus `json:"status"`
	Explanation        string            `json:"explanation"`
	EvidenceSnapshot   json.RawMessage   `json:"evidence_snapshot"`
	Details            json.RawMessage   `json:"details"`
	GenerateFinding    bool              `json:"generate_finding"`
	FindingTitle       string            `json:"finding_title,omitempty"`
	FindingCategory    string            `json:"finding_category,omitempty"`
	FindingSeverity    string            `json:"finding_severity,omitempty"`
	FindingDescription string            `json:"finding_description,omitempty"`
	FindingRemediation string            `json:"finding_remediation,omitempty"`
}

var registeredChecks = map[string]CheckDefinition{
	"http_security_headers": {
		ID:          "http_security_headers",
		Name:        "HTTP响应安全配置审查",
		Version:     "1.0.0",
		Category:    "configuration_review",
		Description: "审查 HTTP 响应头中的安全配置，包括 X-Content-Type-Options、Content-Security-Policy 与 Strict-Transport-Security。",
		ParametersSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"asset_id": {"type": "string", "description": "Target HTTP endpoint asset UUID"},
				"check_id": {"type": "string", "enum": ["http_security_headers", "http_x_content_type_options", "http_content_security_policy", "http_strict_transport_security"]}
			},
			"required": ["asset_id", "check_id"]
		}`),
	},
	"http_x_content_type_options": {
		ID:          "http_x_content_type_options",
		Name:        "X-Content-Type-Options 响应头检查",
		Version:     "1.0.0",
		Category:    "configuration_review",
		Description: "检查 HTTP 响应是否配置了 X-Content-Type-Options: nosniff 防护 MIME 嗅探。",
		ParametersSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"asset_id": {"type": "string", "description": "Target HTTP endpoint asset UUID"},
				"check_id": {"type": "string", "const": "http_x_content_type_options"}
			},
			"required": ["asset_id", "check_id"]
		}`),
	},
	"http_content_security_policy": {
		ID:          "http_content_security_policy",
		Name:        "Content-Security-Policy 响应头存在性检查",
		Version:     "1.0.0",
		Category:    "configuration_review",
		Description: "仅检查 HTML 页面是否存在非空 CSP 响应头，不评价策略有效性或强度。",
		ParametersSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"asset_id": {"type": "string", "description": "Target HTTP endpoint asset UUID"},
				"check_id": {"type": "string", "const": "http_content_security_policy"}
			},
			"required": ["asset_id", "check_id"]
		}`),
	},
	"http_strict_transport_security": {
		ID:          "http_strict_transport_security",
		Name:        "Strict-Transport-Security (HSTS) 检查",
		Version:     "1.0.0",
		Category:    "configuration_review",
		Description: "检查 HTTPS 响应是否配置了 Strict-Transport-Security 强制传输安全。",
		ParametersSchema: json.RawMessage(`{
			"type": "object",
			"properties": {
				"asset_id": {"type": "string", "description": "Target HTTP endpoint asset UUID"},
				"check_id": {"type": "string", "const": "http_strict_transport_security"}
			},
			"required": ["asset_id", "check_id"]
		}`),
	},
}

// GetCatalog returns all registered check definitions.
func GetCatalog() []CheckDefinition {
	defs := make([]CheckDefinition, 0, len(registeredChecks))
	// Return in stable order
	order := []string{
		"http_security_headers",
		"http_x_content_type_options",
		"http_content_security_policy",
		"http_strict_transport_security",
	}
	for _, id := range order {
		if def, ok := registeredChecks[id]; ok {
			defs = append(defs, def)
		}
	}
	return defs
}

// IsRegisteredCheck verifies if a check ID is registered.
func IsRegisteredCheck(checkID string) bool {
	_, ok := registeredChecks[strings.TrimSpace(checkID)]
	return ok
}

// GetCheckDefinition returns the definition for a given check ID.
func GetCheckDefinition(checkID string) (CheckDefinition, error) {
	def, ok := registeredChecks[strings.TrimSpace(checkID)]
	if !ok {
		return CheckDefinition{}, fmt.Errorf("unknown check_id %q: must be one of registered checks in catalog", checkID)
	}
	return def, nil
}

// IncludesRule binds recorded rule outcomes to the rule or suite actually planned.
func IncludesRule(planned, actual string) bool {
	if !IsRegisteredCheck(planned) || !IsRegisteredCheck(actual) {
		return false
	}
	return planned == actual || (planned == "http_security_headers" && actual != "http_security_headers")
}
