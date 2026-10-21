package test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestP6_FindingClassificationAndSemantics(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P6 Classification Task",
		Objective: "Test empirical vs speculative findings",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	// Create a test asset and link to run scope
	var assetID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO assets (namespace, kind, external_key, name, description)
		VALUES ('http', 'endpoint', 'api-endpoint-1', 'Login API', 'http://example.com/api/login')
		RETURNING id`).Scan(&assetID)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, '{}')`, run.ID, assetID)
	require.NoError(t, err)

	// Create a valid successful tool call for empirical evidence
	var toolCallID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call-emp-1', 'get_endpoint', jsonb_build_object('asset_id',$2::text), '{"status_code":200}', 'success')
		RETURNING id`, run.ID, assetID).Scan(&toolCallID)
	require.NoError(t, err)

	// 1. Report Empirical Finding via REST with verified source tool call
	empBody, _ := json.Marshal(map[string]any{
		"asset_id":            assetID,
		"title":               "Unauthenticated Admin Authentication Bypass",
		"category":            "auth_bypass",
		"severity":            "critical",
		"source_kind":         "empirical",
		"source_tool_call_id": toolCallID,
		"description":         "Header X-Original-URL: /admin bypasses gateway authentication filter",
		"remediation":         "Upgrade gateway or restrict header override",
		"trigger_condition":   "GET /api/login with X-Original-URL: /admin",
		"reproduce_steps":     "curl -H 'X-Original-URL: /admin' http://example.com/api/login",
		"evidence_ref": map[string]any{
			"status_code": 200,
			"body":        "{\"role\": \"admin\", \"secret\": \"FLAG-1234\"}",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings", bytes.NewReader(empBody))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("reported finding failed (%d): %s", w.Code, w.Body.String())
	}
	require.Equal(t, http.StatusCreated, w.Code)

	var empFinding model.Finding
	err = json.Unmarshal(w.Body.Bytes(), &empFinding)
	require.NoError(t, err)
	require.Equal(t, "empirical", empFinding.SourceKind)
	require.Equal(t, "verified", empFinding.VerificationState)
	require.Equal(t, model.FindingStatusPendingReview, empFinding.Status)

	// Verify linked exploration node
	var expKind, expOrigin string
	var expFindingID uuid.UUID
	err = env.pool.QueryRow(ctx, `SELECT kind, origin, finding_id FROM exploration_nodes WHERE run_id=$1 AND finding_id=$2`, run.ID, empFinding.ID).Scan(&expKind, &expOrigin, &expFindingID)
	require.NoError(t, err)
	require.Equal(t, "finding", expKind)
	require.Equal(t, "empirical", expOrigin)
	require.Equal(t, empFinding.ID, expFindingID)

	// 2. Report Speculative Finding
	specBody, _ := json.Marshal(map[string]any{
		"asset_id":    assetID,
		"title":       "Outdated Library Suggests Potential Weakness",
		"category":    "cve_match",
		"severity":    "low",
		"source_kind": "speculative",
		"description": "Server header reports nginx/1.14.0",
		"remediation": "Update nginx to latest stable",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings", bytes.NewReader(specBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var specFinding model.Finding
	err = json.Unmarshal(w.Body.Bytes(), &specFinding)
	require.NoError(t, err)
	require.Equal(t, "speculative", specFinding.SourceKind)
	require.Equal(t, "unverified", specFinding.VerificationState)

	// 3. Human Review confirms the empirical finding
	expectedVer := empFinding.ReviewVersion
	revBody, _ := json.Marshal(map[string]any{
		"status":           "confirmed",
		"expected_version": expectedVer,
		"comment":          "Confirmed in staging environment by security engineer",
		"reviewer":         "alice_sec",
		"request_id":       uuid.New().String(),
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+empFinding.ID.String()+"/reviews", bytes.NewReader(revBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	// Verify that human review sets status='confirmed', but preserves source_kind='empirical' and verification_state='verified'
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, findings, 2)

	var reviewedEmp *model.Finding
	for i := range findings {
		if findings[i].ID == empFinding.ID {
			reviewedEmp = &findings[i]
			break
		}
	}
	require.NotNil(t, reviewedEmp)
	require.Equal(t, model.FindingStatusConfirmed, reviewedEmp.Status)
	require.Equal(t, "empirical", reviewedEmp.SourceKind)
	require.Equal(t, "verified", reviewedEmp.VerificationState)
	require.Equal(t, 1, reviewedEmp.ReviewVersion)
}

func TestP6_IndependentRetestLifecycleAndAuditPreservation(t *testing.T) {
	env := setupTestEnv(t, false)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P6 Retest Task",
		Objective: "Test independent verification and audit preservation",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	var assetID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO assets (namespace, kind, external_key, name, description)
		VALUES ('http', 'endpoint', 'ep-retest', 'Transfer API', 'http://example.com/api/transfer')
		RETURNING id`).Scan(&assetID)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, '{}')`, run.ID, assetID)
	require.NoError(t, err)

	var tcID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call-csrf', 'get_endpoint', jsonb_build_object('asset_id',$2::text), '{"status_code":200}', 'success')
		RETURNING id`, run.ID, assetID).Scan(&tcID)
	require.NoError(t, err)

	// 1. Create and confirm an empirical finding
	f, err := env.runService.ReportFinding(ctx, run.ID, service.ReportFindingInput{
		AssetID:          assetID,
		Title:            "Cross-Site Request Forgery in Balance Transfer",
		Category:         "csrf",
		Severity:         "high",
		SourceKind:       "empirical",
		SourceToolCallID: &tcID,
		Description:      "Missing CSRF token allows unauthorized POST /api/transfer",
	})
	require.NoError(t, err)

	// Human confirms finding
	revFinding, _, _, err := env.runService.ReviewFinding(ctx, run.ID, f.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		ExpectedVersion: &f.ReviewVersion,
		Reviewer:        "bob_admin",
		Comment:         "CSRF is reproducible with crafted form",
		RequestID:       uuid.New(),
	})
	require.NoError(t, err)
	require.Equal(t, model.FindingStatusConfirmed, revFinding.Status)

	// 2. Start Retest 1 via REST API
	retestReqBody, _ := json.Marshal(map[string]string{
		"notes": "Verifying fix deployment after PR #402 merged",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+f.ID.String()+"/retests", bytes.NewReader(retestReqBody))
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	var retestCreateRes struct {
		Retest  model.FindingRetest `json:"retest"`
		Session model.AgentSession  `json:"session"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &retestCreateRes)
	require.NoError(t, err)
	retest1 := retestCreateRes.Retest
	require.Equal(t, "running", retest1.Status)
	require.Equal(t, "verifier", retestCreateRes.Session.Role)

	// 3. Concurrency check: attempting another retest while retest1 is running returns 409 Conflict
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+f.ID.String()+"/retests", bytes.NewReader(retestReqBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusConflict, w.Code)

	// 4. Stop Retest 1
	stopBody, _ := json.Marshal(map[string]string{
		"reason": "Target host rebooting during maintenance",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+f.ID.String()+"/retests/"+retest1.ID.String()+"/stop", bytes.NewReader(stopBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	// 5. Start Retest 2 (now succeeds because retest 1 is stopped)
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+f.ID.String()+"/retests", bytes.NewReader(retestReqBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)

	err = json.Unmarshal(w.Body.Bytes(), &retestCreateRes)
	require.NoError(t, err)
	retest2 := retestCreateRes.Retest

	// 6. Text alone cannot prove a fix; complete as inconclusive.
	finishBody, _ := json.Marshal(map[string]string{
		"verdict":  "inconclusive",
		"evidence": "POST /api/transfer without anti-CSRF token now returns 403 Forbidden with code 'CSRF_TOKEN_INVALID'",
		"summary":  "Anti-CSRF SameSite=Strict cookie and Synchronizer Token Pattern verified working",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/runs/"+run.ID.String()+"/findings/"+f.ID.String()+"/retests/"+retest2.ID.String()+"/finish", bytes.NewReader(finishBody))
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var finishedRetest model.FindingRetest
	err = json.Unmarshal(w.Body.Bytes(), &finishedRetest)
	require.NoError(t, err)
	require.Equal(t, "completed", finishedRetest.Status)
	require.Equal(t, "inconclusive", *finishedRetest.Verdict)

	// 7. Verify Finding State & Audit History Integrity
	finalFindings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, finalFindings, 1)

	ff := finalFindings[0]
	// Verification state reflects retest
	require.Equal(t, "retested", ff.VerificationState)
	require.NotNil(t, ff.RetestVerdict)
	require.Equal(t, "inconclusive", *ff.RetestVerdict)
	require.NotNil(t, ff.RetestNotes)
	require.Contains(t, *ff.RetestNotes, "Synchronizer Token Pattern")

	// Human review status is PRESERVED (still confirmed), version is preserved
	require.Equal(t, model.FindingStatusConfirmed, ff.Status)
	require.Equal(t, 1, ff.ReviewVersion)

	// Audit history in finding_reviews is intact
	reviews, err := env.runService.ListFindingReviews(ctx, run.ID, f.ID)
	require.NoError(t, err)
	require.Len(t, reviews, 1)
	require.Equal(t, "bob_admin", reviews[0].Reviewer)
	require.Equal(t, model.FindingStatusConfirmed, reviews[0].ToStatus)

	// Retests history contains both retest 1 (stopped) and retest 2 (completed)
	req = httptest.NewRequest(http.MethodGet, "/api/runs/"+run.ID.String()+"/findings/"+f.ID.String()+"/retests", nil)
	w = httptest.NewRecorder()
	env.router.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var retestsRes struct {
		Retests []model.FindingRetest `json:"retests"`
	}
	err = json.Unmarshal(w.Body.Bytes(), &retestsRes)
	require.NoError(t, err)
	require.Len(t, retestsRes.Retests, 2)
	require.Equal(t, "stopped", retestsRes.Retests[0].Status)
	require.Equal(t, "completed", retestsRes.Retests[1].Status)
	require.Equal(t, "inconclusive", *retestsRes.Retests[1].Verdict)
}

func TestP6_ReportFindingTool_Integration(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "P6 Tool Task",
		Objective: "Test report_finding tool execution",
	})
	require.NoError(t, err)

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	require.NoError(t, err)

	var assetID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO assets (namespace, kind, external_key, name, description)
		VALUES ('http', 'endpoint', 'ep-tool', 'Ping API', 'http://example.com/api/ping')
		RETURNING id`).Scan(&assetID)
	require.NoError(t, err)
	_, err = env.pool.Exec(ctx, `INSERT INTO run_assets (run_id, asset_id, snapshot) VALUES ($1, $2, '{}')`, run.ID, assetID)
	require.NoError(t, err)

	var tcID uuid.UUID
	err = env.pool.QueryRow(ctx, `
		INSERT INTO agent_tool_calls (id, run_id, call_id, tool_name, arguments, output, status)
		VALUES (gen_random_uuid(), $1, 'call-ping', 'get_endpoint', jsonb_build_object('asset_id',$2::text), '{"status_code":200}', 'success')
		RETURNING id`, run.ID, assetID).Scan(&tcID)
	require.NoError(t, err)

	tool := tools.NewReportFindingTool(run.ID, env.runService)
	require.Equal(t, "report_finding", tool.Name())

	args, _ := json.Marshal(map[string]any{
		"title":               "Command Injection in ping utility",
		"category":            "command_injection",
		"severity":            "critical",
		"description":         "User input passed unsanitized to system shell",
		"remediation":         "Use exec.Command with separate arguments array",
		"source_kind":         "empirical",
		"source_tool_call_id": tcID.String(),
		"asset_id":            assetID.String(),
		"evidence": map[string]any{
			"payload": "; id",
			"output":  "uid=0(root) gid=0(root)",
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

	// Verify finding exists in database
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	require.NoError(t, err)
	require.Len(t, findings, 1)
	require.Equal(t, "Command Injection in ping utility", findings[0].Title)
	require.Equal(t, "empirical", findings[0].SourceKind)
}
