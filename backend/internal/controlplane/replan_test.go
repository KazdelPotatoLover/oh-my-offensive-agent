package controlplane

import (
	"encoding/json"
	"strings"
	"testing"

	"agent-workbench/backend/internal/provider"

	"github.com/google/uuid"
)

func TestCloseUnexecutedBatchKeepsExecutedResults(t *testing.T) {
	instruction := "只读取授权入口"
	assistant := provider.ChatMessage{
		Role: provider.RoleAssistant,
		ToolCalls: []provider.ToolCall{
			{ID: "call_done", Function: provider.FunctionCall{Name: "read_artifact"}},
			{ID: "call_pending", Function: provider.FunctionCall{Name: "http_request"}},
		},
		NativePayload: &provider.NativePayload{
			Format: "responses",
			Output: []json.RawMessage{
				json.RawMessage(`{"type":"function_call","call_id":"call_done","name":"read_artifact"}`),
				json.RawMessage(`{"type":"function_call","call_id":"call_pending","name":"http_request"}`),
			},
		},
	}
	decision := ToolBatchDecision{
		CloseUnexecuted: true,
		Instruction:     instruction,
		ClosedCalls: []ClosedToolCall{{
			ID:     "call_pending",
			Name:   "http_request",
			Result: closedToolResult(instruction),
		}},
	}
	closed := CloseUnexecutedBatch(assistant, 1, decision)
	if len(closed) != 2 {
		t.Fatalf("closed messages = %d, want tool result then steer guidance", len(closed))
	}
	if closed[0].Role != provider.RoleTool || closed[0].ToolCallID != "call_pending" {
		t.Fatalf("first close message = %+v", closed[0])
	}
	if !strings.Contains(closed[0].Content, instruction) || strings.Contains(closed[0].Content, "executed http_request") {
		t.Fatalf("closed result = %q", closed[0].Content)
	}
	if closed[1].Role != provider.RoleUser || !strings.HasPrefix(closed[1].Content, "[Operator Steer Guidance]\n") {
		t.Fatalf("steer guidance = %+v", closed[1])
	}
	if len(assistant.ToolCalls) != 2 || assistant.NativePayload == nil || len(assistant.NativePayload.Output) != 2 {
		t.Fatalf("closer rewrote the assistant message")
	}

	transcript := []provider.ChatMessage{
		{Role: provider.RoleUser, Content: "continue"},
		assistant,
		{Role: provider.RoleTool, ToolCallID: "call_done", Content: "already executed"},
	}
	transcript = append(transcript, closed...)
	if err := ProtocolPairingOK(transcript); err != nil {
		t.Fatal(err)
	}
	broken := append([]provider.ChatMessage{}, transcript[:2]...)
	broken = append(broken, closed[1], closed[0])
	if err := ProtocolPairingOK(broken); err == nil {
		t.Fatal("user guidance before the tool result was accepted")
	}
}

func TestProtocolPairingChatAndResponses(t *testing.T) {
	chat := []provider.ChatMessage{
		{Role: provider.RoleAssistant, ToolCalls: []provider.ToolCall{{ID: "c1", Function: provider.FunctionCall{Name: "get_worker_trace"}}}},
		{Role: provider.RoleTool, ToolCallID: "c1", Content: "trace"},
	}
	if err := ProtocolPairingOK(chat); err != nil {
		t.Fatal(err)
	}
	responses := []provider.ChatMessage{
		{
			Role:      provider.RoleAssistant,
			ToolCalls: []provider.ToolCall{{ID: "call_resp", Function: provider.FunctionCall{Name: "get_worker_trace"}}},
			NativePayload: &provider.NativePayload{Format: "responses", Output: []json.RawMessage{
				json.RawMessage(`{"type":"function_call","call_id":"call_resp","name":"get_worker_trace"}`),
			}},
		},
		{Role: provider.RoleTool, ToolCallID: "call_resp", Content: "trace"},
	}
	if err := ProtocolPairingOK(responses); err != nil {
		t.Fatal(err)
	}
	responses[0].NativePayload.Output = []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"other"}`)}
	if err := ProtocolPairingOK(responses); err == nil {
		t.Fatal("unmatched native function_call was accepted")
	}
}

func TestMemoryReplanClosesOnlyUnexecutedCalls(t *testing.T) {
	port := NewMemoryReplan()
	workID := uuid.New()
	port.NoteSteer(workID, "只读取授权入口")
	batch := []ClosedToolCall{{ID: "done", Name: "read_file"}, {ID: "next", Name: "http_request"}}
	decision := port.DecideBeforeTool(workID, batch, 1)
	if !decision.CloseUnexecuted || len(decision.ClosedCalls) != 1 || decision.ClosedCalls[0].ID != "next" {
		t.Fatalf("decision = %+v", decision)
	}
	if !strings.Contains(decision.ClosedCalls[0].Result, "只读取授权入口") {
		t.Fatalf("close text = %q", decision.ClosedCalls[0].Result)
	}
	again := port.DecideBeforeTool(workID, batch, 0)
	if again.CloseUnexecuted {
		t.Fatal("steer was consumed twice")
	}
}
