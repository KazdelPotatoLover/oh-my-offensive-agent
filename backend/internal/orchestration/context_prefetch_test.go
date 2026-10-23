package orchestration

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/require"
)

func TestGraphPrefetchBoundsEvidenceWithoutLosingOutcomeReferences(t *testing.T) {
	graph := map[string]any{
		"total_goals": 5, "met_goals": 0, "done_intents_total": 10,
		"recent_done_intents": []map[string]any{{"id": "intent-1", "work_item_id": "work-1", "state": "completed", "last_output": "已完成新端点取证" + strings.Repeat("长结果", 15000)}},
		"recent_findings":     []map[string]any{{"id": "finding-1", "verification_state": "unverified", "title": "候选发现"}},
		"execution_context":   map[string]any{"recent_tool_failures": []map[string]any{{"error": "scope_denied", "count": 3}}},
		"facts":               []map[string]any{{"id": "fact-1", "evidence": strings.Repeat("证据", 15000)}},
	}
	original, err := json.Marshal(graph)
	require.NoError(t, err)
	view := renderGraphPrefetch(graph, 5000)
	require.LessOrEqual(t, len(view), 5000)
	require.True(t, utf8.ValidString(view))
	var decoded map[string]any
	require.NoError(t, json.Unmarshal([]byte(view), &decoded))
	for _, reference := range []string{"work-1", "finding-1", "unverified", "fact-1", "scope_denied", "已完成新端点取证", "section_counts"} {
		require.Contains(t, view, reference)
	}
	after, err := json.Marshal(graph)
	require.NoError(t, err)
	require.Equal(t, original, after, "prefetch must not mutate evidence returned by APIs")
}

func TestPrefetchTinyWindowAndSmallUnchangedOverview(t *testing.T) {
	graph := map[string]any{"total_goals": 2, "facts": []any{map[string]any{"evidence": strings.Repeat("data", 50000)}}}
	view := renderGraphPrefetch(graph, prefetchBudget(1536))
	require.LessOrEqual(t, len(view), 256)
	require.True(t, json.Valid([]byte(view)))
	require.Contains(t, view, "query graph_overview")
	small := map[string]any{"total_goals": 1, "all_goals_met": false}
	raw, _ := json.Marshal(small)
	require.Equal(t, string(raw), renderGraphPrefetch(small, 2000))
	text := prefetchText(strings.Repeat("态势", 10000), 1000)
	require.LessOrEqual(t, len(text), 1000)
	require.True(t, utf8.ValidString(text))
	require.Contains(t, text, "omitted entries are not absent")
}
