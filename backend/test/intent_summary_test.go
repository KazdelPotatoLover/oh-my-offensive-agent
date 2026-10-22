package test

import (
	"context"
	"encoding/json"
	"testing"

	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestIntentSummary_HistoricalPayloads(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	run, _, _ := seedHTTPAsset(t, pool, `{}`, "http://127.0.0.1/", uuid.NewString())
	for _, payload := range []string{`{"text":"历史 text 方向"}`, `{"summary":"历史 summary 方向"}`, `{"summary":"  ","text":"空摘要回退方向"}`} {
		_, err := pool.Exec(ctx, `INSERT INTO exploration_nodes(run_id,kind,state,payload) VALUES($1,'intent','open',$2::jsonb)`, run, payload)
		require.NoError(t, err)
	}
	graph, err := service.NewRunService(pool).GraphOverview(ctx, run)
	require.NoError(t, err)
	raw, _ := json.Marshal(graph)
	for _, summary := range []string{"历史 text 方向", "历史 summary 方向", "空摘要回退方向"} {
		require.Contains(t, string(raw), `"summary":"`+summary+`"`)
	}
}
