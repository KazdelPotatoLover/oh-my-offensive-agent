package provider

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
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
	MaxTokens  int              `json:"max_tokens,omitempty"`
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
				encodedArgs, err := json.Marshal(tc.Function.Arguments)
				if err != nil {
					return nil, fmt.Errorf("marshal tool arguments: %w", err)
				}
				om.ToolCalls = append(om.ToolCalls, openAIToolCall{
					ID:   tc.ID,
					Type: "function",
					Function: openAIFunctionCallData{
						Name:      tc.Function.Name,
						Arguments: encodedArgs,
					},
				})
			}
		}
		reqMessages = append(reqMessages, om)
	}

	maxTokens, err := BoundOutputTokens(opts.MaxOutputTokens)
	if err != nil {
		return nil, err
	}
	chatReq := openAIChatRequest{
		Model:     modelName,
		Messages:  reqMessages,
		MaxTokens: maxTokens,
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

	if opts.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, opts.Timeout)
		defer cancel()
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
		return nil, transportCallError(err)
	}
	defer resp.Body.Close()

	respBytes, err := readLimitedBody(resp.Body)
	if err != nil {
		return nil, err
	}
	requestID := boundRequestID(resp.Header.Get("x-request-id"))
	secrets := messageSecrets(p.apiKey, messages)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &ModelResponse{RequestID: requestID}, httpCallError(resp.StatusCode, requestID, resp.Header.Get("Retry-After"), respBytes, secrets)
	}

	var chatResp openAIChatResponse
	if err := json.Unmarshal(respBytes, &chatResp); err != nil {
		return &ModelResponse{RequestID: requestID}, annotateDiagnosis(protocolCallError(ClassBadSchema, "invalid chat JSON", true), resp.StatusCode, requestID, respBytes)
	}
	if len(chatResp.Choices) == 0 {
		return &ModelResponse{RequestID: requestID, ResponseID: chatResp.ID}, annotateDiagnosis(protocolCallError(ClassBadSchema, "chat response has no choices", true), resp.StatusCode, requestID, respBytes)
	}

	choice := chatResp.Choices[0]
	var content string
	if choice.Message.Content != nil {
		content = *choice.Message.Content
	}
	parsed, classErr := classifyChatToolCalls(choice.FinishReason, choice.Message.ToolCalls)
	if classErr != nil {
		return &ModelResponse{RequestID: requestID, ResponseID: chatResp.ID, Content: content, FinishReason: choice.FinishReason}, annotateDiagnosis(classErr, resp.StatusCode, requestID, respBytes)
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
		ToolCalls:    parsed,
		FinishReason: normalizeChatFinish(choice.FinishReason, parsed),
		Usage:        usage,
		RequestID:    requestID,
		ResponseID:   chatResp.ID,
	}, nil
}

func classifyChatToolCalls(finish string, calls []openAIToolCall) ([]ToolCall, error) {
	if finish == "length" || finish == "content_filter" || finish == "refusal" {
		return nil, nil
	}
	parsed := make([]parsedFunctionCall, 0, len(calls))
	for _, call := range calls {
		args, err := NormalizeArguments(call.Function.Arguments)
		parsed = append(parsed, parsedFunctionCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: args,
			Status:    "completed",
			ArgsOK:    err == nil,
		})
	}
	return ExecutableToolCalls(parsed)
}

func normalizeChatFinish(finish string, calls []ToolCall) string {
	switch finish {
	case "content_filter", "refusal":
		return "refusal"
	case "length":
		return "length"
	case "":
		if len(calls) > 0 {
			return "tool_calls"
		}
		return "stop"
	default:
		return finish
	}
}
