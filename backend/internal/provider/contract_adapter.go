package provider

import (
	"context"
	"encoding/json"
	"errors"

	"agent-workbench/backend/internal/contract"
)

// ContractCaller adapts a stateless Provider to contract.ModelCaller.
// It does not store messages, previous_response_id, or retry a call.
type ContractCaller struct {
	Provider Provider
	Protocol string
}

func (c *ContractCaller) Call(ctx context.Context, req contract.ModelCallRequest) (*contract.ModelCallResult, error) {
	if c == nil || c.Provider == nil {
		return nil, contract.ErrNotImplemented
	}
	messages := make([]ChatMessage, len(req.Messages))
	for i, message := range req.Messages {
		messages[i] = ChatMessage{Role: ChatRole(message.Role), Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			messages[i].ToolCalls = append(messages[i].ToolCalls, ToolCall{
				ID:       call.ID,
				Type:     "function",
				Function: FunctionCall{Name: call.Name, Arguments: call.Arguments},
			})
		}
	}
	tools := make([]ToolDefinition, len(req.Tools))
	for i, tool := range req.Tools {
		tools[i] = ToolDefinition{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters}
	}
	protocol := req.Opts.Protocol
	if protocol == "" {
		protocol = c.Protocol
	}
	resp, err := c.Provider.Call(ctx, messages, tools, ModelOpts{
		ModelName: req.Opts.ModelName,
		Timeout:   req.Opts.Timeout,
	})
	result := resultFromProvider(resp, err, protocol)
	if err != nil {
		return result, contractErrorFrom(err)
	}
	return result, nil
}

func resultFromProvider(resp *ModelResponse, err error, protocol string) *contract.ModelCallResult {
	result := &contract.ModelCallResult{Executable: false, FinishReason: "error"}
	if resp != nil {
		result.Content = resp.Content
		result.FinishReason = resp.FinishReason
		result.ProviderRequestID = resp.RequestID
		if resp.Usage != nil {
			result.Usage = &contract.ModelUsage{
				PromptTokens:     resp.Usage.PromptTokens,
				CompletionTokens: resp.Usage.CompletionTokens,
				TotalTokens:      resp.Usage.TotalTokens,
			}
		}
	}
	if err != nil || resp == nil {
		result.ToolCalls = nil
		result.ProtocolError = contractBody(contractErrorFrom(err))
		result.Executable = false
		return result
	}
	if resp.FinishReason == "length" || resp.FinishReason == "refusal" {
		result.ToolCalls = nil
		result.Executable = false
		return result
	}
	for _, call := range resp.ToolCalls {
		encoded, marshalErr := json.Marshal(call.Function.Arguments)
		if call.ID == "" || marshalErr != nil {
			result.ToolCalls = nil
			result.Executable = false
			result.ProtocolError = &contract.ErrorBody{
				Code:            contract.CodeInvalidModelOutput,
				Scope:           contract.ScopeSession,
				Message:         "incomplete function arguments or status",
				Retryable:       true,
				SuggestedAction: contract.ActionRetryDirection,
				SourceID:        resp.RequestID,
			}
			return result
		}
		if _, argsErr := NormalizeArguments(encoded); argsErr != nil {
			result.ToolCalls = nil
			result.Executable = false
			result.ProtocolError = &contract.ErrorBody{
				Code:            contract.CodeInvalidModelOutput,
				Scope:           contract.ScopeSession,
				Message:         "incomplete function arguments or status",
				Retryable:       true,
				SuggestedAction: contract.ActionRetryDirection,
				SourceID:        resp.RequestID,
			}
			return result
		}
		result.ToolCalls = append(result.ToolCalls, contract.ModelToolCall{
			ID:        call.ID,
			Name:      call.Function.Name,
			Arguments: call.Function.Arguments,
			Status:    "completed",
		})
	}
	result.Executable = resp.FinishReason == "stop" || resp.FinishReason == "tool_calls" || len(result.ToolCalls) > 0
	if protocol == FormatResponses && resp.FinishReason == "stop" && resp.Content == "" && len(result.ToolCalls) == 0 {
		result.Executable = false
	}
	return result
}

func contractErrorFrom(err error) error {
	if err == nil {
		return &contract.Error{Code: contract.CodeInvalidModelOutput, Scope: contract.ScopeSession, Message: "provider returned no result", Retryable: true}
	}
	var existing *contract.Error
	if errors.As(err, &existing) {
		return existing
	}
	var ce *CallError
	if !errors.As(err, &ce) {
		return err
	}
	out := &contract.Error{
		Scope:      contract.ScopeSession,
		Message:    ce.Error(),
		Retryable:  ce.Retryable,
		SourceID:   firstNonEmpty(ce.SourceModelCallID, ce.RequestID),
		HTTPStatus: ce.HTTPStatus,
	}
	switch ce.Class {
	case ClassRateLimited:
		out.Code = contract.CodeUpstreamRateLimited
		out.SuggestedAction = contract.ActionWait
	case ClassUpstream, ClassTransport, ClassTimeout:
		out.Code = contract.CodeExecutionUnavailable
		if ce.Class == ClassTimeout && out.HTTPStatus == 0 {
			out.HTTPStatus = 504
		}
	case ClassIncomplete, ClassDuplicate, ClassMissingCall, ClassBadSchema, ClassOutputLimit:
		out.Code = contract.CodeInvalidModelOutput
		out.SuggestedAction = contract.ActionRetryDirection
	default:
		out.Code = contract.CodeInvalidModelOutput
	}
	return out
}

func contractBody(err error) *contract.ErrorBody {
	var ce *contract.Error
	if !errors.As(err, &ce) || ce == nil {
		return &contract.ErrorBody{Code: contract.CodeInvalidModelOutput, Scope: contract.ScopeSession, Message: "model output is not executable", Retryable: true}
	}
	body := ce.Envelope().Error
	return &body
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
