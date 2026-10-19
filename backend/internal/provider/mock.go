package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

var (
	ErrMockProtocolMismatch = errors.New("mock provider: previous tool results missing or ID mismatch")
	ErrMockSimulatedFailure = errors.New("mock provider: simulated upstream failure")
)

type MockScenario string

const (
	ScenarioNormal              MockScenario = "normal"
	ScenarioTextAndTool         MockScenario = "text_and_tool"
	ScenarioInvalidTool         MockScenario = "invalid_tool"
	ScenarioInvalidArgs         MockScenario = "invalid_args"
	ScenarioDuplicateToolCallID MockScenario = "duplicate_tool_call_id"
	ScenarioMissingToolCallID   MockScenario = "missing_tool_call_id"
	ScenarioInfiniteTools       MockScenario = "infinite_tools"
	ScenarioEmptyResponse       MockScenario = "empty_response"
	ScenarioRefusal             MockScenario = "refusal"
	ScenarioTruncated           MockScenario = "truncated"
	ScenarioProviderError       MockScenario = "provider_error"
	ScenarioDelay               MockScenario = "delay"
)

type MockProvider struct {
	mu           sync.Mutex
	scenario     MockScenario
	callCount    int
	delay        time.Duration
	onCallHook   func(round int, messages []ChatMessage)
}

func NewMockProvider(scenario MockScenario) *MockProvider {
	if scenario == "" {
		scenario = ScenarioNormal
	}
	return &MockProvider{
		scenario: scenario,
	}
}

func (m *MockProvider) SetDelay(d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.delay = d
}

func (m *MockProvider) SetOnCallHook(hook func(round int, messages []ChatMessage)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onCallHook = hook
}

func (m *MockProvider) Call(ctx context.Context, messages []ChatMessage, tools []ToolDefinition, opts ModelOpts) (*ModelResponse, error) {
	m.mu.Lock()
	m.callCount++
	round := m.callCount
	scenario := m.scenario
	delay := m.delay
	hook := m.onCallHook
	m.mu.Unlock()

	if hook != nil {
		hook(round, messages)
	}

	if delay > 0 {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
	}

	switch scenario {
	case ScenarioProviderError:
		return nil, ErrMockSimulatedFailure

	case ScenarioDelay:
		// Wait until cancelled
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		}

	case ScenarioEmptyResponse:
		return &ModelResponse{
			Content:      "",
			FinishReason: "stop",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioRefusal:
		return &ModelResponse{
			Content:      "I refuse to execute this task due to policy constraints.",
			FinishReason: "refusal",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioTruncated:
		return &ModelResponse{
			Content:      "Summary of findings: [output cut off due to token limit...",
			FinishReason: "length",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioDuplicateToolCallID:
		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:   "call_dup_1",
					Type: "function",
					Function: FunctionCall{
						Name:      "read_sample",
						Arguments: `{"sample_id":"sample_home"}`,
					},
				},
				{
					ID:   "call_dup_1", // Duplicate ID in same batch!
					Type: "function",
					Function: FunctionCall{
						Name:      "read_sample",
						Arguments: `{"sample_id":"sample_login"}`,
					},
				},
			},
			FinishReason: "tool_calls",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioMissingToolCallID:
		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:   "", // Empty ID!
					Type: "function",
					Function: FunctionCall{
						Name:      "list_samples",
						Arguments: `{}`,
					},
				},
			},
			FinishReason: "tool_calls",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioInfiniteTools:
		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:   fmt.Sprintf("call_infinite_%d", round),
					Type: "function",
					Function: FunctionCall{
						Name:      "list_samples",
						Arguments: `{}`,
					},
				},
			},
			FinishReason: "tool_calls",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioInvalidTool:
		if round == 1 {
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:   "call_invalid_tool_1",
						Type: "function",
						Function: FunctionCall{
							Name:      "nonexistent_tool_xyz",
							Arguments: `{"foo":"bar"}`,
						},
					},
				},
				FinishReason: "tool_calls",
				RequestID:    fmt.Sprintf("mock-req-%d", round),
			}, nil
		}
		// In round 2, verify error was returned
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool || lastMsg.ToolCallID != "call_invalid_tool_1" {
			return nil, fmt.Errorf("%w: expected tool response for call_invalid_tool_1, got role %s", ErrMockProtocolMismatch, lastMsg.Role)
		}
		return &ModelResponse{
			Content:      "Observed error: nonexistent_tool_xyz does not exist. Recovering.",
			FinishReason: "stop",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioInvalidArgs:
		if round == 1 {
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:   "call_invalid_args_1",
						Type: "function",
						Function: FunctionCall{
							Name:      "read_sample",
							Arguments: `{"sample_id":"invalid_sample_404"}`,
						},
					},
				},
				FinishReason: "tool_calls",
				RequestID:    fmt.Sprintf("mock-req-%d", round),
			}, nil
		}
		// In round 2, verify rejection error returned
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool || lastMsg.ToolCallID != "call_invalid_args_1" {
			return nil, fmt.Errorf("%w: expected tool response for call_invalid_args_1, got role %s", ErrMockProtocolMismatch, lastMsg.Role)
		}
		return &ModelResponse{
			Content:      "Observed validation rejection for invalid_sample_404. Task finished.",
			FinishReason: "stop",
			RequestID:    fmt.Sprintf("mock-req-%d", round),
		}, nil

	case ScenarioTextAndTool:
		if round == 1 {
			return &ModelResponse{
				Content: "I am preparing to list all available samples.",
				ToolCalls: []ToolCall{
					{
						ID:   "call_list_1",
						Type: "function",
						Function: FunctionCall{
							Name:      "list_samples",
							Arguments: `{}`,
						},
					},
				},
				FinishReason: "tool_calls",
				RequestID:    fmt.Sprintf("mock-req-%d", round),
			}, nil
		}
		// Subsequent rounds follow normal
		return m.handleNormalFlow(round, messages)

	case ScenarioNormal:
		fallthrough
	default:
		return m.handleNormalFlow(round, messages)
	}
}

