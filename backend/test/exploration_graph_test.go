package test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-workbench/backend/internal/handler"
	"agent-workbench/backend/internal/service"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/stretchr/testify/require"
)

func TestExplorationGraph_AuthoritativeSnapshot(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	run, asset, work := seedHTTPAsset(t, pool, `{}`, "http://127.0.0.1/", uuid.NewString())
	otherRun, _, _ := seedHTTPAsset(t, pool, `{}`, "http://127.0.0.1/", uuid.NewString())
	svc := service.NewRunService(pool)
	node := func(kind, state string) uuid.UUID {
		var id uuid.UUID
		err := pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,state,payload) VALUES($1,$2,$3,'{"summary":"真实节点"}') RETURNING id`, run, kind, state).Scan(&id)
		require.NoError(t, err)
		return id
	}
	goal := node("goal", "met")
	isolated := node("goal", "open")
	intent := node("intent", "open")
	fact := node("fact", "verified")
	unverified := node("fact", "unverified")
	findingNode := node("finding", "unverified")
	_, err := pool.Exec(ctx, `UPDATE exploration_nodes SET work_item_id=$1 WHERE id=$2`, work, intent)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE work_items SET status='completed' WHERE id=$1`, work)
	require.NoError(t, err)
	var finding uuid.UUID
	err = pool.QueryRow(ctx, `WITH c AS (
		INSERT INTO check_results(run_id,asset_id,check_id,check_name,check_version,status,explanation)
		VALUES($1,$2,'graph-test','graph-test','1','needs_attention','候选') RETURNING id)
		INSERT INTO findings(run_id,asset_id,check_result_id,title,category,severity,description,source_kind,verification_state)
		SELECT $1,$2,c.id,'真实待审标题','config','low','候选','speculative','unverified' FROM c RETURNING id`, run, asset).Scan(&finding)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE exploration_nodes SET finding_id=$1 WHERE id=$2`, finding, findingNode)
	require.NoError(t, err)
	var liveProof uuid.UUID
	err = pool.QueryRow(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel,source_version,target_version,reason)
		VALUES($1,$2,$3,'proves',1,1,'真实证明') RETURNING id`, run, fact, goal).Scan(&liveProof)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel,source_version,target_version) VALUES
		($1,$2,$3,'yields',1,1),($1,$4,$2,'derived_from',1,1),
		($1,$3,$5,'proves',1,1),($1,$4,$6,'proves',0,1),($1,$7,$5,'proves',1,1)`, run, intent, unverified, fact, goal, isolated, findingNode)
	require.NoError(t, err)
	// Historical invalid edges are hidden, and cross-run edges are rejected.
	_, err = pool.Exec(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel,valid) VALUES($1,$2,$3,'spawns',false)`, run, goal, intent)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO exploration_edges(run_id,src_id,dst_id,rel) VALUES($1,$2,$3,'spawns')`, otherRun, intent, fact)
	require.Error(t, err)
	for i := 0; i < 105; i++ {
		node("fact", "unverified")
	}
	graph, err := svc.ExplorationGraph(ctx, run)
	require.NoError(t, err)
	require.Len(t, graph.Nodes, 111, "full canvas must not be limited to overview's last facts/page")
	require.Len(t, graph.Edges, 3, "two real lineage edges and one effective proof")
	byID := map[string]map[string]any{}
	for _, raw := range graph.Nodes {
		var n map[string]any
		require.NoError(t, json.Unmarshal(raw, &n))
		byID[n["id"].(string)] = n
	}
	require.Equal(t, "completed", byID[intent.String()]["work_status"])
	require.Equal(t, "open", byID[intent.String()]["state"])
	require.Equal(t, work.String(), byID[intent.String()]["work_item_id"])
	require.Equal(t, "unverified", byID[unverified.String()]["state"])
	f := byID[findingNode.String()]["finding"].(map[string]any)
	require.Equal(t, finding.String(), f["id"])
	require.Equal(t, "真实待审标题", f["title"])
	require.Equal(t, "pending_review", f["status"])
	require.Equal(t, "unverified", f["verification_state"])
	_, err = pool.Exec(ctx, `UPDATE exploration_nodes SET state='verified' WHERE id=$1`, findingNode)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `UPDATE findings SET verification_state='verified' WHERE id=$1`, finding)
	require.NoError(t, err)
	graph, err = svc.ExplorationGraph(ctx, run)
	require.NoError(t, err)
	require.Len(t, graph.Edges, 4, "a real verified finding proof uses its exploration node ID")
	for _, verdict := range []string{"fixed", "still_reproducible"} {
		_, err = pool.Exec(ctx, `UPDATE findings SET verification_state='retested',retest_verdict=$2 WHERE id=$1`, finding, verdict)
		require.NoError(t, err)
		graph, err = svc.ExplorationGraph(ctx, run)
		require.NoError(t, err)
		if verdict == "fixed" {
			require.Len(t, graph.Edges, 3, "fixed finding is no longer proof")
		} else {
			require.Len(t, graph.Edges, 4)
		}
	}
	_, err = pool.Exec(ctx, `UPDATE findings SET status='false_positive' WHERE id=$1`, finding)
	require.NoError(t, err)
	graph, err = svc.ExplorationGraph(ctx, run)
	require.NoError(t, err)
	require.Len(t, graph.Edges, 3, "false positive is no longer proof")
	_, err = pool.Exec(ctx, `UPDATE exploration_nodes SET content_version=2 WHERE id=$1`, fact)
	require.NoError(t, err)
	graph, err = svc.ExplorationGraph(ctx, run)
	require.NoError(t, err)
	require.Len(t, graph.Edges, 2, "source content change removes stale proof, preserves lineage")
	graph, err = svc.ExplorationGraph(ctx, otherRun)
	require.NoError(t, err)
	require.Empty(t, graph.Edges)
	_, err = svc.ExplorationGraph(ctx, uuid.New())
	require.ErrorIs(t, err, pgx.ErrNoRows)

	h := handler.NewRunHandler(svc, nil, nil)
	router := chi.NewRouter()
	router.Get("/api/runs/{runID}/graph", h.ExplorationGraph)
	for _, tc := range []struct {
		id     string
		status int
	}{{run.String(), http.StatusOK}, {uuid.NewString(), http.StatusNotFound}, {"not-a-uuid", http.StatusBadRequest}} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/runs/"+tc.id+"/graph", nil))
		require.Equal(t, tc.status, response.Code, response.Body.String())
	}
}
