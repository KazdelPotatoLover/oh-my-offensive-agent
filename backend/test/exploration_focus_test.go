package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestExplorationFocus_ConfigurationAuditRequiresExplicitCapability(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()
	for _, tc := range []struct {
		name, config, profile string
		allowed               bool
	}{
		{"default exploration", `{"exploration":true,"provider":"mock"}`, "explore", false},
		{"disabled audit", `{"exploration":true,"provider":"mock","profiles":[{"name":"audit","version":"1","tools":[{"name":"run_security_check","disabled":true}]}]}`, "audit", false},
		{"explicit audit", `{"exploration":true,"provider":"mock","profiles":[{"name":"audit","version":"1","tools":[{"name":"run_security_check"}]}]}`, "audit", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
				Name: tc.name, Objective: "验证授权入口的真实影响", Scope: json.RawMessage(`{"endpoints":["http_login"]}`), Config: json.RawMessage(tc.config),
			})
			require.NoError(t, err)
			run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
			require.NoError(t, err)
			profiles, err := model.ProfilesFromInput(run.ExecutionInput)
			require.NoError(t, err)
			if tc.profile == "explore" {
				profile, err := model.ProfileByName(run.ExecutionInput, "explore")
				require.NoError(t, err)
				names := map[string]bool{}
				for _, tool := range profile.Tools {
					names[tool.Name] = !tool.Disabled
				}
				require.True(t, names["http_request"])
				require.True(t, names["search_http_traffic"])
				for _, p := range profiles {
					for _, tool := range p.Tools {
						require.NotEqual(t, "run_security_check", tool.Name)
					}
				}
			}
			_, err = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
			require.NoError(t, err)
			assets, err := env.runService.ListAssetsByRun(ctx, run.ID)
			require.NoError(t, err)
			require.NotEmpty(t, assets)
			asset := assets[0].ID
			items, err := env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
				{Key: "entry", Title: "验证入口", Objective: "依据目标响应推进验证", WorkType: "intent", AssetIDs: []uuid.UUID{asset}, InputData: map[string]any{"profile": tc.profile}},
				{Key: "audit", Title: "可选配置审查", Objective: "显式请求的配置审查", WorkType: "run_security_check", AssetIDs: []uuid.UUID{asset}, InputData: map[string]any{"asset_id": asset.String(), "check_id": "http_security_headers"}},
			})
			if tc.allowed {
				require.NoError(t, err)
				require.Len(t, items, 2)
			} else {
				require.ErrorIs(t, err, model.ErrInvalidProfile)
				persisted, err := env.runService.ListWorkItemsByRun(ctx, run.ID)
				require.NoError(t, err)
				require.Empty(t, persisted, "a disallowed audit must reject the entire plan atomically")
			}
		})
	}
}

func TestExplorationFocus_RealResponseDoesNotTurnBaselineIntoVulnerability(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/download" && r.URL.Query().Get("file") == "../private.txt" {
			_, _ = w.Write([]byte("private-fixture-data: account=fixture-user"))
			return
		}
		_, _ = w.Write([]byte("public-fixture-data"))
	}))
	defer app.Close()
	u, err := url.Parse(app.URL)
	require.NoError(t, err)
	policy := fmt.Sprintf(`{"network_policy":{"template":"active","allowed_methods":["GET","POST"],"origins":[{"scheme":"http","host":"127.0.0.1","port":%q,"path_mode":"same_origin","allow_private":true}]}}`, u.Port())
	target := app.URL + "/download"
	runID, assetID, workID := seedHTTPAsset(t, pool, policy, target, "focus-private-file")
	var sessionID, attemptID uuid.UUID
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,status,work_item_id) VALUES($1,'worker','running',$2) RETURNING id`, runID, workID).Scan(&sessionID))
	require.NoError(t, pool.QueryRow(ctx, `INSERT INTO attempts(work_item_id,attempt_number,status,session_id) VALUES($1,1,'running',$2) RETURNING id`, workID, sessionID).Scan(&attemptID))
	svc := service.NewRunService(pool)
	args, _ := json.Marshal(map[string]any{"asset_id": assetID, "method": "GET", "query": map[string]string{"file": "../private.txt"}})
	callID := insertToolCall(t, pool, runID, &sessionID, "http_request", args, "", "pending", 1)
	output, err := tools.NewHTTPRequestTool(runID, workID, attemptID, sessionID, svc, netcap.NewTransport(netcap.NewStore(pool))).Execute(ctx, args)
	require.NoError(t, err)
	require.Contains(t, output, "private-fixture-data")
	_, err = pool.Exec(ctx, `UPDATE agent_tool_calls SET status='success',output=$2 WHERE id=$1`, callID, output)
	require.NoError(t, err)

	for _, category := range []string{"security_misconfiguration", "configuration_risk", "http_security_headers", "version_disclosure", "improper_error_handling"} {
		for _, kind := range []string{"empirical", "speculative"} {
			_, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
				AssetID: assetID, Title: "页面基线观察", Category: category, Severity: "low", Description: "真实响应未配置加固项",
				SourceKind: kind, SourceToolCallID: &callID,
			})
			require.ErrorIs(t, err, service.ErrInvalidEvidence)
		}
	}
	findings, err := svc.ListFindingsByRun(ctx, runID)
	require.NoError(t, err)
	require.Empty(t, findings)
	_, err = svc.RecordFact(ctx, runID, "下载响应的配置基线", "同站点基线合并记录", "observed", callID.String(), nil, []uuid.UUID{assetID})
	require.NoError(t, err, "baseline observations still have a supported writeback path")

	findingTool := tools.NewReportFindingTool(runID, svc)
	leadArgs, err := json.Marshal(map[string]any{
		"asset_id": assetID, "title": "待验证授权边界", "category": "auth_bypass",
		"severity": "low", "description": "尚未证实授权边界",
	})
	require.NoError(t, err)
	require.ErrorContains(t, findingTool.Validate(leadArgs), "source_kind must explicitly")
	var lead map[string]any
	require.NoError(t, json.Unmarshal(leadArgs, &lead))
	lead["source_kind"] = "speculative"
	leadArgs, err = json.Marshal(lead)
	require.NoError(t, err)
	require.NoError(t, findingTool.Validate(leadArgs))

	suspected, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "另一个具体待验证线索", Category: "auth_bypass", Severity: "low", Description: "尚未证实授权边界",
	})
	require.NoError(t, err)
	require.Equal(t, "speculative", suspected.SourceKind, "omitting classification must never imply a reproduced exploit")
	require.Equal(t, "unverified", suspected.VerificationState)
	actual, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "下载接口可读取非公开文件", Category: "path_traversal", Severity: "high",
		Description: "实际响应返回私有 fixture 文件内容", SourceKind: "empirical", SourceToolCallID: &callID,
		TriggerCondition: "file=../private.txt", ReproduceSteps: "GET /download?file=../private.txt 返回 private-fixture-data",
	})
	require.NoError(t, err)
	require.Equal(t, "verified", actual.VerificationState)
	findings, err = svc.ListFindingsByRun(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, actual.ID, findings[0].ID, "a later real vulnerability precedes low-value leads")
	report, err := svc.GetFindingReport(ctx, runID)
	require.NoError(t, err)
	require.Equal(t, actual.ID, report.Findings[0].Finding.ID)
	summary := orchestration.GenerateProgrammaticSummary(ctx, runID, svc, model.CauseCompleted)
	require.NotContains(t, summary, "Unchecked Assets")
	require.NotContains(t, summary, "Deterministic Check Results")
	require.Contains(t, summary, actual.Title)
	require.Contains(t, summary, "empirical|verified")
}
