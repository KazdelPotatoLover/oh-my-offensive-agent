package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// ResponsesProvider has no mutable conversation state; planner and worker sessions
// can safely share it. Each request carries that session's complete history.
type ResponsesProvider struct {
	baseURL, apiKey, defaultModel string
	client                        *http.Client
}

func NewResponsesProvider(baseURL, apiKey, model string, client *http.Client) *ResponsesProvider {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	return &ResponsesProvider{strings.TrimRight(baseURL, "/"), apiKey, model, client}
}

type responseTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

func responsesInput(messages []ChatMessage) ([]json.RawMessage, error) {
	items := make([]json.RawMessage, 0, len(messages))
	appendItem := func(v interface{}) error {
		b, err := json.Marshal(v)
		if err == nil {
			items = append(items, b)
		}
		return err
	}
	for _, m := range messages {
		if m.NativePayload != nil {
			if m.Role != RoleAssistant || m.NativePayload.Format != FormatResponses || len(m.NativePayload.Output) == 0 {
				return nil, fmt.Errorf("%w: incompatible native conversation state", ErrProtocol)
			}
			for _, item := range m.NativePayload.Output {
				var kind struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(item, &kind); err != nil || (kind.Type != "reasoning" && kind.Type != "message" && kind.Type != "function_call") {
					return nil, fmt.Errorf("%w: unsupported replay item", ErrProtocol)
				}
				items = append(items, item)
			}
			continue
		}
		switch m.Role {
		case RoleTool:
			if m.ToolCallID == "" {
				return nil, fmt.Errorf("%w: tool result missing call_id", ErrProtocol)
			}
			if err := appendItem(map[string]interface{}{"type": "function_call_output", "call_id": m.ToolCallID, "output": m.Content}); err != nil {
				return nil, err
			}
		case RoleSystem, RoleUser, RoleAssistant:
			if m.Content != "" || len(m.ToolCalls) == 0 {
				if err := appendItem(map[string]interface{}{"role": m.Role, "content": m.Content}); err != nil {
					return nil, err
				}
			}
			for _, tc := range m.ToolCalls {
				if m.Role != RoleAssistant || tc.ID == "" {
					return nil, fmt.Errorf("%w: invalid historical tool call", ErrProtocol)
				}
				if err := appendItem(map[string]interface{}{"type": "function_call", "call_id": tc.ID, "name": tc.Function.Name, "arguments": tc.Function.Arguments}); err != nil {
					return nil, err
				}
			}
		default:
			return nil, fmt.Errorf("%w: unsupported message role", ErrProtocol)
		}
	}
	return items, nil
}

func (p *ResponsesProvider) Call(ctx context.Context, messages []ChatMessage, tools []ToolDefinition, opts ModelOpts) (*ModelResponse, error) {
	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
	}
	input, err := responsesInput(messages)
	if err != nil {
		return nil, err
	}
	model := opts.ModelName
	if model == "" {
		model = p.defaultModel
	}
	maxTokens, err := BoundOutputTokens(opts.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	defs := make([]responseTool, 0, len(tools))
	for _, t := range tools {
		defs = append(defs, responseTool{Type: "function", Name: t.Name, Description: t.Description, Parameters: t.Parameters, Strict: false})
	}
	body, err := json.Marshal(struct {
		Model           string            `json:"model"`
		Input           []json.RawMessage `json:"input"`
		Tools           []responseTool    `json:"tools,omitempty"`
		Store           bool              `json:"store"`
		Stream          bool              `json:"stream"`
		Include         []string          `json:"include"`
		MaxOutputTokens int               `json:"max_output_tokens"`
	}{model, input, defs, false, false, []string{"reasoning.encrypted_content"}, maxTokens})
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/responses", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+p.apiKey)
	}
	resp, err := p.client.Do(req)
	if err != nil {
		return nil, transportCallError(err)
	}
	defer resp.Body.Close()
	raw, err := readLimitedBody(resp.Body)
	if err != nil {
		return nil, err
	}
	requestID := boundRequestID(resp.Header.Get("x-request-id"))
	// Do not persist upstream error bodies: gateways can echo credentials or prompts.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ModelResponse{RequestID: requestID}, httpCallError(resp.StatusCode, requestID, resp.Header.Get("Retry-After"), raw, messageSecrets(p.apiKey, messages))
	}
	result, err := parseResponse(raw)
	if result == nil {
		var metadata struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal(raw, &metadata)
		result = &ModelResponse{ResponseID: metadata.ID}
	}
	result.RequestID = requestID
	if err != nil {
		err = annotateDiagnosis(err, resp.StatusCode, requestID, raw)
	}
	return result, err
}

