package provider

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCompactionPreservesNativeProtocolBlocks(t *testing.T) {
	for _, format := range []string{"chat_completions", "responses"} {
		t.Run(format, func(t *testing.T) {
			messages := []ChatMessage{{Role: RoleSystem, Content: "Immutable authorization boundary"}, {Role: RoleUser, Content: "Inspect saved evidence"}}
			for i := 0; i < 30; i++ {
				id := fmt.Sprintf("call-%d", i)
				messages = append(messages, ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Type: "function", Function: FunctionCall{Name: "read_artifact", Arguments: `{}`}}}, NativePayload: &NativePayload{Format: format, Output: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"reasoning","encrypted_content":"opaque-%d"}`, i))}}}, ChatMessage{Role: RoleTool, ToolCallID: id, Content: strings.Repeat("recorded evidence ", 150)})
			}
			before := ConversationHash(messages)
			view, summary, changed, err := CompactConversation(messages, 20000)
			require.NoError(t, err)
			require.True(t, changed)
			require.Contains(t, summary, "not new evidence")
			require.Equal(t, before, ConversationHash(messages))
			require.Equal(t, messages[0], view[0])
			pending := map[string]bool{}
			for _, m := range view {
				for _, call := range m.ToolCalls {
					pending[call.ID] = true
				}
				if m.Role == RoleTool {
					require.True(t, pending[m.ToolCallID], "tool result lost its matching assistant call")
					delete(pending, m.ToolCallID)
				}
			}
			require.Empty(t, pending)
			encoded, err := EncodeCheckpoint(view)
			require.NoError(t, err)
			restored, err := DecodeCheckpoint(encoded)
			require.NoError(t, err)
			require.Equal(t, ConversationHash(view), ConversationHash(restored))
			require.Contains(t, string(encoded), "opaque-29", "recent native payload must not be replaced by a lossy generic message")
		})
	}
}

func TestCompactionRefusesToSplitOpenToolBatch(t *testing.T) {
	messages := []ChatMessage{{Role: RoleSystem, Content: "Policy"}, {Role: RoleAssistant, ToolCalls: []ToolCall{{ID: "pending", Function: FunctionCall{Name: "read_artifact"}}}}}
	for i := 0; i < 20; i++ {
		messages = append(messages, ChatMessage{Role: RoleTool, ToolCallID: "unrelated", Content: strings.Repeat("x", 2000)})
	}
	_, _, _, err := CompactConversation(messages, 4000)
	require.Error(t, err)
	require.ErrorIs(t, err, ErrContextLimit)
}

// Regression: real Planner failures had only 7-8 messages. Message count must
// not prevent compaction of otherwise complete protocol blocks.
func TestCompactionShortConversationRetainsRequestAndLatestBatch(t *testing.T) {
	for _, turns := range []int{2, 3} {
		t.Run(fmt.Sprintf("%d_messages", 2+turns*2), func(t *testing.T) {
			messages := []ChatMessage{{Role: RoleSystem, Content: "Scope is immutable"}, {Role: RoleUser, Content: strings.Repeat("Current task and authorized scope. ", 150)}}
			for i := 0; i < turns; i++ {
				id := fmt.Sprintf("read-%d", i)
				messages = append(messages,
					ChatMessage{Role: RoleAssistant, ToolCalls: []ToolCall{{ID: id, Function: FunctionCall{Name: "read_artifact", Arguments: `{}`}}}, NativePayload: &NativePayload{Format: "responses", Output: []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"reasoning","encrypted_content":"native-%d"}`, i))}}},
					ChatMessage{Role: RoleTool, ToolCallID: id, Content: strings.Repeat("saved evidence ", 250)})
			}
			original := ConversationHash(messages)
			view, _, changed, err := CompactConversation(messages, 11000)
			require.NoError(t, err)
			require.True(t, changed)
			require.Equal(t, messages[:2], view[:2], "never replace the user's actual task with an excerpt")
			require.Equal(t, messages[len(messages)-2:], view[len(view)-2:])
			require.Equal(t, original, ConversationHash(messages))
			packed, err := EncodeCheckpoint(view)
			require.NoError(t, err)
			require.LessOrEqual(t, len(packed), 11000)
		})
	}
}

func TestCompactionAdaptiveTailKeepsWholeMultiToolBatch(t *testing.T) {
	messages := []ChatMessage{{Role: RoleSystem, Content: "Scope"}, {Role: RoleUser, Content: "Task"}}
	for batch := 0; batch < 3; batch++ {
		calls := []ToolCall{}
		for i := 0; i < 3; i++ {
			calls = append(calls, ToolCall{ID: fmt.Sprintf("%d-%d", batch, i), Function: FunctionCall{Name: "read_artifact"}})
		}
		messages = append(messages, ChatMessage{Role: RoleAssistant, ToolCalls: calls})
		for _, call := range calls {
			messages = append(messages, ChatMessage{Role: RoleTool, ToolCallID: call.ID, Content: strings.Repeat("中", 650)})
		}
	}
	view, _, changed, err := CompactConversation(messages, 8500)
	require.NoError(t, err)
	require.True(t, changed)
	require.Equal(t, messages[len(messages)-4:], view[len(view)-4:])
	require.Len(t, view, 7, "must compact beyond an eight-message suffix when needed")
}

func TestCompactionDoesNotDiscardOversizedTaskOrLatestNativePayload(t *testing.T) {
	for _, messages := range [][]ChatMessage{
		{{Role: RoleSystem, Content: "Scope"}, {Role: RoleUser, Content: strings.Repeat("task", 1500)}, {Role: RoleAssistant, Content: "done"}},
		{{Role: RoleSystem, Content: "Scope"}, {Role: RoleUser, Content: "Task"}, {Role: RoleAssistant, Content: strings.Repeat("history", 1000)}, {Role: RoleAssistant, NativePayload: &NativePayload{Format: "responses", Output: []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("x", 5000) + `"}`)}}}},
	} {
		original := ConversationHash(messages)
		_, _, changed, err := CompactConversation(messages, 4000)
		require.ErrorIs(t, err, ErrContextLimit)
		require.False(t, changed)
		require.Equal(t, original, ConversationHash(messages))
	}
}
