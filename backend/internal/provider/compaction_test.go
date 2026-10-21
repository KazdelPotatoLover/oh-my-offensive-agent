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
}
