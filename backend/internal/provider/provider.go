package provider

import (
	"context"
	"encoding/json"
	"time"
)

type ChatRole string

const (
	RoleSystem    ChatRole = "system"
	RoleUser      ChatRole = "user"
	RoleAssistant ChatRole = "assistant"
	RoleTool      ChatRole = "tool"
)

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"` // "function"
	Function FunctionCall `json:"function"`
}

type ChatMessage struct {
	Role       ChatRole   `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

type ToolDefinition struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ModelUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type ModelResponse struct {
	Content      string      `json:"content"`
	ToolCalls    []ToolCall  `json:"tool_calls,omitempty"`
	FinishReason string      `json:"finish_reason"` // "stop", "tool_calls", "length", "refusal", "error"
	Usage        *ModelUsage `json:"usage,omitempty"`
	RequestID    string      `json:"request_id,omitempty"`
}

type ModelOpts struct {
	ModelName string
	Timeout   time.Duration
}

type Provider interface {
	Call(ctx context.Context, messages []ChatMessage, tools []ToolDefinition, opts ModelOpts) (*ModelResponse, error)
}
