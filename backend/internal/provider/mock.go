package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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
	ScenarioOrchestration       MockScenario = "orchestration"
	ScenarioTraceability        MockScenario = "traceability"
	ScenarioHTTPTraceability    MockScenario = "http_traceability"
	ScenarioDiscovery           MockScenario = "discovery"
)

type MockProvider struct {
	mu         sync.Mutex
	scenario   MockScenario
	callCount  int
	delay      time.Duration
	onCallHook func(round int, messages []ChatMessage)
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

	if scenario == ScenarioOrchestration || scenario == ScenarioTraceability || scenario == ScenarioHTTPTraceability || m.hasTool(tools, "submit_plan") || m.hasTool(tools, "suggest_completion") || m.hasTool(tools, "record_observation") || m.hasTool(tools, "read_observations") || m.hasTool(tools, "get_endpoint") || m.hasTool(tools, "run_security_check") {
		return m.handleOrchestrationFlow(round, messages, tools)
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

func (m *MockProvider) hasTool(tools []ToolDefinition, name string) bool {
	for _, t := range tools {
		if t.Name == name {
			return true
		}
	}
	return false
}

func extractUUIDForAsset(text, key string) string {
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		if strings.Contains(l, "- Asset: ") && strings.Contains(l, key) && strings.Contains(l, "[id: ") {
			idx := strings.Index(l, "[id: ")
			sub := l[idx+len("[id: "):]
			endIdx := strings.Index(sub, "]")
			if endIdx != -1 {
				return strings.TrimSpace(sub[:endIdx])
			}
		}
	}
	target := "- Asset: " + key + " [id: "
	idx := strings.Index(text, target)
	if idx != -1 {
		sub := text[idx+len(target):]
		endIdx := strings.Index(sub, "]")
		if endIdx != -1 {
			return strings.TrimSpace(sub[:endIdx])
		}
	}
	return ""
}

func extractAssetIDFromPrompt(text string) string {
	if idx := strings.Index(text, "Asset ID: "); idx != -1 {
		rest := text[idx+len("Asset ID: "):]
		fields := strings.Fields(rest)
		if len(fields) > 0 {
			return strings.Trim(fields[0], " \r\n\t,;\"'")
		}
	}
	idx := strings.Index(text, `"asset_id":`)
	if idx != -1 {
		rest := text[idx+len(`"asset_id":`):]
		q1 := strings.Index(rest, `"`)
		if q1 != -1 {
			rest2 := rest[q1+1:]
			q2 := strings.Index(rest2, `"`)
			if q2 != -1 {
				cand := strings.TrimSpace(rest2[:q2])
				return cand
			}
		}
	}
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		if strings.Contains(l, "Asset") && strings.Contains(l, "[id: ") {
			idx := strings.Index(l, "[id: ")
			sub := l[idx+len("[id: "):]
			endIdx := strings.Index(sub, "]")
			if endIdx != -1 {
				return strings.TrimSpace(sub[:endIdx])
			}
		}
	}
	return ""
}

func extractJSONStringField(text, key string) string {
	target := `"` + key + `":`
	idx := strings.Index(text, target)
	if idx != -1 {
		rest := text[idx+len(target):]
		q1 := strings.Index(rest, `"`)
		if q1 != -1 {
			rest2 := rest[q1+1:]
			q2 := strings.Index(rest2, `"`)
			if q2 != -1 {
				return strings.TrimSpace(rest2[:q2])
			}
		}
	}
	return ""
}

func extractObservationID(text, keyword string) string {
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		if strings.Contains(l, keyword) && strings.Contains(l, "[obs_id: ") {
			idx := strings.Index(l, "[obs_id: ")
			sub := l[idx+len("[obs_id: "):]
			endIdx := strings.Index(sub, "]")
			if endIdx != -1 {
				return strings.TrimSpace(sub[:endIdx])
			}
		}
	}
	// Fallback: any observation ID
	for _, l := range lines {
		if strings.Contains(l, "[obs_id: ") {
			idx := strings.Index(l, "[obs_id: ")
			sub := l[idx+len("[obs_id: "):]
			endIdx := strings.Index(sub, "]")
			if endIdx != -1 {
				return strings.TrimSpace(sub[:endIdx])
			}
		}
	}
	return ""
}

type parsedAsset struct {
	ID   string
	Key  string
	Name string
}

