package test

import (
	"context"
	"encoding/json"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestReportFindingTool_WithTrafficRefs(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Traffic Refs Task",
		Objective: "Test report_finding tool with structured traffic refs",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	var assetID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO assets (namespace, kind, external_key, name, description)
		VALUES ('http', 'endpoint', 'ep-traffic-test', 'Vulnerable Search API', 'http://example.com/api/search')
		RETURNING id`).Scan(&assetID)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, '{}')`, run.ID, assetID)
	require.NoError(t, err)

	var tcID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call-search', 'get_endpoint', jsonb_build_object('asset_id',$2::text), '{"status_code":200}', 'success')
		RETURNING id`, run.ID, assetID).Scan(&tcID)
	require.NoError(t, err)

	tool := tools.NewReportFindingTool(run.ID, env.runService)

	baselineReceiptID := uuid.New().String()
	proofReceiptID := uuid.New().String()
	verificationReceiptID := uuid.New().String()

	args, _ := json.Marshal(map[string]any{
		"title":               "SQL Injection in Search Query parameter",
		"category":            "injection",
		"severity":            "high",
		"description":         "Unsanitized parameter passed to raw SQL query",
		"remediation":         "Use parameterized queries with prepared statements",
		"source_kind":         "empirical",
		"source_tool_call_id": tcID.String(),
		"asset_id":            assetID.String(),
		"traffic_refs": []map[string]string{
			{
				"receipt_id": baselineReceiptID,
				"role":       "baseline",
				"note":       "Normal query returns 200 OK with zero matches",
			},
			{
				"receipt_id": proofReceiptID,
				"role":       "proof",
				"note":       "Payload ' OR 1=1-- dumps all records",
			},
			{
				"receipt_id": verificationReceiptID,
				"role":       "verification",
				"note":       "Time-based delay sleep(3) confirms blind execution",
			},
		},
		"evidence": map[string]any{
			"parameter": "q",
		},
	})

	resStr, err := tool.Execute(ctx, args)
	require.NoError(t, err)

	var resMap map[string]interface{}
	err = json.Unmarshal([]byte(resStr), &resMap)
	require.NoError(t, err)
	require.NotNil(t, resMap["finding_id"])
	require.Equal(t, "empirical", resMap["source_kind"])
	require.Equal(t, "verified", resMap["verification_state"])

	// Check findings in DB and verify traffic_refs are in EvidenceRef
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)

	var evidenceData struct {
		Parameter   string `json:"parameter"`
		TrafficRefs []struct {
			ReceiptID string `json:"receipt_id"`
			Role      string `json:"role"`
			Note      string `json:"note"`
		} `json:"traffic_refs"`
	}
	err = json.Unmarshal(findings[0].EvidenceRef, &evidenceData)
	require.NoError(t, err)
	require.Equal(t, "q", evidenceData.Parameter)
	require.Len(t, evidenceData.TrafficRefs, 3)
	require.Equal(t, baselineReceiptID, evidenceData.TrafficRefs[0].ReceiptID)
	require.Equal(t, "baseline", evidenceData.TrafficRefs[0].Role)
	require.Equal(t, proofReceiptID, evidenceData.TrafficRefs[1].ReceiptID)
	require.Equal(t, "proof", evidenceData.TrafficRefs[1].Role)
	require.Equal(t, verificationReceiptID, evidenceData.TrafficRefs[2].ReceiptID)
	require.Equal(t, "verification", evidenceData.TrafficRefs[2].Role)
}
