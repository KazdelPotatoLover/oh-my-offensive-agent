package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

type SampleData struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Method      string            `json:"method"`
	StatusCode  int               `json:"status_code"`
	Headers     map[string]string `json:"headers"`
	Body        string            `json:"body"`
}

var BuiltinSamples = map[string]SampleData{
	"sample_home": {
		ID:          "sample_home",
		Description: "Website homepage (index)",
		Method:      "GET",
		StatusCode:  200,
		Headers: map[string]string{
			"Content-Type": "text/html; charset=utf-8",
			"Server":       "SampleServer/1.0",
		},
		Body: "<!DOCTYPE html><html><head><title>Company Portal Home</title></head><body><h1>Welcome to Acme Portal</h1><p>Public landing page.</p></body></html>",
	},
	"sample_login": {
		ID:          "sample_login",
		Description: "User login page with HTML form",
		Method:      "GET",
		StatusCode:  200,
		Headers: map[string]string{
			"Content-Type":    "text/html; charset=utf-8",
			"X-Frame-Options": "DENY",
		},
		Body: "<!DOCTYPE html><html><head><title>Account Login</title></head><body><h1>Login Page</h1><form method=\"POST\" action=\"/login\"><input type=\"text\" name=\"user\"/></form></body></html>",
	},
	"sample_health": {
		ID:          "sample_health",
		Description: "Service health check endpoint",
		Method:      "GET",
		StatusCode:  200,
		Headers: map[string]string{
			"Content-Type": "application/json",
		},
		Body: "{\"status\": \"UP\", \"version\": \"1.2.0\", \"title\": \"System Health Status\"}",
	},
}

// ListSamplesTool implements the list_samples tool.
type ListSamplesTool struct{}

func NewListSamplesTool() *ListSamplesTool {
	return &ListSamplesTool{}
}

func (t *ListSamplesTool) Name() string {
	return "list_samples"
}

func (t *ListSamplesTool) Description() string {
	return "List all built-in HTTP response sample IDs and brief descriptions"
}

func (t *ListSamplesTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {},
		"additionalProperties": false
	}`)
}

func (t *ListSamplesTool) Validate(argumentsJSON []byte) error {
	trimmed := strings.TrimSpace(string(argumentsJSON))
	if trimmed == "" || trimmed == "{}" {
		return nil
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(argumentsJSON, &raw); err != nil {
		return fmt.Errorf("malformed JSON arguments: %w", err)
	}
	if len(raw) > 0 {
		return fmt.Errorf("list_samples accepts no arguments, but got %d properties", len(raw))
	}
	return nil
}

func (t *ListSamplesTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	type SampleSummary struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		Method      string `json:"method"`
		StatusCode  int    `json:"status_code"`
	}

	// Stable order: sample_home, sample_login, sample_health
	keys := []string{"sample_home", "sample_login", "sample_health"}
	summaries := make([]SampleSummary, 0, len(keys))
	for _, k := range keys {
		s := BuiltinSamples[k]
		summaries = append(summaries, SampleSummary{
			ID:          s.ID,
			Description: s.Description,
			Method:      s.Method,
			StatusCode:  s.StatusCode,
		})
	}

	bytes, err := json.Marshal(map[string]interface{}{
		"samples": summaries,
		"count":   len(summaries),
	})
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// ReadSampleTool implements the read_sample tool.
type ReadSampleTool struct{}

func NewReadSampleTool() *ReadSampleTool {
	return &ReadSampleTool{}
}

func (t *ReadSampleTool) Name() string {
	return "read_sample"
}

func (t *ReadSampleTool) Description() string {
	return "Read details of a specific HTTP response sample by ID"
}

func (t *ReadSampleTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"sample_id": {
				"type": "string",
				"description": "The ID of the sample to read (e.g. sample_home, sample_login, sample_health)"
			}
		},
		"required": ["sample_id"],
		"additionalProperties": false
	}`)
}

type ReadSampleArgs struct {
	SampleID string `json:"sample_id"`
}

func (t *ReadSampleTool) Validate(argumentsJSON []byte) error {
	var raw map[string]interface{}
	if err := json.Unmarshal(argumentsJSON, &raw); err != nil {
		return fmt.Errorf("malformed JSON arguments: %w", err)
	}

	val, exists := raw["sample_id"]
	if !exists {
		return fmt.Errorf("missing required property 'sample_id'")
	}
	sampleID, ok := val.(string)
	if !ok {
		return fmt.Errorf("property 'sample_id' must be a string")
	}
	sampleID = strings.TrimSpace(sampleID)
	if sampleID == "" {
		return fmt.Errorf("property 'sample_id' cannot be empty")
	}

	// Disallow extra properties
	for k := range raw {
		if k != "sample_id" {
			return fmt.Errorf("unexpected extra property '%s'", k)
		}
	}

	// Must be one of the registered sample IDs
	if _, ok := BuiltinSamples[sampleID]; !ok {
		return fmt.Errorf("unknown sample_id '%s': must be one of sample_home, sample_login, sample_health", sampleID)
	}

	return nil
}

func (t *ReadSampleTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}

	var args ReadSampleArgs
	if err := json.Unmarshal(argumentsJSON, &args); err != nil {
		return "", err
	}

	sample, ok := BuiltinSamples[args.SampleID]
	if !ok {
		return "", fmt.Errorf("sample '%s' not found", args.SampleID)
	}

	bytes, err := json.Marshal(map[string]interface{}{
		"sample_id":   sample.ID,
		"status_code": sample.StatusCode,
		"headers":     sample.Headers,
		"body":        sample.Body,
	})
	if err != nil {
		return "", err
	}
	return string(bytes), nil
}

// DefaultRegistry creates a new ToolRegistry pre-registered with built-in sample tools.
func DefaultRegistry() *Registry {
	reg := NewRegistry()
	reg.Register(NewListSamplesTool())
	reg.Register(NewReadSampleTool())
	return reg
}
