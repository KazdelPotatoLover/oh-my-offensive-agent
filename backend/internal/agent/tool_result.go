package agent

import "encoding/json"

// evidenceToolMessage adds program-owned references to the model-visible result.
// The original tool output is stored separately, without this context truncation.
func evidenceToolMessage(raw, recordID, callID string, limit int) string {
	if limit <= 0 {
		limit = 8192
	}
	var result map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &result); err != nil || result == nil {
		result = map[string]interface{}{"result_preview": raw}
	}
	metadata := map[string]interface{}{"source_tool_call_id": recordID, "call_id": callID, "result_truncated": false}
	result["_execution"] = metadata
	encode := func() string { b, _ := json.Marshal(result); return string(b) }
	full := encode()
	if len(full) <= limit {
		return full
	}
	metadata["result_truncated"] = true
	// Shorten only the body while retaining valid JSON, headers and source identity.
	parent := result
	if response, ok := result["response"].(map[string]interface{}); ok {
		parent = response
	}
	key := "body"
	body, ok := parent[key].(string)
	if !ok {
		key = "result_preview"
		body, _ = parent[key].(string)
	}
	parent[key] = ""
	if len(encode()) <= limit {
		low, high := 0, len(body)
		for low < high {
			mid := (low + high + 1) / 2
			parent[key] = truncateUTF8(body, mid)
			if len(encode()) <= limit {
				low = mid
			} else {
				high = mid - 1
			}
		}
		parent[key] = truncateUTF8(body, low)
		return encode()
	}
	// Even headers may exceed a very small configured budget; retain evidence IDs.
	result = map[string]interface{}{"_execution": metadata}
	if compact := encode(); len(compact) <= limit {
		return compact
	}
	return "{}"
}
