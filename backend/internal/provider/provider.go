package provider

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var ErrProtocol = errors.New("invalid model protocol response")

// NativePayload is opaque conversation state. Never serialize it to the public API.
type NativePayload struct {
	Format string            `json:"format"`
	Output []json.RawMessage `json:"output"`
}

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
	Role          ChatRole       `json:"role"`
	Content       string         `json:"content"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	ToolCallID    string         `json:"tool_call_id,omitempty"`
	NativePayload *NativePayload `json:"-"`
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
	Content       string         `json:"content"`
	ToolCalls     []ToolCall     `json:"tool_calls,omitempty"`
	FinishReason  string         `json:"finish_reason"` // "stop", "tool_calls", "length", "refusal", "error"
	Usage         *ModelUsage    `json:"usage,omitempty"`
	RequestID     string         `json:"request_id,omitempty"`
	ResponseID    string         `json:"response_id,omitempty"`
	NativePayload *NativePayload `json:"-"`
}

type ModelOpts struct {
	ModelName       string
	Timeout         time.Duration
	MaxOutputTokens int
}

type Provider interface {
	Call(ctx context.Context, messages []ChatMessage, tools []ToolDefinition, opts ModelOpts) (*ModelResponse, error)
}
