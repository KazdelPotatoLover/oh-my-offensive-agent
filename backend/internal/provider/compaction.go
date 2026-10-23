package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
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

// ErrContextLimit means local history cannot fit without dropping protected
// instructions or splitting the latest protocol block. It is not a provider error.
var ErrContextLimit = errors.New("context cannot be safely compacted")

// CompactConversation replaces complete historical protocol blocks, preserving
// system instructions, the original user request and the latest exchange. Native
// reasoning and tool-call payloads in the retained suffix remain byte-for-byte.
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
	firstUser := -1
	for i, m := range messages {
		if m.Role == RoleUser {
			firstUser = i
			break
		}
	}
	// Skip impossible candidates before building indexes or re-encoding a long
	// restored transcript. Each message's serialized size is additive in JSON.
	suffixBytes := make([]int, len(messages)+1)
	for i := len(messages) - 1; i >= 0; i-- {
		encoded, _ := EncodeCheckpoint(messages[i : i+1])
		suffixBytes[i] = suffixBytes[i+1] + len(encoded) - 2
	}
	protectedBytes, protectedCount := 0, 0
	pending := map[string]bool{}
	for i, m := range messages {
		if m.Role == RoleSystem || i == firstUser {
			protectedBytes += suffixBytes[i] - suffixBytes[i+1]
			protectedCount++
		}
		if m.Role == RoleAssistant {
			for _, call := range m.ToolCalls {
				pending[call.ID] = true
			}
		}
		if m.Role == RoleTool {
			delete(pending, m.ToolCallID)
		}
		// Try progressively shorter complete suffixes instead of retaining an
		// arbitrary message count. Never remove the current (possibly open) batch.
		if len(pending) != 0 || m.Role == RoleSystem || i+1 == len(messages) {
			continue
		}
		cut := i + 1
		lowerBound := 2 + protectedBytes + suffixBytes[cut] + max(0, protectedCount+len(messages)-cut-1)
		if lowerBound > limit {
			continue
		}
		view := make([]ChatMessage, 0, len(messages))
		history := make([]string, 0, cut)
		for j, old := range messages[:cut] {
			if old.Role == RoleSystem || j == firstUser {
				view = append(view, old)
				continue
			}
			entry := fmt.Sprintf("[%d %s] %s", j, old.Role, contextExcerpt(old.Content, 200))
			for _, call := range old.ToolCalls {
				entry += fmt.Sprintf(" tool=%s call=%s", contextExcerpt(call.Function.Name, 80), contextExcerpt(call.ID, 100))
				if len(entry) > 600 {
					break
				}
			}
			if old.ToolCallID != "" {
				entry += " result_of=" + contextExcerpt(old.ToolCallID, 100)
			}
			history = append(history, contextExcerpt(entry, 800)+"\n")
		}
		if len(history) == 0 {
			continue
		}
		header := "Historical context index (not new evidence). Full original messages and native payloads remain in traces/artifacts.\n"
		remaining := max(0, min(4096, limit/8)-len(header))
		start := len(history)
		// Prefer recent outcomes and references when the index itself is bounded.
		for start > 0 && len(history[start-1]) <= remaining {
			start--
			remaining -= len(history[start])
		}
		summary := header + strings.Join(history[start:], "")
		view = append(view, ChatMessage{Role: RoleUser, Content: summary})
		view = append(view, messages[cut:]...)
		packed, packErr := EncodeCheckpoint(view)
		if packErr != nil {
			return nil, "", false, packErr
		}
		if len(packed) < len(raw) && len(packed) <= limit {
			return view, summary, true, nil
		}
	}
	return nil, "", false, fmt.Errorf("%w: %d bytes exceeds limit %d; original request, system instructions and latest protocol block retained in full transcript", ErrContextLimit, len(raw), limit)
}

func contextExcerpt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:max(0, end)] + "…"
}
