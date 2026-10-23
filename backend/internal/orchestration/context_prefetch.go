package orchestration

import (
	"encoding/json"
	"unicode/utf8"
)

// Prefetch is an index into persisted state, not another copy of all evidence.
// Keep room for tool batches and native reasoning in the same context window.
func prefetchBudget(window int) int {
	if window <= 0 {
		window = 98304
	}
	return max(256, min(16384, window/6))
}

func prefetchExcerpt(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	if len(s) <= limit {
		return s
	}
	suffix := "…"
	if limit < len(suffix) {
		suffix = ""
	}
	end := max(0, limit-len(suffix))
	for end > 0 && !utf8.RuneStart(s[end]) {
		end--
	}
	return s[:end] + suffix
}

func prefetchText(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	note := "\n[Prefetch shortened. Query graph_overview, node_detail, list_run_assets, manage_planner_todos or worker traces for persisted details; omitted entries are not absent.]"
	if limit < len(note) {
		return prefetchExcerpt(note, max(0, limit))
	}
	return prefetchExcerpt(s, max(0, limit-len(note))) + note
}

func renderGraphPrefetch(graph map[string]any, limit int) string {
	raw, err := json.Marshal(graph)
	if err != nil {
		return `{"_prefetch":"unavailable; query graph_overview"}`
	}
	if len(raw) <= limit {
		return string(raw)
	}
	// Normalize typed slices without mutating the authoritative overview.
	var source map[string]any
	if json.Unmarshal(raw, &source) != nil {
		return `{"_prefetch":"unavailable; query graph_overview"}`
	}
	counts := map[string]int{}
	for key, value := range source {
		if rows, ok := value.([]any); ok {
			counts[key] = len(rows)
		}
	}
	for rows, excerpt := 12, 1024; ; {
		view := prefetchValue(source, rows, excerpt).(map[string]any)
		view["_prefetch"] = map[string]any{
			"truncated": true, "section_counts": counts,
			"details": "Query node_detail, graph_overview or worker traces; excerpts are not full evidence. Omitted entries are not absent.",
		}
		packed, _ := json.Marshal(view)
		if len(packed) <= limit {
			return string(packed)
		}
		// Shorten prose before dropping node IDs: peer work and outcomes should
		// remain discoverable even when their attached evidence is verbose.
		if excerpt > 256 {
			excerpt /= 2
		} else if rows > 1 {
			rows = max(1, rows/2)
		} else if excerpt > 64 {
			excerpt /= 2
		} else {
			// Tiny windows still receive truthful counts and an explicit lookup.
			fallback, _ := json.Marshal(map[string]any{"_prefetch": "omitted; query graph_overview", "total_goals": graph["total_goals"], "met_goals": graph["met_goals"], "done_intents_total": graph["done_intents_total"]})
			return string(fallback)
		}
	}
}

func prefetchValue(value any, rows, excerpt int) any {
	switch v := value.(type) {
	case string:
		return prefetchExcerpt(v, excerpt)
	case []any:
		out := make([]any, min(len(v), rows))
		for i := range out {
			out[i] = prefetchValue(v[i], rows, excerpt)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(v))
		for key, item := range v {
			out[key] = prefetchValue(item, rows, excerpt)
		}
		return out
	default:
		return value
	}
}
