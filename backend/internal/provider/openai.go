package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrModelConfigMissing = errors.New("real model configuration missing: LLM_API_KEY, LLM_BASE_URL, or LLM_MODEL not configured")
	ErrOpenAIUpstream     = errors.New("upstream model provider returned an error")
)

type OpenAIProvider struct {
	baseURL      string
	apiKey       string
	defaultModel string
	client       *http.Client
}

func NewOpenAIProvider(baseURL, apiKey, defaultModel string, client *http.Client) *OpenAIProvider {
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	baseURL = strings.TrimRight(baseURL, "/")
	return &OpenAIProvider{
		baseURL:      baseURL,
		apiKey:       apiKey,
		defaultModel: defaultModel,
		client:       client,
	}
}

// Request and Response JSON structures for OpenAI Chat Completions API
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    *string          `json:"content,omitempty"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIToolCall struct {
	ID       string                 `json:"id"`
	Type     string                 `json:"type"` // "function"
	Function openAIFunctionCallData `json:"function"`
}

type openAIFunctionCallData struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type openAIToolItem struct {
	Type     string             `json:"type"` // "function"
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type openAIChatRequest struct {
	Model      string           `json:"model"`
	Messages   []openAIMessage  `json:"messages"`
	Tools      []openAIToolItem `json:"tools,omitempty"`
	ToolChoice string           `json:"tool_choice,omitempty"`
}

type openAIChatResponse struct {
	ID      string `json:"id"`
	Choices []struct {
		Index        int           `json:"index"`
		Message      openAIMessage `json:"message"`
		FinishReason string        `json:"finish_reason"`
	} `json:"choices"`
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

func (p *OpenAIProvider) Call(ctx context.Context, messages []ChatMessage, tools []ToolDefinition, opts ModelOpts) (*ModelResponse, error) {
	modelName := opts.ModelName
	if modelName == "" {
		modelName = p.defaultModel
	}

	reqMessages := make([]openAIMessage, 0, len(messages))
	for _, m := range messages {
		var contentPtr *string
		if m.Content != "" || len(m.ToolCalls) == 0 {
			c := m.Content
			contentPtr = &c
		}

		om := openAIMessage{
			Role:       string(m.Role),
			Content:    contentPtr,
			ToolCallID: m.ToolCallID,
		}

		if len(m.ToolCalls) > 0 {
			om.ToolCalls = make([]openAIToolCall, 0, len(m.ToolCalls))
			for _, tc := range m.ToolCalls {
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIFunctionCallData{
						Name:      tc.Function.Name,
						Arguments: tc.Function.Arguments,
					},
				})
			}
		}
		reqMessages = append(reqMessages, om)
	}

	chatReq := openAIChatRequest{
		Model:    modelName,
		Messages: reqMessages,
	}

	if len(tools) > 0 {
		chatReq.Tools = make([]openAIToolItem, 0, len(tools))
		for _, t := range tools {
			chatReq.Tools = append(chatReq.Tools, openAIToolItem{
				Type: "function",
				Function: openAIToolFunction{
					Name:        t.Name,
					Description: t.Description,
					Parameters:  t.Parameters,
				},
			})
		}
		chatReq.ToolChoice = "auto"
	}

	reqBody, err := json.Marshal(chatReq)
	if err != nil {
		return nil, fmt.Errorf("marshal chat request: %w", err)
	}

	url := fmt.Sprintf("%s/chat/completions", p.baseURL)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("create http request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	if p.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+p.apiKey)
	}

	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrOpenAIUpstream, err)
	}
	defer resp.Body.Close()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response body: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var errResp openAIChatResponse
		if jsonErr := json.Unmarshal(respBytes, &errResp); jsonErr == nil && errResp.Error != nil {
			return nil, fmt.Errorf("%w: HTTP %d: %s", ErrOpenAIUpstream, resp.StatusCode, errResp.Error.Message)
		}
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrOpenAIUpstream, resp.StatusCode, string(respBytes))
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		return nil, fmt.Errorf("unmarshal chat response: %w", err)
	}

	if len(chatResp.Choices) == 0 {
		return nil, fmt.Errorf("invalid response: no choices returned from provider")
	}

	choice := chatResp.Choices[0]
	var content string
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}

	var parsedToolCalls []ToolCall
	if len(choice.Message.ToolCalls) > 0 {
		parsedToolCalls = make([]ToolCall, 0, len(choice.Message.ToolCalls))
		for _, tc := range choice.Message.ToolCalls {
			parsedToolCalls = append(parsedToolCalls, ToolCall{
				ID:   tc.ID,
				Type: tc.Type,
				Function: FunctionCall{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments,
				},
			})
		}
	}

	var usage *ModelUsage
	if chatResp.Usage != nil {
		usage = &ModelUsage{
			PromptTokens:     chatResp.Usage.PromptTokens,
			CompletionTokens: chatResp.Usage.CompletionTokens,
			TotalTokens:      chatResp.Usage.TotalTokens,
		}
	}

	return &ModelResponse{
		Content:      content,
		ToolCalls:    parsedToolCalls,
		FinishReason: choice.FinishReason,
		Usage:        usage,
		RequestID:    resp.Header.Get("x-request-id"),
		ResponseID:   chatResp.ID,
	}, nil
}
