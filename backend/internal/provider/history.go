package provider

// SavedToolOutcome is a previously persisted tool result. Restoring it must not
// execute the tool again.
type SavedToolOutcome struct {
	CallID  string
	Status  string
	Content string
}

// RestoreSavedToolResults closes an interrupted tool batch with saved results.
// Calls without a saved outcome stay open and are returned so the caller can
// refuse to replay them. This function does not invent arguments or call ids.
func RestoreSavedToolResults(messages []ChatMessage, saved map[string]SavedToolOutcome) ([]ChatMessage, []string) {
	if len(messages) == 0 {
		return messages, nil
	}
	out := make([]ChatMessage, 0, len(messages)+4)
	pending := []ToolCall{}
	fulfilled := map[string]bool{}
	flush := func() []string {
		var open []string
		for _, call := range pending {
			if fulfilled[call.ID] {
				continue
			}
			outcome, ok := saved[call.ID]
			if !ok {
				open = append(open, call.ID)
				continue
			}
			content := outcome.Content
			if content == "" {
				content = `{"status":"outcome_unknown","recovered_result":true,"instruction":"do not automatically replay the external action"}`
			}
			out = append(out, ChatMessage{Role: RoleTool, ToolCallID: call.ID, Content: content})
			fulfilled[call.ID] = true
		}
		pending = nil
		return open
	}
	var open []string
	for _, message := range messages {
		if message.Role == RoleUser || message.Role == RoleAssistant {
			open = append(open, flush()...)
		}
		out = append(out, message)
		if message.Role == RoleAssistant {
			pending = append(pending, message.ToolCalls...)
		}
		if message.Role == RoleTool && message.ToolCallID != "" {
			fulfilled[message.ToolCallID] = true
		}
	}
	open = append(open, flush()...)
	return out, open
}