func parseResponse(raw []byte) (*ModelResponse, error) {
	var wire struct {
		ID         string            `json:"id"`
		Status     string            `json:"status"`
		Output     []json.RawMessage `json:"output"`
		Error      json.RawMessage   `json:"error"`
		Incomplete struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Usage *struct {
			Input  int `json:"input_tokens"`
			Output int `json:"output_tokens"`
			Total  int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, fmt.Errorf("%w: invalid Responses JSON", ErrProtocol)
	}
	if len(wire.Error) > 0 && string(wire.Error) != "null" {
		return nil, &CallError{Class: ClassUpstream, Retryable: true, Summary: ErrOpenAIUpstream.Error() + ": Responses reported an error", unwrap: []error{ErrOpenAIUpstream}}
	}
	if wire.Status != "completed" && wire.Status != "incomplete" {
		return nil, protocolCallError(ClassBadSchema, fmt.Sprintf("unexpected Responses status %q", wire.Status), false)
	}
	r := &ModelResponse{ResponseID: wire.ID, FinishReason: "stop", NativePayload: &NativePayload{Format: FormatResponses, Output: wire.Output}}
	if wire.Usage != nil {
		r.Usage = &ModelUsage{wire.Usage.Input, wire.Usage.Output, wire.Usage.Total}
	}
	var text strings.Builder
	var parsedCalls []parsedFunctionCall
	refused, finalText := false, false
	for _, rawItem := range wire.Output {
		var item struct {
			Type      string          `json:"type"`
			Role      string          `json:"role"`
			CallID    string          `json:"call_id"`
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Phase     string          `json:"phase"`
			Status    string          `json:"status"`
			Content   []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return nil, protocolCallError(ClassBadSchema, "invalid output item", false)
		}
		switch item.Type {
		case "reasoning": // Preserve opaque encrypted reasoning for the next round.
		case "function_call":
			args, argsErr := NormalizeArguments(item.Arguments)
			parsedCalls = append(parsedCalls, parsedFunctionCall{
				ID: item.CallID, Name: item.Name, Arguments: args, Status: item.Status, ArgsOK: argsErr == nil,
			})
		case "message":
			if item.Role != "assistant" {
				return nil, fmt.Errorf("%w: output message must be assistant", ErrProtocol)
			}
			for _, c := range item.Content {
				switch c.Type {
				case "output_text":
					text.WriteString(c.Text)
					if strings.TrimSpace(c.Text) != "" && (item.Phase == "" || item.Phase == "final_answer") {
						finalText = true
					}
				case "refusal":
					refused = true
				default:
					return nil, protocolCallError(ClassBadSchema, fmt.Sprintf("unsupported message content %q", c.Type), false)
				}
			}
		default:
			return nil, protocolCallError(ClassBadSchema, fmt.Sprintf("unsupported output type %q", item.Type), false)
		}
	}
	r.Content = text.String()
	switch {
	case refused:
		r.FinishReason = "refusal"
		r.ToolCalls = nil
	case wire.Status == "incomplete":
		if wire.Incomplete.Reason != "max_output_tokens" {
			return nil, protocolCallError(ClassBadSchema, "incomplete Responses output", false)
		}
		r.FinishReason = "length"
		r.ToolCalls = nil
	default:
		calls, err := ExecutableToolCalls(parsedCalls)
		if err != nil {
			return nil, err
		}
		r.ToolCalls = calls
		if len(calls) > 0 {
			r.FinishReason = "tool_calls"
		} else if !finalText {
			return nil, protocolCallError(ClassBadSchema, "completed response has no final answer or function calls", false)
		}
	}
	return r, nil
}