func (m *MockProvider) handleNormalFlow(round int, messages []ChatMessage) (*ModelResponse, error) {
	switch round {
	case 1:
		// Round 1: Request list_samples
		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:   "call_list_1",
					Type: "function",
					Function: FunctionCall{
						Name:      "list_samples",
						Arguments: `{}`,
					},
				},
			},
			FinishReason: "tool_calls",
			Usage: &ModelUsage{
				PromptTokens:     100,
				CompletionTokens: 20,
				TotalTokens:      120,
			},
			RequestID: "mock-round-1",
		}, nil

	case 2:
		// Round 2: Verify list_samples tool result is present
		if len(messages) < 3 {
			return nil, fmt.Errorf("%w: expected at least 3 messages in round 2, got %d", ErrMockProtocolMismatch, len(messages))
		}
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool || lastMsg.ToolCallID != "call_list_1" {
			return nil, fmt.Errorf("%w: expected tool result for call_list_1, got role %s, tool_call_id %s",
				ErrMockProtocolMismatch, lastMsg.Role, lastMsg.ToolCallID)
		}

		// Request reading all 3 samples in one batch
		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:   "call_read_home",
					Type: "function",
					Function: FunctionCall{
						Name:      "read_sample",
						Arguments: `{"sample_id":"sample_home"}`,
					},
				},
				{
					ID:   "call_read_login",
					Type: "function",
					Function: FunctionCall{
						Name:      "read_sample",
						Arguments: `{"sample_id":"sample_login"}`,
					},
				},
				{
					ID:   "call_read_health",
					Type: "function",
					Function: FunctionCall{
						Name:      "read_sample",
						Arguments: `{"sample_id":"sample_health"}`,
					},
				},
			},
			FinishReason: "tool_calls",
			Usage: &ModelUsage{
				PromptTokens:     250,
				CompletionTokens: 60,
				TotalTokens:      310,
			},
			RequestID: "mock-round-2",
		}, nil

	case 3:
		// Round 3: Verify all 3 tool results are present in order
		expectedIDs := []string{"call_read_home", "call_read_login", "call_read_health"}
		// Last 3 messages must be tools matching expectedIDs
		if len(messages) < len(expectedIDs) {
			return nil, fmt.Errorf("%w: expected at least %d messages, got %d", ErrMockProtocolMismatch, len(expectedIDs), len(messages))
		}

		startIndex := len(messages) - len(expectedIDs)
		for i, expectedID := range expectedIDs {
			msg := messages[startIndex+i]
			if msg.Role != RoleTool {
				return nil, fmt.Errorf("%w: message at index %d expected role 'tool', got '%s'", ErrMockProtocolMismatch, startIndex+i, msg.Role)
			}
			if msg.ToolCallID != expectedID {
				return nil, fmt.Errorf("%w: message at index %d expected tool_call_id '%s', got '%s'", ErrMockProtocolMismatch, startIndex+i, expectedID, msg.ToolCallID)
			}
			if strings.TrimSpace(msg.Content) == "" {
				return nil, fmt.Errorf("%w: message at index %d has empty content", ErrMockProtocolMismatch, startIndex+i)
			}
		}

		// Return final structured report
		finalReport := "HTTP Sample Analysis Report:\n" +
			"- sample_home: HTTP 200 OK, Title: 'Company Portal Home' (source: sample_home)\n" +
			"- sample_login: HTTP 200 OK, Title: 'Account Login' (source: sample_login)\n" +
			"- sample_health: HTTP 200 OK, Title: 'System Health Status' (source: sample_health)\n\n" +
			"Observation: All 3 registered endpoints responded with HTTP status 200. No authentication failures detected."

		return &ModelResponse{
			Content:      finalReport,
			FinishReason: "stop",
			Usage: &ModelUsage{
				PromptTokens:     500,
				CompletionTokens: 110,
				TotalTokens:      610,
			},
			RequestID: "mock-round-3",
		}, nil

	default:
		return &ModelResponse{
			Content:      "Execution concluded.",
			FinishReason: "stop",
			RequestID:    fmt.Sprintf("mock-round-%d", round),
		}, nil
	}
}