func extractAssetsInScope(text string) []parsedAsset {
	var res []parsedAsset
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "- Asset: ") {
			idIdx := strings.Index(trimmed, "[id: ")
			if idIdx == -1 {
				continue
			}
			sub := trimmed[idIdx+len("[id: "):]
			endIdx := strings.Index(sub, "]")
			if endIdx == -1 {
				continue
			}
			id := strings.TrimSpace(sub[:endIdx])
			key := strings.TrimSpace(trimmed[len("- Asset: "):idIdx])

			name := ""
			if nameIdx := strings.Index(trimmed, "name: "); nameIdx != -1 {
				nameSub := trimmed[nameIdx+len("name: "):]
				if parenIdx := strings.Index(nameSub, ")"); parenIdx != -1 {
					name = strings.TrimSpace(nameSub[:parenIdx])
				}
			}
			res = append(res, parsedAsset{ID: id, Key: key, Name: name})
		}
	}
	return res
}

func extractExistingWorkItemKeys(text string) map[string]bool {
	keys := make(map[string]bool)
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "- WorkItem: ") {
			parts := strings.Fields(trimmed)
			if len(parts) >= 3 {
				keys[parts[2]] = true
			}
		}
	}
	return keys
}

func extractDiscoveryObservationID(text, assetID string) string {
	lines := strings.Split(text, "\n")
	for _, l := range lines {
		if strings.Contains(l, "discovered_links") && strings.Contains(l, assetID) && strings.Contains(l, "[obs_id: ") {
			idx := strings.Index(l, "[obs_id: ")
			sub := l[idx+len("[obs_id: "):]
			endIdx := strings.Index(sub, "]")
			if endIdx != -1 {
				return strings.TrimSpace(sub[:endIdx])
			}
		}
	}
	return ""
}

func workItemMetaForAsset(a parsedAsset) (key, title, objective string) {
	if strings.HasSuffix(a.Key, "/health") || strings.Contains(a.Name, "Health") {
		return "inspect_http_health", "Inspect HTTP Health", "Fetch health status endpoint"
	}
	if strings.HasSuffix(a.Key, "/login") || strings.Contains(a.Name, "Login") {
		return "inspect_http_login", "Inspect HTTP Login", "Fetch login page and extract links"
	}
	if strings.HasSuffix(a.Key, "/") || strings.Contains(a.Name, "Home") {
		return "inspect_http_home", "Inspect HTTP Home", "Fetch root page and extract links"
	}
	urlStr := strings.TrimPrefix(a.Key, "GET ")
	if u, err := url.Parse(urlStr); err == nil && u.Path != "" {
		clean := strings.Map(func(r rune) rune {
			if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
				return r
			}
			return '_'
		}, strings.Trim(u.Path, "/"))
		if clean == "" {
			clean = "root"
		}
		return "inspect_http_" + clean, "Inspect HTTP " + u.Path, "Fetch and inspect " + u.Path
	}
	slug := strings.ReplaceAll(strings.Trim(a.Key, "/"), "/", "_")
	if slug == "" {
		slug = a.ID[:8]
	}
	return "inspect_http_" + slug, "Inspect HTTP " + a.Key, "Fetch and inspect " + a.Key
}

