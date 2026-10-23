package test

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/stretchr/testify/require"
)

func TestLineage_NewExplorationHasOneStartAndRealSources(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", "")
	t.Setenv("OMA_AGENT_LIMITS", "")
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	tasks := service.NewTaskService(pool)
	runs := service.NewRunService(pool)
	task, err := tasks.Create(ctx, model.CreateTaskRequest{
		Name: "血缘探索", Objective: "检查入口并保存证据",
		Config: json.RawMessage(`{"exploration":true,"executor":"orchestrator","provider":"mock"}`),
	})
	require.NoError(t, err)
	run, _, err := runs.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	policy := model.PolicyFromExecutionInput(run.ExecutionInput)
	require.NotNil(t, policy)
	require.Equal(t, model.AgentLimitsSchema, policy.SchemaVersion)
	require.False(t, policy.ModelLimited())
	require.False(t, policy.HasDeadline())

	var origins int
	var originID, goalID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1 AND kind='fact' AND state='origin'`, run.ID).Scan(&origins))
	require.Equal(t, 1, origins)
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM exploration_nodes WHERE run_id=$1 AND kind='fact' AND state='origin'`, run.ID).Scan(&originID))
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM exploration_nodes WHERE run_id=$1 AND kind='goal'`, run.ID).Scan(&goalID))
	var rel string
	var verified bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT e.rel, (n.payload->>'verified_source')::boolean
		FROM exploration_edges e JOIN exploration_nodes n ON n.id=e.src_id
		WHERE e.run_id=$1 AND e.dst_id=$2`, run.ID, goalID).Scan(&rel, &verified))
	require.Equal(t, "spawns", rel)
	require.False(t, verified)
	_, err = runs.ProveGoal(ctx, run.ID, goalID, originID, "起点不能当证据")
	require.Error(t, err)
	require.Error(t, runs.ReviseExplorationNode(ctx, run.ID, originID, 1, json.RawMessage(`{"summary":"改写起点"}`), "verified", "no", "tester"))

	_, err = runs.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	require.NoError(t, err)
	items, err := runs.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{Key: "entry", Title: "检查入口", Objective: "记录入口事实", WorkType: "intent", InputData: map[string]any{"profile": "http_readonly"}},
		{Key: "headers", Title: "检查响应头", Objective: "记录响应头", WorkType: "intent", InputData: map[string]any{"profile": "http_readonly"}},
	})
	require.NoError(t, err)
	require.Len(t, items, 2)
	var sourced int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND rel='derived_from'`, run.ID, originID).Scan(&sourced))
	require.Equal(t, 2, sourced, "first dispatch without explicit parents uses the task start")

	f1, err := runs.RecordFact(ctx, run.ID, "入口返回 200", "", "observed", "body", nil, nil)
	require.NoError(t, err)
	f2, err := runs.RecordFact(ctx, run.ID, "响应包含安全头", "", "observed", "header", nil, nil)
	require.NoError(t, err)
	beforeNodes := countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1`, run.ID)
	_, err = runs.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{
		Key: "foreign", Title: "非法来源", Objective: "不得回退到起点", WorkType: "intent",
		InputData: map[string]any{"profile": "http_readonly"}, SourceFactIDs: []uuid.UUID{uuid.New()},
	}})
	require.Error(t, err)
	require.Equal(t, beforeNodes, countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1`, run.ID))

	sourcedItems, err := runs.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{{
		Key: "follow", Title: "沿真实事实继续", Objective: "只使用已记录事实", WorkType: "intent",
		InputData: map[string]any{"profile": "http_readonly"}, SourceFactIDs: []uuid.UUID{f1.ID, f2.ID},
	}})
	require.NoError(t, err)
	var followSources int
	var originUsed bool
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*), bool_or(src_id=$2) FROM exploration_edges e
		JOIN exploration_nodes n ON n.id=e.dst_id
		WHERE n.work_item_id=$1 AND e.rel='derived_from'`, sourcedItems[0].ID, originID).Scan(&followSources, &originUsed))
	require.Equal(t, 2, followSources)
	require.False(t, originUsed)

	var assetID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO assets(namespace,kind,external_key,name) VALUES('lineage','http_endpoint','entry','入口') RETURNING id`).Scan(&assetID))
	_, err = pool.Exec(ctx, `INSERT INTO run_assets(run_id,asset_id,snapshot) VALUES($1,$2,'{}')`, run.ID, assetID)
	require.NoError(t, err)
	finding, err := runs.ReportFinding(ctx, run.ID, service.ReportFindingInput{
		AssetID: assetID, Title: "可能的配置问题", Category: "test", Severity: "low",
		Description: "方向产出", SourceKind: "speculative", TrustedWorkItemID: &items[0].ID,
	})
	require.NoError(t, err)
	var yieldSrc uuid.UUID
	var yieldCount int
	require.NoError(t, pool.QueryRow(ctx, `SELECT count(*) FROM exploration_edges e
		JOIN exploration_nodes n ON n.id=e.dst_id
		WHERE n.finding_id=$1 AND e.rel='yields'`, finding.ID).Scan(&yieldCount))
	require.NoError(t, pool.QueryRow(ctx, `SELECT e.src_id FROM exploration_edges e
		JOIN exploration_nodes n ON n.id=e.dst_id
		WHERE n.finding_id=$1 AND e.rel='yields'`, finding.ID).Scan(&yieldSrc))
	require.Equal(t, 1, yieldCount)
	var expectedIntent uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM exploration_nodes WHERE work_item_id=$1 AND kind='intent'`, items[0].ID).Scan(&expectedIntent))
	require.Equal(t, expectedIntent, yieldSrc)

	var orphan uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO work_items(run_id,key,title) VALUES($1,'orphan','无方向') RETURNING id`, run.ID).Scan(&orphan))
	_, err = runs.ReportFinding(ctx, run.ID, service.ReportFindingInput{
		AssetID: assetID, Title: "不能归属", Category: "test", Severity: "info",
		Description: "没有方向", SourceKind: "speculative", TrustedWorkItemID: &orphan,
	})
	require.ErrorIs(t, err, service.ErrInvalidEvidence)

	var wg sync.WaitGroup
	errCh := make(chan error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			_, goalErr := runs.SetGoals(ctx, run.ID, []model.GoalSpec{{Text: fmt.Sprintf("补充目标 %d", n)}})
			errCh <- goalErr
		}(i)
	}
	wg.Wait()
	close(errCh)
	for goalErr := range errCh {
		require.NoError(t, goalErr)
	}
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1 AND kind='fact' AND state='origin'`, run.ID))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM exploration_nodes g WHERE g.run_id=$1 AND g.kind='goal' AND NOT EXISTS (
		SELECT 1 FROM exploration_edges e WHERE e.dst_id=g.id AND e.rel='spawns')`, run.ID))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND rel='proves'`, run.ID))

	before := countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1`, run.ID)
	graph, err := runs.ExplorationGraph(ctx, run.ID)
	require.NoError(t, err)
	graphAgain, err := runs.ExplorationGraph(ctx, run.ID)
	require.NoError(t, err)
	require.Equal(t, len(graph.Nodes), len(graphAgain.Nodes))
	require.Equal(t, len(graph.Edges), len(graphAgain.Edges))
	require.Equal(t, before, countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1`, run.ID))
}

