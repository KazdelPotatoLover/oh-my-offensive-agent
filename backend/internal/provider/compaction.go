package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// CheckpointMessage keeps provider-native reasoning/response items alongside
// the generic message. ChatMessage intentionally omits those fields on the wire.
type CheckpointMessage struct {
	ChatMessage
	Payload *NativePayload `json:"native_payload,omitempty"`
}

func EncodeCheckpoint(messages []ChatMessage) ([]byte, error) {
	wire := make([]CheckpointMessage, len(messages))
	for i, m := range messages {
		wire[i] = CheckpointMessage{ChatMessage: m, Payload: m.NativePayload}
	}
	return json.Marshal(wire)
}
func DecodeCheckpoint(raw []byte) ([]ChatMessage, error) {
	var wire []CheckpointMessage
	if err := json.Unmarshal(raw, &wire); err != nil {
		return nil, err
	}
	out := make([]ChatMessage, len(wire))
	for i, m := range wire {
		out[i] = m.ChatMessage
		out[i].NativePayload = m.Payload
	}
	return out, nil
}
func ConversationHash(messages []ChatMessage) string {
	b, _ := EncodeCheckpoint(messages)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// CompactConversation replaces only complete historical protocol blocks. It
// preserves all system instructions and the current exchange, and never splits
// a tool batch or its native Responses items. The digest is an index, not proof.
func CompactConversation(messages []ChatMessage, limit int) ([]ChatMessage, string, bool, error) {
	if limit <= 0 {
		limit = 98304
	}
	raw, err := EncodeCheckpoint(messages)
	if err != nil {
		return nil, "", false, err
	}
	if len(raw) <= limit {
		return messages, "", false, nil
	}
	pending := map[string]bool{}
	cut := 0
	for i, m := range messages {
		if i >= len(messages)-8 {
			break
		}
		if m.Role == RoleAssistant {
			for _, call := range m.ToolCalls {
				pending[call.ID] = true
			}
		}
		if m.Role == RoleTool {
			delete(pending, m.ToolCallID)
		}
		if len(pending) == 0 && m.Role != RoleSystem {
			cut = i + 1
		}
	}
	if cut == 0 {
		return nil, "", false, fmt.Errorf("context limit exceeded within an indivisible protocol block; full transcript retained")
	}
	var digest strings.Builder
	digest.WriteString("Historical context index (not new evidence). Original messages and native payloads remain available through traces and artifacts.\n")
	view := []ChatMessage{}
	for i, m := range messages[:cut] {
		if m.Role == RoleSystem {
			view = append(view, m)
			continue
		}
		text := m.Content
		// Store references and excerpts rather than fabricate a model summary.
		if len(text) > 200 {
			text = string([]rune(text)[:min(100, len([]rune(text)))]) + "…"
		}
		if digest.Len() < 5000 {
			fmt.Fprintf(&digest, "[%d %s] %s", i, m.Role, text)
			for _, call := range m.ToolCalls {
				fmt.Fprintf(&digest, " tool=%s call=%s", call.Function.Name, call.ID)
			}
			if m.ToolCallID != "" {
				fmt.Fprintf(&digest, " result_of=%s", m.ToolCallID)
			}
			digest.WriteByte('\n')
		}
	}
	summary := digest.String()
	view = append(view, ChatMessage{Role: RoleUser, Content: summary})
	view = append(view, messages[cut:]...)
	packed, _ := EncodeCheckpoint(view)
	if len(packed) >= len(raw) || len(packed) > limit {
		return nil, "", false, fmt.Errorf("context cannot be safely compacted below the configured limit; raw transcript retained")
	}
	return view, summary, true, nil
}