func (m *MockProvider) handleOrchestrationFlow(round int, messages []ChatMessage, tools []ToolDefinition) (*ModelResponse, error) {
	fullPrompt := ""
	for _, msg := range messages {
		fullPrompt += msg.Content + "\n"
	}

	// 0. Independent Security Verifier flow for finding retests
	if strings.Contains(fullPrompt, "Independent Security Verifier") && m.hasTool(tools, "get_endpoint") {
		assetID := extractAssetIDFromPrompt(fullPrompt)
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool {
			callID := "call_retest_endpoint"
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       callID,
						Type:     "function",
						Function: FunctionCall{Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":"%s"}`, assetID)},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
				RequestID:    fmt.Sprintf("mock-retest-%d", round),
			}, nil
		}
		return &ModelResponse{Content: "Target response collected. The server must evaluate the stored verification conditions; this text is not a verdict.", FinishReason: "stop", RequestID: fmt.Sprintf("mock-retest-stop-%d", round)}, nil

	}

	// 1a. Worker inspect_http_endpoint flow
	if m.hasTool(tools, "get_endpoint") && m.hasTool(tools, "record_observation") {
		assetID := extractAssetIDFromPrompt(fullPrompt)
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool {
			callID := "call_get_http"
			if len(assetID) >= 8 {
				callID = fmt.Sprintf("call_get_%s", assetID[:8])
			}
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       callID,
						Type:     "function",
						Function: FunctionCall{Name: "get_endpoint", Arguments: fmt.Sprintf(`{"asset_id":"%s"}`, assetID)},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
				RequestID:    fmt.Sprintf("mock-worker-http-%d", round),
			}, nil
		}

		if strings.HasPrefix(lastMsg.ToolCallID, "call_get_") || lastMsg.ToolCallID == "call_get_http" {
			var resObj struct {
				Response struct {
					StatusCode int               `json:"status_code"`
					Headers    map[string]string `json:"headers"`
					Body       string            `json:"body"`
				} `json:"response"`
			}
			parseErr := json.Unmarshal([]byte(lastMsg.Content), &resObj)

			isHTML := false
			if parseErr == nil {
				for hk, hv := range resObj.Response.Headers {
					if strings.EqualFold(hk, "Content-Type") && strings.Contains(strings.ToLower(hv), "text/html") {
						isHTML = true
						break
					}
				}
				if !isHTML && (strings.Contains(resObj.Response.Body, "<html") || strings.Contains(resObj.Response.Body, "<a ")) {
					isHTML = true
				}
			}
			// Fallback: if JSON parse failed (e.g. truncated), scan raw string for Content-Type header.
			// The new structured output format puts "headers" before "body", so even under a
			// tight MaxToolOutputBytes the content-type field appears early.
			if !isHTML {
				rawLower := strings.ToLower(lastMsg.Content)
				if strings.Contains(rawLower, "text/html") ||
					strings.Contains(rawLower, "<!doctype html") ||
					strings.Contains(rawLower, "\\u003chtml") {
					isHTML = true
				}
			}

			// If worker has extract_endpoint_links tool and this is an HTML response, extract links first
			if m.hasTool(tools, "extract_endpoint_links") && isHTML {
				extractCallID := "call_extract_links"
				if len(assetID) >= 8 {
					extractCallID = fmt.Sprintf("call_extract_%s", assetID[:8])
				}
				return &ModelResponse{
					ToolCalls: []ToolCall{
						{
							ID:       extractCallID,
							Type:     "function",
							Function: FunctionCall{Name: "extract_endpoint_links", Arguments: fmt.Sprintf(`{"source_tool_call_id":"%s"}`, lastMsg.ToolCallID)},
						},
					},
					FinishReason: "tool_calls",
					Usage:        &ModelUsage{PromptTokens: 85, CompletionTokens: 25, TotalTokens: 110},
					RequestID:    fmt.Sprintf("mock-worker-extract-%d", round),
				}, nil
			}

			obsType := "http_response"
			content := fmt.Sprintf("HTTP Endpoint responded with status %d", resObj.Response.StatusCode)
			dataJSON := fmt.Sprintf(`{"status_code": %d, "asset_id": "%s"}`, resObj.Response.StatusCode, assetID)

			if strings.Contains(resObj.Response.Body, "version") {
				var healthBody struct {
					Version string `json:"version"`
				}
				if json.Unmarshal([]byte(resObj.Response.Body), &healthBody) == nil && healthBody.Version != "" {
					obsType = "service_version"
					content = fmt.Sprintf("Health check reported version %s", healthBody.Version)
					dataJSON = fmt.Sprintf(`{"status_code": %d, "version": "%s", "asset_id": "%s"}`, resObj.Response.StatusCode, healthBody.Version, assetID)
				}
			}

			obsArgs := fmt.Sprintf(`{
				"obs_type": "%s",
				"content": "%s",
				"data": %s,
				"source_tool_call_id": "%s",
				"asset_id": "%s"
			}`, obsType, content, dataJSON, lastMsg.ToolCallID, assetID)

			obsCallID := "call_obs_http"
			if len(assetID) >= 8 {
				obsCallID = fmt.Sprintf("call_obs_%s", assetID[:8])
			}

			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       obsCallID,
						Type:     "function",
						Function: FunctionCall{Name: "record_observation", Arguments: obsArgs},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120},
				RequestID:    fmt.Sprintf("mock-worker-http-%d", round),
			}, nil
		}

		if strings.HasPrefix(lastMsg.ToolCallID, "call_extract_") {
			var getToolCallID string
			var getBody string
			for i := len(messages) - 1; i >= 0; i-- {
				if strings.HasPrefix(messages[i].ToolCallID, "call_get_") || messages[i].ToolCallID == "call_get_http" {
					getToolCallID = messages[i].ToolCallID
					getBody = messages[i].Content
					break
				}
			}
			if getToolCallID == "" {
				getToolCallID = lastMsg.ToolCallID
			}

			var resObj struct {
				Response struct {
					StatusCode int    `json:"status_code"`
					Body       string `json:"body"`
				} `json:"response"`
			}
			_ = json.Unmarshal([]byte(getBody), &resObj)
			if resObj.Response.StatusCode == 0 {
				resObj.Response.StatusCode = 200
			}

			obsType := "http_response"
			content := fmt.Sprintf("HTTP Endpoint responded with status %d", resObj.Response.StatusCode)
			dataJSON := fmt.Sprintf(`{"status_code": %d, "asset_id": "%s"}`, resObj.Response.StatusCode, assetID)

			if strings.Contains(resObj.Response.Body, "version") {
				var healthBody struct {
					Version string `json:"version"`
				}
				if json.Unmarshal([]byte(resObj.Response.Body), &healthBody) == nil && healthBody.Version != "" {
					obsType = "service_version"
					content = fmt.Sprintf("Health check reported version %s", healthBody.Version)
					dataJSON = fmt.Sprintf(`{"status_code": %d, "version": "%s", "asset_id": "%s"}`, resObj.Response.StatusCode, healthBody.Version, assetID)
				}
			}

			obsArgs := fmt.Sprintf(`{
				"obs_type": "%s",
				"content": "%s",
				"data": %s,
				"source_tool_call_id": "%s",
				"asset_id": "%s"
			}`, obsType, content, dataJSON, getToolCallID, assetID)

			obsCallID := "call_obs_http"
			if len(assetID) >= 8 {
				obsCallID = fmt.Sprintf("call_obs_%s", assetID[:8])
			}

			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       obsCallID,
						Type:     "function",
						Function: FunctionCall{Name: "record_observation", Arguments: obsArgs},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120},
				RequestID:    fmt.Sprintf("mock-worker-http-%d", round),
			}, nil
		}

		return &ModelResponse{
			Content:      fmt.Sprintf("Successfully retrieved endpoint for asset %s and recorded evidence observation.", assetID),
			FinishReason: "stop",
			Usage:        &ModelUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
			RequestID:    fmt.Sprintf("mock-worker-http-%d", round),
		}, nil
	}

	// 1. Worker analyze_sample flow
	if m.hasTool(tools, "read_sample") && m.hasTool(tools, "record_observation") {
		sampleID := "sample_home"
		if strings.Contains(fullPrompt, "sample_login") {
			sampleID = "sample_login"
		} else if strings.Contains(fullPrompt, "sample_health") {
			sampleID = "sample_health"
		}

		isRecheck := strings.Contains(fullPrompt, "recheck_login_title")

		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool {
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       fmt.Sprintf("call_read_%s", sampleID),
						Type:     "function",
						Function: FunctionCall{Name: "read_sample", Arguments: fmt.Sprintf(`{"sample_id":"%s"}`, sampleID)},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
				RequestID:    fmt.Sprintf("mock-worker-round-%d", round),
			}, nil
		}

		if strings.HasPrefix(lastMsg.ToolCallID, "call_read_") {
			obsType := "http_status"
			content := "Endpoint returned status 200 OK"
			dataJSON := fmt.Sprintf(`{"sample_id": "%s", "status": 200}`, sampleID)
			if isRecheck {
				obsType = "page_title"
				content = "Confirmed login page title: Account Login"
				dataJSON = fmt.Sprintf(`{"sample_id": "%s", "title": "Account Login"}`, sampleID)
			}
			obsArgs := fmt.Sprintf(`{
				"obs_type": "%s",
				"content": "%s",
				"data": %s,
				"source_tool_call_id": "%s"
			}`, obsType, content, dataJSON, lastMsg.ToolCallID)
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       fmt.Sprintf("call_obs_%s", sampleID),
						Type:     "function",
						Function: FunctionCall{Name: "record_observation", Arguments: obsArgs},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 90, CompletionTokens: 30, TotalTokens: 120},
				RequestID:    fmt.Sprintf("mock-worker-round-%d", round),
			}, nil
		}

		return &ModelResponse{
			Content:      fmt.Sprintf("Successfully inspected %s and recorded evidence observation.", sampleID),
			FinishReason: "stop",
			Usage:        &ModelUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
			RequestID:    fmt.Sprintf("mock-worker-round-%d", round),
		}, nil
	}

	// 2. Worker aggregate_summary flow
	if m.hasTool(tools, "read_observations") {
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role != RoleTool {
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       "call_read_obs_all",
						Type:     "function",
						Function: FunctionCall{Name: "read_observations", Arguments: `{}`},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 90, CompletionTokens: 20, TotalTokens: 110},
				RequestID:    fmt.Sprintf("mock-agg-round-%d", round),
			}, nil
		}
		return &ModelResponse{
			Content:      "Analyzed all prerequisite observations. All endpoints healthy and returning 200 OK.",
			FinishReason: "stop",
			Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 30, TotalTokens: 150},
			RequestID:    fmt.Sprintf("mock-agg-round-%d", round),
		}, nil
	}

	// 2b. Worker run_security_check flow
	if m.hasTool(tools, "run_security_check") {
		lastMsg := messages[len(messages)-1]
		if lastMsg.Role == RoleTool {
			return &ModelResponse{
				Content:      "Security checks executed against persisted HTTP evidence. Results and candidate findings recorded.",
				FinishReason: "stop",
				Usage:        &ModelUsage{PromptTokens: 100, CompletionTokens: 25, TotalTokens: 125},
				RequestID:    fmt.Sprintf("mock-check-round-%d", round),
			}, nil
		}

		assetID := extractAssetIDFromPrompt(fullPrompt)
		checkID := extractJSONStringField(fullPrompt, "check_id")
		if checkID == "" {
			checkID = "http_security_headers"
		}

		argsJSON, _ := json.Marshal(map[string]string{
			"asset_id": assetID,
			"check_id": checkID,
		})

		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:       fmt.Sprintf("call_sec_check_%d", round),
					Type:     "function",
					Function: FunctionCall{Name: "run_security_check", Arguments: string(argsJSON)},
				},
			},
			FinishReason: "tool_calls",
			Usage:        &ModelUsage{PromptTokens: 90, CompletionTokens: 20, TotalTokens: 110},
			RequestID:    fmt.Sprintf("mock-check-round-%d", round),
		}, nil
	}

	// 3. Planner flows
	isProgressReview := strings.Contains(fullPrompt, "Current execution progress:")
	isDiscovery := m.scenario == ScenarioDiscovery ||
		strings.Contains(fullPrompt, `"mode": "discovery"`) ||
		strings.Contains(fullPrompt, `"mode":"discovery"`) ||
		strings.Contains(fullPrompt, "discovery_policy") ||
		strings.Contains(fullPrompt, "discovered_links")

	if isProgressReview {
		if isDiscovery {
			assetsInScope := extractAssetsInScope(fullPrompt)
			existingWIs := extractExistingWorkItemKeys(fullPrompt)

			var unscheduledItems []string
			var allAssetIDs []string
			var allWIKids []string

			for _, a := range assetsInScope {
				allAssetIDs = append(allAssetIDs, fmt.Sprintf(`"%s"`, a.ID))
				wiKey, title, obj := workItemMetaForAsset(a)
				allWIKids = append(allWIKids, fmt.Sprintf(`"%s"`, wiKey))
				if !existingWIs[wiKey] {
					srcObsID := extractDiscoveryObservationID(fullPrompt, a.ID)
					srcObsJSON := "[]"
					if srcObsID != "" {
						srcObsJSON = fmt.Sprintf(`["%s"]`, srcObsID)
					}
					itemJSON := fmt.Sprintf(`{
						"key": "%s",
						"title": "%s",
						"objective": "%s",
						"work_type": "inspect_http_endpoint",
						"input_data": {"asset_id": "%s"},
						"asset_ids": ["%s"],
						"source_observation_ids": %s
					}`, wiKey, title, obj, a.ID, a.ID, srcObsJSON)
					unscheduledItems = append(unscheduledItems, itemJSON)
				}
			}

			if len(unscheduledItems) > 0 && m.hasTool(tools, "submit_plan") {
				if len(messages) > 0 && messages[len(messages)-1].Role == RoleTool {
					return &ModelResponse{
						Content:      "Discovered endpoint inspection plan submitted.",
						FinishReason: "stop",
						Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
						RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
					}, nil
				}

				planArgs := fmt.Sprintf(`{
					"summary": "Schedule inspection for newly discovered endpoints",
					"items": [%s]
				}`, strings.Join(unscheduledItems, ", "))

				return &ModelResponse{
					ToolCalls: []ToolCall{
						{
							ID:       fmt.Sprintf("call_submit_plan_disc_%d", round),
							Type:     "function",
							Function: FunctionCall{Name: "submit_plan", Arguments: planArgs},
						},
					},
					FinishReason: "tool_calls",
					Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}

			// All assets in scope scheduled. Check if aggregate_summary scheduled.
			if !existingWIs["aggregate_summary"] && m.hasTool(tools, "submit_plan") {
				if len(messages) > 0 && messages[len(messages)-1].Role == RoleTool {
					return &ModelResponse{
						Content:      "Aggregate summary plan submitted.",
						FinishReason: "stop",
						Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
						RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
					}, nil
				}

				planArgs := fmt.Sprintf(`{
					"summary": "Plan aggregate summary across all inspected endpoints",
					"items": [
						{
							"key": "aggregate_summary",
							"title": "Aggregate HTTP Findings",
							"objective": "Summarize all discovered endpoints and observations",
							"work_type": "aggregate_summary",
							"dependencies": [%s],
							"asset_ids": [%s]
						}
					]
				}`, strings.Join(allWIKids, ", "), strings.Join(allAssetIDs, ", "))

				return &ModelResponse{
					ToolCalls: []ToolCall{
						{
							ID:       fmt.Sprintf("call_submit_plan_agg_%d", round),
							Type:     "function",
							Function: FunctionCall{Name: "submit_plan", Arguments: planArgs},
						},
					},
					FinishReason: "tool_calls",
					Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}

			// If aggregate_summary is done, call suggest_completion!
			if m.hasTool(tools, "suggest_completion") {
				if len(messages) > 0 && messages[len(messages)-1].Role == RoleTool {
					return &ModelResponse{
						Content:      "Task execution successfully completed with all verified findings.",
						FinishReason: "stop",
						Usage:        &ModelUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
						RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
					}, nil
				}

				compArgs := `{
					"conclusion": "complete",
					"final_summary": "In accordance with configured discovery policy, all reachable HTTP endpoints were discovered and inspected. Link discovery converged without further reachable endpoints."
				}`
				return &ModelResponse{
					ToolCalls: []ToolCall{
						{
							ID:       "call_suggest_completion_disc",
							Type:     "function",
							Function: FunctionCall{Name: "suggest_completion", Arguments: compArgs},
						},
					},
					FinishReason: "tool_calls",
					Usage:        &ModelUsage{PromptTokens: 140, CompletionTokens: 50, TotalTokens: 190},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}
		}

		// If scenario is Traceability and we haven't done recheck_login_title yet:
		hasRecheck := strings.Contains(fullPrompt, "recheck_login_title") || strings.Contains(fullPrompt, "Confirmed login page title")
		if m.scenario == ScenarioTraceability && !hasRecheck && m.hasTool(tools, "submit_plan") {
			if len(messages) > 0 && messages[len(messages)-1].Role == RoleTool {
				return &ModelResponse{
					Content:      "Follow-up verification plan submitted.",
					FinishReason: "stop",
					Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}

			loginAssetID := extractUUIDForAsset(fullPrompt, "sample_login")
			loginObsID := extractObservationID(fullPrompt, "sample_login")

			var assetIDsJSON, sourceObsIDsJSON string
			if loginAssetID != "" {
				assetIDsJSON = fmt.Sprintf(`["%s"]`, loginAssetID)
			} else {
				assetIDsJSON = `[]`
			}
			if loginObsID != "" {
				sourceObsIDsJSON = fmt.Sprintf(`["%s"]`, loginObsID)
			} else {
				sourceObsIDsJSON = `[]`
			}

			planArgs := fmt.Sprintf(`{
				"summary": "Follow-up verification for login page title",
				"items": [
					{
						"key": "recheck_login_title",
						"title": "Re-check Login Page Title",
						"objective": "Verify login page title from previous observation",
						"work_type": "analyze_sample",
						"input_data": {"sample_id": "sample_login"},
						"asset_ids": %s,
						"source_observation_ids": %s
					}
				]
			}`, assetIDsJSON, sourceObsIDsJSON)

			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       "call_submit_plan_recheck",
						Type:     "function",
						Function: FunctionCall{Name: "submit_plan", Arguments: planArgs},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
				RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
			}, nil
		}

		// If there are still active or pending work items, do not suggest completion.
		// Instead, acknowledge progress and let workers continue.
		if !strings.Contains(fullPrompt, "pending_or_running=0") && (strings.Contains(fullPrompt, "pending_or_running=") || strings.Contains(fullPrompt, "status: running") || strings.Contains(fullPrompt, "status: pending")) {
			return &ModelResponse{
				Content:      "Work items are currently in progress. Continuing to monitor execution.",
				FinishReason: "stop",
				Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
				RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
			}, nil
		}

		// Otherwise, suggest_completion
		if m.hasTool(tools, "suggest_completion") {
			if len(messages) > 0 && messages[len(messages)-1].Role == RoleTool {
				return &ModelResponse{
					Content:      "Task execution successfully completed with all verified findings.",
					FinishReason: "stop",
					Usage:        &ModelUsage{PromptTokens: 100, CompletionTokens: 20, TotalTokens: 120},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}

			if strings.Contains(fullPrompt, "inspect_http_") || strings.Contains(fullPrompt, "service_version") || strings.Contains(fullPrompt, "http_response") {
				healthObsID := extractObservationID(fullPrompt, "version")
				var refInfo string
				if healthObsID != "" {
					refInfo = fmt.Sprintf(" Evidence observation: %s.", healthObsID)
				}
				compArgs := fmt.Sprintf(`{
					"conclusion": "complete",
					"final_summary": "All target HTTP endpoints were verified via real GET requests. Verified real HTTP status codes and response bodies from local fixture application.%s"
				}`, refInfo)
				return &ModelResponse{
					ToolCalls: []ToolCall{
						{
							ID:       "call_suggest_completion_http",
							Type:     "function",
							Function: FunctionCall{Name: "suggest_completion", Arguments: compArgs},
						},
					},
					FinishReason: "tool_calls",
					Usage:        &ModelUsage{PromptTokens: 140, CompletionTokens: 50, TotalTokens: 190},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}

			loginObsID := extractObservationID(fullPrompt, "sample_login")
			var refInfo string
			if loginObsID != "" {
				refInfo = fmt.Sprintf(" Evidence observation: %s.", loginObsID)
			}
			compArgs := fmt.Sprintf(`{
				"conclusion": "complete",
				"final_summary": "All 3 HTTP response samples (sample_home, sample_login, sample_health) were verified. HTTP status 200 OK and secure headers confirmed across all endpoints.%s"
			}`, refInfo)
			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       "call_suggest_completion_1",
						Type:     "function",
						Function: FunctionCall{Name: "suggest_completion", Arguments: compArgs},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 140, CompletionTokens: 50, TotalTokens: 190},
				RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
			}, nil
		}
	}

	// Initial submit_plan flow (Round 1)
	if m.hasTool(tools, "submit_plan") {
		if len(messages) > 0 && messages[len(messages)-1].Role == RoleTool {
			return &ModelResponse{
				Content:      "Plan submitted successfully. Work items scheduled for execution.",
				FinishReason: "stop",
				Usage:        &ModelUsage{PromptTokens: 80, CompletionTokens: 20, TotalTokens: 100},
				RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
			}, nil
		}

		if isDiscovery {
			assetsInScope := extractAssetsInScope(fullPrompt)
			existingWIs := extractExistingWorkItemKeys(fullPrompt)

			var unscheduledItems []string
			for _, a := range assetsInScope {
				wiKey, title, obj := workItemMetaForAsset(a)
				if !existingWIs[wiKey] {
					itemJSON := fmt.Sprintf(`{
						"key": "%s",
						"title": "%s",
						"objective": "%s",
						"work_type": "inspect_http_endpoint",
						"input_data": {"asset_id": "%s"},
						"asset_ids": ["%s"]
					}`, wiKey, title, obj, a.ID, a.ID)
					unscheduledItems = append(unscheduledItems, itemJSON)
				}
			}

			if len(unscheduledItems) > 0 {
				planArgs := fmt.Sprintf(`{
					"summary": "Initial discovery plan to inspect seed endpoint",
					"items": [%s]
				}`, strings.Join(unscheduledItems, ", "))

				return &ModelResponse{
					ToolCalls: []ToolCall{
						{
							ID:       "call_submit_plan_seed",
							Type:     "function",
							Function: FunctionCall{Name: "submit_plan", Arguments: planArgs},
						},
					},
					FinishReason: "tool_calls",
					Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
					RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
				}, nil
			}
		}

		// Check for HTTP fixture assets in prompt
		rootHTTPID := extractUUIDForAsset(fullPrompt, "Local Fixture Home")
		if rootHTTPID == "" {
			rootHTTPID = extractUUIDForAsset(fullPrompt, "http_home")
		}
		if rootHTTPID == "" {
			rootHTTPID = extractUUIDForAsset(fullPrompt, "/")
		}
		loginHTTPID := extractUUIDForAsset(fullPrompt, "Local Fixture Login")
		if loginHTTPID == "" {
			loginHTTPID = extractUUIDForAsset(fullPrompt, "http_login")
		}
		if loginHTTPID == "" {
			loginHTTPID = extractUUIDForAsset(fullPrompt, "/login")
		}
		healthHTTPID := extractUUIDForAsset(fullPrompt, "Local Fixture Health Check")
		if healthHTTPID == "" {
			healthHTTPID = extractUUIDForAsset(fullPrompt, "http_health")
		}
		if healthHTTPID == "" {
			healthHTTPID = extractUUIDForAsset(fullPrompt, "/health")
		}

		if rootHTTPID != "" || loginHTTPID != "" || healthHTTPID != "" {
			var items []string
			var aggDeps []string
			var allAssetIDs []string

			if rootHTTPID != "" {
				items = append(items, fmt.Sprintf(`{"key": "inspect_http_home", "title": "Inspect HTTP Home", "objective": "Fetch root page", "work_type": "inspect_http_endpoint", "input_data": {"asset_id": "%s"}, "asset_ids": ["%s"]}`, rootHTTPID, rootHTTPID))
				aggDeps = append(aggDeps, `"inspect_http_home"`)
				allAssetIDs = append(allAssetIDs, fmt.Sprintf(`"%s"`, rootHTTPID))
			}
			if loginHTTPID != "" {
				items = append(items, fmt.Sprintf(`{"key": "inspect_http_login", "title": "Inspect HTTP Login", "objective": "Fetch login page", "work_type": "inspect_http_endpoint", "input_data": {"asset_id": "%s"}, "asset_ids": ["%s"]}`, loginHTTPID, loginHTTPID))
				aggDeps = append(aggDeps, `"inspect_http_login"`)
				allAssetIDs = append(allAssetIDs, fmt.Sprintf(`"%s"`, loginHTTPID))
			}
			if healthHTTPID != "" {
				items = append(items, fmt.Sprintf(`{"key": "inspect_http_health", "title": "Inspect HTTP Health", "objective": "Fetch health status", "work_type": "inspect_http_endpoint", "input_data": {"asset_id": "%s"}, "asset_ids": ["%s"]}`, healthHTTPID, healthHTTPID))
				aggDeps = append(aggDeps, `"inspect_http_health"`)
				allAssetIDs = append(allAssetIDs, fmt.Sprintf(`"%s"`, healthHTTPID))
			}

			items = append(items, fmt.Sprintf(`{"key": "aggregate_summary", "title": "Aggregate HTTP Findings", "objective": "Summarize observations", "work_type": "aggregate_summary", "dependencies": [%s], "asset_ids": [%s]}`, strings.Join(aggDeps, ", "), strings.Join(allAssetIDs, ", ")))

			planArgs := fmt.Sprintf(`{
				"summary": "Plan to inspect all configured HTTP fixture endpoints and aggregate observations",
				"items": [%s]
			}`, strings.Join(items, ", "))

			return &ModelResponse{
				ToolCalls: []ToolCall{
					{
						ID:       "call_submit_plan_http",
						Type:     "function",
						Function: FunctionCall{Name: "submit_plan", Arguments: planArgs},
					},
				},
				FinishReason: "tool_calls",
				Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
				RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
			}, nil
		}

		homeAssetID := extractUUIDForAsset(fullPrompt, "sample_home")
		loginAssetID := extractUUIDForAsset(fullPrompt, "sample_login")
		healthAssetID := extractUUIDForAsset(fullPrompt, "sample_health")

		var planArgs string
		if homeAssetID != "" && loginAssetID != "" && healthAssetID != "" {
			planArgs = fmt.Sprintf(`{
				"summary": "Plan to analyze all 3 HTTP samples and synthesize findings",
				"items": [
					{"key": "analyze_home", "title": "Analyze Home Sample", "objective": "Read and observe sample_home", "work_type": "analyze_sample", "input_data": {"sample_id": "sample_home"}, "asset_ids": ["%s"]},
					{"key": "analyze_login", "title": "Analyze Login Sample", "objective": "Read and observe sample_login", "work_type": "analyze_sample", "input_data": {"sample_id": "sample_login"}, "asset_ids": ["%s"]},
					{"key": "analyze_health", "title": "Analyze Health Sample", "objective": "Read and observe sample_health", "work_type": "analyze_sample", "input_data": {"sample_id": "sample_health"}, "asset_ids": ["%s"]},
					{"key": "aggregate_summary", "title": "Aggregate Findings", "objective": "Summarize all observations", "work_type": "aggregate_summary", "dependencies": ["analyze_home", "analyze_login", "analyze_health"], "asset_ids": ["%s", "%s", "%s"]}
				]
			}`, homeAssetID, loginAssetID, healthAssetID, homeAssetID, loginAssetID, healthAssetID)
		} else {
			planArgs = `{
				"summary": "Plan to analyze all 3 HTTP samples and synthesize findings",
				"items": [
					{"key": "analyze_home", "title": "Analyze Home Sample", "objective": "Read and observe sample_home", "work_type": "analyze_sample", "input_data": {"sample_id": "sample_home"}},
					{"key": "analyze_login", "title": "Analyze Login Sample", "objective": "Read and observe sample_login", "work_type": "analyze_sample", "input_data": {"sample_id": "sample_login"}},
					{"key": "analyze_health", "title": "Analyze Health Sample", "objective": "Read and observe sample_health", "work_type": "analyze_sample", "input_data": {"sample_id": "sample_health"}},
					{"key": "aggregate_summary", "title": "Aggregate Findings", "objective": "Summarize all observations", "work_type": "aggregate_summary", "dependencies": ["analyze_home", "analyze_login", "analyze_health"]}
				]
			}`
		}

		return &ModelResponse{
			ToolCalls: []ToolCall{
				{
					ID:       "call_submit_plan_1",
					Type:     "function",
					Function: FunctionCall{Name: "submit_plan", Arguments: planArgs},
				},
			},
			FinishReason: "tool_calls",
			Usage:        &ModelUsage{PromptTokens: 120, CompletionTokens: 40, TotalTokens: 160},
			RequestID:    fmt.Sprintf("mock-planner-round-%d", round),
		}, nil
	}

	return m.handleNormalFlow(round, messages)
}