func TestLineage_GrantsDoNotInventUnlimitedRemainder(t *testing.T) {
	t.Setenv("OMA_EXPLORATION_BUDGET", "")
	t.Setenv("OMA_AGENT_LIMITS", "")
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	tasks := service.NewTaskService(pool)
	runs := service.NewRunService(pool)
	explore, err := tasks.Create(ctx, model.CreateTaskRequest{
		Name: "不限授权", Objective: "继续探索",
		Config: json.RawMessage(`{"exploration":true,"executor":"orchestrator","provider":"mock"}`),
	})
	require.NoError(t, err)
	openRun, _, err := runs.Create(ctx, explore.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	_, err = runs.GrantBudget(ctx, openRun.ID, 10, 0, 0, "多余的模型授权", nil)
	var contractErr *contract.Error
	require.ErrorAs(t, err, &contractErr)
	require.Equal(t, contract.CodeInvalidInput, contractErr.Code)
	require.Equal(t, "模型调用未设上限，无需追加授权", contractErr.Message)
	_, err = runs.GrantBudget(ctx, openRun.ID, 0, 4, 0, "多余的工具授权", nil)
	require.ErrorAs(t, err, &contractErr)
	require.Equal(t, "工具调用未设上限，无需追加授权", contractErr.Message)
	_, err = runs.GrantBudget(ctx, openRun.ID, 0, 0, 60, "多余的时长", nil)
	require.ErrorAs(t, err, &contractErr)
	require.Equal(t, "任务未设时限，无需追加时长", contractErr.Message)

	boundedTask, err := tasks.Create(ctx, model.CreateTaskRequest{
		Name: "有限授权", Objective: "有限探索",
		Config: json.RawMessage(`{"exploration":true,"executor":"orchestrator","provider":"mock","budget":{"max_model_calls":20,"max_tool_calls":8,"run_timeout_seconds":300}}`),
	})
	require.NoError(t, err)
	bounded, _, err := runs.Create(ctx, boundedTask.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	_, err = runs.GrantBudget(ctx, bounded.ID, 5, 0, 0, "追加有限模型次数", nil)
	require.NoError(t, err)
	effective, err := runs.GetEffectiveBudget(ctx, bounded.ID)
	require.NoError(t, err)
	require.Equal(t, 25, effective.MaxModelCalls)
	require.Equal(t, 8, effective.MaxToolCalls)
	require.Equal(t, 300, effective.RunTimeoutSeconds)

	legacyTask, err := tasks.Create(ctx, model.CreateTaskRequest{
		Name: "旧任务", Objective: "保持旧预算",
		Config: json.RawMessage(`{"executor":"orchestrator","provider":"mock","budget":{"max_model_calls":12}}`),
	})
	require.NoError(t, err)
	legacy, _, err := runs.Create(ctx, legacyTask.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)
	require.Nil(t, model.PolicyFromExecutionInput(legacy.ExecutionInput))
	_, err = runs.GrantBudget(ctx, legacy.ID, 3, 0, 0, "旧任务仍可追加", nil)
	require.NoError(t, err)
	effective, err = runs.GetEffectiveBudget(ctx, legacy.ID)
	require.NoError(t, err)
	require.Equal(t, 15, effective.MaxModelCalls)
	require.Equal(t, 120, effective.RunTimeoutSeconds)
}

func TestLineage_HistoricalBackfillIsIdempotent(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	taskID, runID, assetID := uuid.New(), uuid.New(), uuid.New()
	beginRun, beginID := uuid.New(), uuid.New()
	_, err := pool.Exec(ctx, `INSERT INTO tasks(id,name,objective) VALUES($1,'历史任务','原始目标')`, taskID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO runs(id,task_id,status,current_epoch,execution_input) VALUES($1,$2,'completed',2,'{"objective":"原始目标","scope":{"endpoints":["sample_home"]},"budget":{"max_model_calls":12}}')`, runID, taskID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO runs(id,task_id,status,execution_input) VALUES($1,$2,'completed','{"objective":"已有起点"}')`, beginRun, taskID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO assets(id,namespace,kind,external_key,name) VALUES($1,'hist','http_endpoint','old','旧资产')`, assetID)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `INSERT INTO run_assets(run_id,asset_id,snapshot) VALUES($1,$2,'{"original":true}')`, runID, assetID)
	require.NoError(t, err)

	var goalID, beginGoal, bareIntent, badArray, badElem, emptySources, factID, workID, sessionID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'goal','{"text":"历史目标"}','open') RETURNING id`, runID).Scan(&goalID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'intent','{"summary":"无来源"}','open') RETURNING id`, runID).Scan(&bareIntent))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'intent','{"source_fact_ids":{"bad":true}}','open') RETURNING id`, runID).Scan(&badArray))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'intent',$2::jsonb,'open') RETURNING id`, runID, `{"source_fact_ids":[12,"not-a-uuid"]}`).Scan(&badElem))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'intent','{"source_fact_ids":[]}','open') RETURNING id`, runID).Scan(&emptySources))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(id,run_id,kind,payload,state) VALUES($1,$2,'begin','{"text":"旧根"}','open') RETURNING id`, beginID, beginRun).Scan(&beginID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'goal','{"text":"沿用旧根"}','open') RETURNING id`, beginRun).Scan(&beginGoal))

	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO work_items(run_id,key,title) VALUES($1,'old','历史方向') RETURNING id`, runID).Scan(&workID))
	var intentID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state,work_item_id) VALUES($1,'intent','{"summary":"有产出"}','open',$2) RETURNING id`, runID, workID).Scan(&intentID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,work_item_id,status) VALUES($1,'worker',$2,'completed') RETURNING id`, runID, workID).Scan(&sessionID))
	toolID := insertToolCall(t, pool, runID, &sessionID, "record_fact", []byte(`{}`), "saved", "success", 2)
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'fact',$2::jsonb,'verified') RETURNING id`, runID, fmt.Sprintf(`{"summary":"已观察","source_tool_call_id":"%s","verified_source":true}`, toolID)).Scan(&factID))
	var checkID, findingID, findingNode uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO check_results(run_id,asset_id,check_id,check_name,check_version,status,explanation) VALUES($1,$2,'hist','历史','1','needs_attention','旧证据') RETURNING id`, runID, assetID).Scan(&checkID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO findings(run_id,check_result_id,asset_id,title,category,severity,description,source_tool_call_id) VALUES($1,$2,$3,'历史发现','test','low','旧发现',$4) RETURNING id`, runID, checkID, assetID, toolID).Scan(&findingID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state,finding_id) VALUES($1,'finding','{"title":"历史发现"}','unverified',$2) RETURNING id`, runID, findingID).Scan(&findingNode))
	var unresolved uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO exploration_nodes(run_id,kind,payload,state) VALUES($1,'fact',$2::jsonb,'observed') RETURNING id`, runID, fmt.Sprintf(`{"summary":"无法归属","source_tool_call_id":"%s"}`, uuid.New())).Scan(&unresolved))

	var status string
	var epoch, modelCalls int
	require.NoError(t, pool.QueryRow(ctx, `SELECT status,current_epoch,(execution_input->'budget'->>'max_model_calls')::int FROM runs WHERE id=$1`, runID).Scan(&status, &epoch, &modelCalls))

	_, err = pool.Exec(ctx, `SELECT backfill_exploration_lineage()`)
	require.NoError(t, err)
	_, err = pool.Exec(ctx, `SELECT backfill_exploration_lineage()`)
	require.NoError(t, err)

	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1 AND kind='fact' AND state='origin'`, runID))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM exploration_nodes WHERE run_id=$1 AND kind='fact' AND state='origin'`, beginRun))
	var originID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `SELECT id FROM exploration_nodes WHERE run_id=$1 AND state='origin'`, runID).Scan(&originID))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='spawns'`, beginRun, beginID, beginGoal))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='spawns'`, runID, originID, goalID))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='derived_from'`, runID, originID, bareIntent))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='derived_from'`, runID, originID, emptySources))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND dst_id=$2`, runID, badArray))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND dst_id=$2`, runID, badElem))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='yields'`, runID, intentID, factID))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND src_id=$2 AND dst_id=$3 AND rel='yields'`, runID, intentID, findingNode))
	require.Equal(t, 0, countRows(t, pool, `SELECT count(*) FROM exploration_edges WHERE run_id=$1 AND rel='proves'`, runID))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM run_events WHERE run_id=$1 AND type='lineage_backfill_diagnostic' AND payload->>'node_id'=$2`, runID, badArray.String()))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM run_events WHERE run_id=$1 AND type='lineage_backfill_diagnostic' AND payload->>'node_id'=$2`, runID, badElem.String()))
	require.Equal(t, 1, countRows(t, pool, `SELECT count(*) FROM run_events WHERE run_id=$1 AND type='lineage_backfill_diagnostic' AND payload->>'node_id'=$2`, runID, unresolved.String()))

	var statusAfter string
	var epochAfter, modelAfter int
	require.NoError(t, pool.QueryRow(ctx, `SELECT status,current_epoch,(execution_input->'budget'->>'max_model_calls')::int FROM runs WHERE id=$1`, runID).Scan(&statusAfter, &epochAfter, &modelAfter))
	require.Equal(t, status, statusAfter)
	require.Equal(t, epoch, epochAfter)
	require.Equal(t, modelCalls, modelAfter)
	require.Equal(t, 12, modelAfter)
}

func countRows(t *testing.T, pool *pgxpool.Pool, query string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, pool.QueryRow(context.Background(), query, args...).Scan(&n))
	return n
}
