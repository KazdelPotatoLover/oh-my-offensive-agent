package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/fixture"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

func setupPhase9Env(t *testing.T) (*TestEnv, *fixture.Server, func()) {
	t.Helper()
	env := setupTestEnv(t)
	srv, err := fixture.NewServer("0")
	if err != nil {
		t.Fatalf("failed to create fixture server: %v", err)
	}
	srv.Start()

	oldPort := os.Getenv("FIXTURE_PORT")
	_ = os.Setenv("FIXTURE_PORT", srv.Port())

	cleanup := func() {
		_ = srv.Close()
		if oldPort != "" {
			_ = os.Setenv("FIXTURE_PORT", oldPort)
		} else {
			_ = os.Unsetenv("FIXTURE_PORT")
		}
		env.teardown()
	}
	return env, srv, cleanup
}

// helper to create a run with a candidate finding
func createRunAndFinding(t *testing.T, env *TestEnv, srv *fixture.Server, taskName string) (*model.Run, *model.Finding, *model.CheckResult, *model.AgentToolCall) {
	t.Helper()
	ctx := context.Background()

	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      taskName,
		Objective: "Generate candidate finding for review testing",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	run, _, err := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)

	runAssets, err := env.runService.ListAssetsByRun(ctx, run.ID)
	if err != nil || len(runAssets) == 0 {
		t.Fatalf("list run assets: %v", err)
	}
	homeAsset := runAssets[0]

	// 1. inspect_http_endpoint to get HTTP evidence
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect_home",
			Title:     "Inspect Home",
			Objective: "Fetch home",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	if err != nil {
		t.Fatalf("submit inspect plan: %v", err)
	}

	wiInspect, attInspect, sessInspect, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim inspect item: %v", err)
	}

	httpTool := tools.NewGetEndpointTool(run.ID, wiInspect.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})

	tcInspect, err := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessInspect.ID, nil, "call_get_home", "get_endpoint", getArgs)
	if err != nil {
		t.Fatalf("create tool call: %v", err)
	}
	getOut, err := httpTool.Execute(ctx, getArgs)
	if err != nil {
		t.Fatalf("execute get_endpoint: %v", err)
	}
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcInspect.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiInspect.ID, attInspect.ID, sessInspect.ID, true, json.RawMessage(`{"summary":"fetched"}`), "completed")

	// 2. run_security_check on homeAsset
	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:          "check_home",
			Title:        "Check Home Headers",
			Objective:    "Check headers on home",
			WorkType:     "run_security_check",
			AssetIDs:     []uuid.UUID{homeAsset.ID},
			InputData:    map[string]interface{}{"asset_id": homeAsset.ID.String(), "check_id": "http_x_content_type_options"},
			Dependencies: []string{"inspect_home"},
		},
	})
	if err != nil {
		t.Fatalf("submit check plan: %v", err)
	}

	wiCheck, attCheck, sessCheck, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim check item: %v", err)
	}

	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiCheck.ID, attCheck.ID, sessCheck.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_x_content_type_options",
	})
	checkOut, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("execute check tool: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiCheck.ID, attCheck.ID, sessCheck.ID, true, json.RawMessage(checkOut), "completed")

	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	if err != nil || len(findings) == 0 {
		t.Fatalf("expected candidate findings, got err: %v, count: %d", err, len(findings))
	}
	checkResults, err := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if err != nil || len(checkResults) == 0 {
		t.Fatalf("expected check results, got err: %v, count: %d", err, len(checkResults))
	}

	tcInspect.Output = getOut
	tcInspect.Status = "success"
	return run, &findings[0], &checkResults[0], tcInspect
}

// 1. 本地 fixture 产生候选发现；确认后刷新仍显示正确状态、理由、审核人和历史，原 CheckResult/证据不变。
func TestPhase9_NormalTransitionAndTraceability(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	run, finding, origCR, origTC := createRunAndFinding(t, env, srv, "Phase 9 Normal Transition")

	reqID := uuid.New()
	expectedVer := 0
	reviewPayload := map[string]interface{}{
		"status":           "confirmed",
		"comment":          "确认缺失 X-Content-Type-Options: nosniff 响应头",
		"reviewer":         "Alice",
		"expected_version": expectedVer,
		"request_id":       reqID,
	}
	body, _ := json.Marshal(reviewPayload)

	req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	if w.Code != http.StatusCreated && w.Code != http.StatusOK {
		t.Fatalf("expected 200 or 201, got %d: %s", w.Code, w.Body.String())
	}

	var resp struct {
		Finding  model.Finding       `json:"finding"`
		Review   model.FindingReview `json:"review"`
		Replayed bool                `json:"replayed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}

	// Verify Finding state
	if resp.Finding.Status != model.FindingStatusConfirmed {
		t.Fatalf("expected finding status confirmed, got %s", resp.Finding.Status)
	}
	if resp.Finding.ReviewVersion != 1 {
		t.Fatalf("expected review version 1, got %d", resp.Finding.ReviewVersion)
	}
	if resp.Finding.LastReviewer == nil || *resp.Finding.LastReviewer != "Alice" {
		t.Fatalf("expected last reviewer Alice, got %v", resp.Finding.LastReviewer)
	}
	if resp.Finding.LastReviewComment == nil || !strings.Contains(*resp.Finding.LastReviewComment, "确认缺失") {
		t.Fatalf("expected comment, got %v", resp.Finding.LastReviewComment)
	}
	if resp.Replayed {
		t.Fatalf("expected replayed to be false on first call")
	}

	// Verify Review audit trail
	if resp.Review.FromStatus != model.FindingStatusPendingReview || resp.Review.ToStatus != model.FindingStatusConfirmed {
		t.Fatalf("expected pending_review -> confirmed, got %s -> %s", resp.Review.FromStatus, resp.Review.ToStatus)
	}
	if resp.Review.FromVersion != 0 || resp.Review.ToVersion != 1 {
		t.Fatalf("expected version 0 -> 1, got %d -> %d", resp.Review.FromVersion, resp.Review.ToVersion)
	}
	if resp.Review.RequestID != reqID {
		t.Fatalf("expected request_id %s, got %s", reqID, resp.Review.RequestID)
	}

	// Re-query Findings API
	reqList := httptest.NewRequest("GET", fmt.Sprintf("/api/runs/%s/findings", run.ID), nil)
	wList := httptest.NewRecorder()
	env.router.ServeHTTP(wList, reqList)
	if wList.Code != http.StatusOK {
		t.Fatalf("list findings failed: %s", wList.Body.String())
	}
	var findingsResp struct {
		Findings []model.Finding `json:"findings"`
	}
	_ = json.Unmarshal(wList.Body.Bytes(), &findingsResp)
	if len(findingsResp.Findings) == 0 || findingsResp.Findings[0].Status != model.FindingStatusConfirmed {
		t.Fatalf("expected confirmed finding in list, got %+v", findingsResp.Findings)
	}

	// Query Review History API
	reqHist := httptest.NewRequest("GET", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), nil)
	wHist := httptest.NewRecorder()
	env.router.ServeHTTP(wHist, reqHist)
	if wHist.Code != http.StatusOK {
		t.Fatalf("list reviews failed: %s", wHist.Body.String())
	}
	var histResp struct {
		Reviews []model.FindingReview `json:"reviews"`
	}
	_ = json.Unmarshal(wHist.Body.Bytes(), &histResp)
	if len(histResp.Reviews) != 1 {
		t.Fatalf("expected 1 review in history, got %d", len(histResp.Reviews))
	}
	if histResp.Reviews[0].Reviewer != "Alice" || histResp.Reviews[0].ToVersion != 1 {
		t.Fatalf("unexpected review record: %+v", histResp.Reviews[0])
	}

	// Check that original CheckResult and raw HTTP evidence remain unchanged
	checkResults, _ := env.runService.ListCheckResultsByRun(context.Background(), run.ID)
	if len(checkResults) == 0 || checkResults[0].ID != origCR.ID || checkResults[0].Status != origCR.Status {
		t.Fatalf("CheckResult changed unexpectedly")
	}
	toolCalls, _ := env.runService.ListAgentToolCalls(context.Background(), run.ID)
	foundOrigTC := false
	for _, tc := range toolCalls {
		if tc.ID == origTC.ID && tc.Output == origTC.Output {
			foundOrigTC = true
			break
		}
	}
	if !foundOrigTC {
		t.Fatalf("original HTTP evidence tool call was mutated or missing")
	}
}

// 2. 详细输入错误分类测试：Unicode 长度（100/2000）、负版本、缺失版本、超大请求体 413、非法 JSON 400
func TestPhase9_InputValidationClassification(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	run, finding, _, _ := createRunAndFinding(t, env, srv, "Phase 9 Validation Task")

	v0 := 0
	negOne := -1

	// Strings for boundary testing
	reviewer100Chars := strings.Repeat("审", 100)
	reviewer101Chars := strings.Repeat("审", 101)
	comment2000Chars := strings.Repeat("注", 2000)
	comment2001Chars := strings.Repeat("注", 2001)

	tests := []struct {
		name         string
		runID        uuid.UUID
		findingID    uuid.UUID
		status       string
		comment      string
		reviewer     string
		version      *int
		requestID    *uuid.UUID
		rawBody      string // optional raw body override
		expectedCode int
	}{
		{
			name:         "reviewer 100 unicode chars (boundary valid)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "valid comment",
			reviewer:     reviewer100Chars,
			version:      &v0,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusCreated,
		},
		{
			name:         "reviewer 101 unicode chars exceeds limit (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "valid comment",
			reviewer:     reviewer101Chars,
			version:      &v0,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "comment 2000 unicode chars (boundary valid)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "false_positive",
			comment:      comment2000Chars,
			reviewer:     "Bob",
			version:      ptr(1), // following the previous successful review
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusCreated,
		},
		{
			name:         "comment 2001 unicode chars exceeds limit (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "ignored",
			comment:      comment2001Chars,
			reviewer:     "Bob",
			version:      ptr(2),
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "negative expected_version (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "valid comment",
			reviewer:     "Alice",
			version:      &negOne,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "missing expected_version nil (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "valid comment",
			reviewer:     "Alice",
			version:      nil,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "empty comment (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "   ",
			reviewer:     "Alice",
			version:      &v0,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "empty reviewer (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "valid comment",
			reviewer:     "   ",
			version:      &v0,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "invalid json body (400)",
			runID:        run.ID,
			findingID:    finding.ID,
			rawBody:      `{"status": "confirmed", "broken_json`,
			expectedCode: http.StatusBadRequest,
		},
		{
			name:         "payload exceeds 32KB limit (413 Payload Too Large)",
			runID:        run.ID,
			findingID:    finding.ID,
			rawBody:      `{"status":"confirmed","comment":"` + strings.Repeat("A", 35*1024) + `"}`,
			expectedCode: http.StatusRequestEntityTooLarge, // 413
		},
		{
			name:         "wrong run scope (404)",
			runID:        uuid.New(),
			findingID:    finding.ID,
			status:       "confirmed",
			comment:      "valid comment",
			reviewer:     "Alice",
			version:      &v0,
			requestID:    ptr(uuid.New()),
			expectedCode: http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			if tc.rawBody != "" {
				body = []byte(tc.rawBody)
			} else {
				payload := map[string]interface{}{
					"status":           tc.status,
					"comment":          tc.comment,
					"reviewer":         tc.reviewer,
					"expected_version": tc.version,
				}
				if tc.requestID != nil {
					payload["request_id"] = *tc.requestID
				}
				body, _ = json.Marshal(payload)
			}

			req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", tc.runID, tc.findingID), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, req)

			if w.Code != tc.expectedCode {
				t.Fatalf("expected code %d, got %d: %s", tc.expectedCode, w.Code, w.Body.String())
			}
		})
	}
}

// 3. 连续转换：pending_review (v0) -> confirmed (v1) -> false_positive (v2) -> ignored (v3) -> pending_review (v4)
func TestPhase9_MultipleSequentialTransitions(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	run, finding, _, _ := createRunAndFinding(t, env, srv, "Phase 9 Sequential Transitions")

	transitions := []struct {
		targetStatus    model.FindingStatus
		reviewer        string
		comment         string
		expectedVersion int
	}{
		{model.FindingStatusConfirmed, "Alice", "Confirmed missing header", 0},
		{model.FindingStatusFalsePositive, "Bob", "Corrected: internal non-browsable service", 1},
		{model.FindingStatusIgnored, "Charlie", "Changed to ignored: intentional legacy compat", 2},
		{model.FindingStatusPendingReview, "David", "Reopened for re-triage by team lead", 3},
	}

	for i, tr := range transitions {
		reqID := uuid.New()
		body, _ := json.Marshal(map[string]interface{}{
			"status":           tr.targetStatus,
			"comment":          tr.comment,
			"reviewer":         tr.reviewer,
			"expected_version": tr.expectedVersion,
			"request_id":       reqID,
		})
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)

		if w.Code != http.StatusCreated && w.Code != http.StatusOK {
			t.Fatalf("transition %d (%s) failed with code %d: %s", i, tr.targetStatus, w.Code, w.Body.String())
		}
	}

	// Verify audit history from DB
	reviews, err := env.runService.ListFindingReviews(context.Background(), run.ID, finding.ID)
	if err != nil {
		t.Fatalf("list reviews: %v", err)
	}
	if len(reviews) != 4 {
		t.Fatalf("expected 4 review records, got %d", len(reviews))
	}

	// Assert version sequence is strictly continuous 0->1, 1->2, 2->3, 3->4
	for i, rev := range reviews {
		expectedFromVer := i
		expectedToVer := i + 1
		if rev.FromVersion != expectedFromVer || rev.ToVersion != expectedToVer {
			t.Fatalf("review %d expected version %d->%d, got %d->%d", i, expectedFromVer, expectedToVer, rev.FromVersion, rev.ToVersion)
		}
		if rev.Reviewer != transitions[i].reviewer {
			t.Fatalf("review %d expected reviewer %s, got %s", i, transitions[i].reviewer, rev.Reviewer)
		}
		if rev.Comment != transitions[i].comment {
			t.Fatalf("review %d expected comment %s, got %s", i, transitions[i].comment, rev.Comment)
		}
	}

	// Final finding state should be pending_review with review_version 4
	findings, err := env.runService.ListFindingsByRun(context.Background(), run.ID)
	if err != nil || len(findings) == 0 {
		t.Fatalf("list findings: %v", err)
	}
	if findings[0].Status != model.FindingStatusPendingReview || findings[0].ReviewVersion != 4 {
		t.Fatalf("expected final finding status pending_review v4, got %s v%d", findings[0].Status, findings[0].ReviewVersion)
	}
	if *findings[0].LastReviewer != "David" {
		t.Fatalf("expected last reviewer David, got %s", *findings[0].LastReviewer)
	}
}

// 4. 两个请求使用同一 expected_version 并发修改，仅一个成功，另一个 409；不存在历史与当前状态分离。
func TestPhase9_ConcurrentVersionConflict_409(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	run, finding, _, _ := createRunAndFinding(t, env, srv, "Phase 9 Concurrency Conflict")

	v0 := 0
	type result struct {
		code int
		body string
	}
	resChan := make(chan result, 2)
	startBarrier := make(chan struct{})

	var wg sync.WaitGroup
	wg.Add(2)

	go func() {
		defer wg.Done()
		<-startBarrier
		body, _ := json.Marshal(map[string]interface{}{
			"status":           "confirmed",
			"comment":          "Alice confirms finding",
			"reviewer":         "Alice",
			"expected_version": v0,
			"request_id":       uuid.New(),
		})
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		resChan <- result{code: w.Code, body: w.Body.String()}
	}()

	go func() {
		defer wg.Done()
		<-startBarrier
		body, _ := json.Marshal(map[string]interface{}{
			"status":           "false_positive",
			"comment":          "Bob marks as false positive",
			"reviewer":         "Bob",
			"expected_version": v0,
			"request_id":       uuid.New(),
		})
		req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		resChan <- result{code: w.Code, body: w.Body.String()}
	}()

	// Release both goroutines simultaneously
	close(startBarrier)
	wg.Wait()
	close(resChan)

	var codes []int
	for r := range resChan {
		codes = append(codes, r.code)
	}

	successCount := 0
	conflictCount := 0
	for _, c := range codes {
		if c == http.StatusOK || c == http.StatusCreated {
			successCount++
		} else if c == http.StatusConflict {
			conflictCount++
		}
	}

	if successCount != 1 || conflictCount != 1 {
		t.Fatalf("expected exactly 1 success (200/201) and 1 conflict (409), got codes: %v", codes)
	}

	// Verify that exactly 1 review record was created
	reviews, err := env.runService.ListFindingReviews(context.Background(), run.ID, finding.ID)
	if err != nil {
		t.Fatalf("list reviews: %v", err)
	}
	if len(reviews) != 1 {
		t.Fatalf("expected exactly 1 review record in database, got %d", len(reviews))
	}
}

// 5. 同 request_id 幂等内容比较包含 expected_version 与原 from_version；同键仅改版本返回 409，原请求即使版本已过期仍可幂等重放。
func TestPhase9_IdempotencyAndReplay(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	run, finding, _, _ := createRunAndFinding(t, env, srv, "Phase 9 Idempotency Task")

	reqID1 := uuid.New()
	v0 := 0

	// 5a. First review with reqID1 (from_version 0)
	body1, _ := json.Marshal(map[string]interface{}{
		"status":           "confirmed",
		"comment":          "Initial confirmation comment",
		"reviewer":         "Alice",
		"expected_version": v0,
		"request_id":       reqID1,
	})
	req1 := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body1))
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()
	env.router.ServeHTTP(w1, req1)
	if w1.Code != http.StatusCreated {
		t.Fatalf("first review failed: %s", w1.Body.String())
	}

	// 5b. Exact same replay with reqID1 (same content, same expected_version 0)
	req1Replay := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body1))
	req1Replay.Header.Set("Content-Type", "application/json")
	w1Replay := httptest.NewRecorder()
	env.router.ServeHTTP(w1Replay, req1Replay)
	if w1Replay.Code != http.StatusOK {
		t.Fatalf("idempotent replay failed, expected 200 OK: %s", w1Replay.Body.String())
	}
	var replayResp struct {
		Replayed bool `json:"replayed"`
	}
	_ = json.Unmarshal(w1Replay.Body.Bytes(), &replayResp)
	if !replayResp.Replayed {
		t.Fatalf("expected replayed == true")
	}

	// Verify only 1 review record in DB
	revs, _ := env.runService.ListFindingReviews(context.Background(), run.ID, finding.ID)
	if len(revs) != 1 {
		t.Fatalf("expected 1 review record, got %d", len(revs))
	}

	// 5c. Same reqID1 but different expected_version (e.g. 1 instead of 0) -> 409 conflict
	bodyDiffVer, _ := json.Marshal(map[string]interface{}{
		"status":           "confirmed",
		"comment":          "Initial confirmation comment",
		"reviewer":         "Alice",
		"expected_version": 1,
		"request_id":       reqID1,
	})
	reqDiffVer := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(bodyDiffVer))
	reqDiffVer.Header.Set("Content-Type", "application/json")
	wDiffVer := httptest.NewRecorder()
	env.router.ServeHTTP(wDiffVer, reqDiffVer)
	if wDiffVer.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict when same request_id changes expected_version, got %d: %s", wDiffVer.Code, wDiffVer.Body.String())
	}

	// 5d. Same reqID1 but different comment -> 409 conflict
	bodyDiff, _ := json.Marshal(map[string]interface{}{
		"status":           "confirmed",
		"comment":          "Different comment with same request_id",
		"reviewer":         "Alice",
		"expected_version": v0,
		"request_id":       reqID1,
	})
	reqDiff := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(bodyDiff))
	reqDiff.Header.Set("Content-Type", "application/json")
	wDiff := httptest.NewRecorder()
	env.router.ServeHTTP(wDiff, reqDiff)
	if wDiff.Code != http.StatusConflict {
		t.Fatalf("expected 409 conflict on different content with same request_id, got %d: %s", wDiff.Code, wDiff.Body.String())
	}

	// 5e. Advance finding to v2 with a new reqID2
	v1 := 1
	reqID2 := uuid.New()
	body2, _ := json.Marshal(map[string]interface{}{
		"status":           "false_positive",
		"comment":          "Advancing to v2",
		"reviewer":         "Bob",
		"expected_version": v1,
		"request_id":       reqID2,
	})
	req2 := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	env.router.ServeHTTP(w2, req2)
	if w2.Code != http.StatusCreated {
		t.Fatalf("advance to v2 failed: %s", w2.Body.String())
	}

	// 5f. Re-send reqID1 with original payload (expected_version 0):
	// "原请求即使版本已过期仍可幂等重放，历史请求重试不能重做转换。"
	// It should return the original review without reverting status back to confirmed!
	req1Late := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body1))
	req1Late.Header.Set("Content-Type", "application/json")
	w1Late := httptest.NewRecorder()
	env.router.ServeHTTP(w1Late, req1Late)
	if w1Late.Code != http.StatusOK {
		t.Fatalf("late replay of reqID1 failed: %d, %s", w1Late.Code, w1Late.Body.String())
	}

	// Verify finding is still at v2 false_positive
	findings, _ := env.runService.ListFindingsByRun(context.Background(), run.ID)
	if findings[0].Status != model.FindingStatusFalsePositive || findings[0].ReviewVersion != 2 {
		t.Fatalf("late replay incorrectly reverted finding status! Got status %s, ver %d", findings[0].Status, findings[0].ReviewVersion)
	}
}

// 6a. 事务前取消 context：证明未执行时不写入
func TestPhase9_PreTransactionCancelledContextNoOp(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	run, finding, _, _ := createRunAndFinding(t, env, srv, "Phase 9 Cancelled Context Task")

	cancellingCtx, cancel := context.WithCancel(context.Background())
	cancel() // cancel context before calling service

	v0 := 0
	_, _, _, err := env.runService.ReviewFinding(cancellingCtx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Cancelled context call",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})
	if err == nil {
		t.Fatalf("expected error on cancelled context, got nil")
	}

	// Verify no writes
	reviews, _ := env.runService.ListFindingReviews(context.Background(), run.ID, finding.ID)
	if len(reviews) != 0 {
		t.Fatalf("expected 0 reviews after cancelled call, got %d", len(reviews))
	}
	findings, _ := env.runService.ListFindingsByRun(context.Background(), run.ID)
	if findings[0].ReviewVersion != 0 || findings[0].Status != model.FindingStatusPendingReview {
		t.Fatalf("expected finding untouched, got %s v%d", findings[0].Status, findings[0].ReviewVersion)
	}
}

// 6b. 真正的回滚测试：通过临时 BEFORE UPDATE 触发器，在确认同事务审核 INSERT 已发生后再抛出特定错误，证明部分写入会完全回滚。
func TestPhase9_MidTransactionRollback_TriggerInjected(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	ctx := context.Background()
	run, finding, origCR, origTC := createRunAndFinding(t, env, srv, "Phase 9 Mid-Tx Rollback Task")

	// Create temporary trigger function on findings:
	// When target finding is updated, verify that finding_reviews has ALREADY inserted the review in this tx!
	// If it has, raise injected_mid_transaction_failure. If not, raise unexpected_trigger_state.
	triggerSQL := fmt.Sprintf(`
		CREATE OR REPLACE FUNCTION test_fail_finding_review_trigger() RETURNS TRIGGER AS $$
		BEGIN
			IF NEW.id = '%s' THEN
				IF EXISTS (SELECT 1 FROM finding_reviews WHERE finding_id = NEW.id AND to_version = NEW.review_version) THEN
					RAISE EXCEPTION 'injected_mid_transaction_failure';
				ELSE
					RAISE EXCEPTION 'unexpected_trigger_state_no_review_found';
				END IF;
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;

		DROP TRIGGER IF EXISTS trg_test_fail_finding_review ON findings;
		CREATE TRIGGER trg_test_fail_finding_review
		BEFORE UPDATE ON findings
		FOR EACH ROW EXECUTE FUNCTION test_fail_finding_review_trigger();
	`, finding.ID.String())

	if _, err := env.pool.Exec(ctx, triggerSQL); err != nil {
		t.Fatalf("create test failure trigger: %v", err)
	}

	// Register t.Cleanup to unconditionally drop test trigger
	t.Cleanup(func() {
		_, _ = env.pool.Exec(context.Background(), `
			DROP TRIGGER IF EXISTS trg_test_fail_finding_review ON findings;
			DROP FUNCTION IF EXISTS test_fail_finding_review_trigger();
		`)
	})

	v0 := 0
	reqID := uuid.New()
	_, _, _, err := env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Will fail at findings UPDATE after review INSERT",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       reqID,
	})
	if err == nil {
		t.Fatalf("expected injected failure, got nil")
	}
	if !strings.Contains(err.Error(), "injected_mid_transaction_failure") {
		t.Fatalf("expected error to contain 'injected_mid_transaction_failure', got: %v", err)
	}

	// Assertions from outside the transaction:
	// 1. No review records exist in database
	reviews, err := env.runService.ListFindingReviews(ctx, run.ID, finding.ID)
	if err != nil {
		t.Fatalf("list reviews: %v", err)
	}
	if len(reviews) != 0 {
		t.Fatalf("transaction rollback failed! Expected 0 review records, found %d", len(reviews))
	}

	// 2. Finding review fields are completely unchanged
	findings, err := env.runService.ListFindingsByRun(ctx, run.ID)
	if err != nil || len(findings) == 0 {
		t.Fatalf("list findings: %v", err)
	}
	f := findings[0]
	if f.Status != model.FindingStatusPendingReview || f.ReviewVersion != 0 || f.LastReviewer != nil || f.LastReviewedAt != nil || f.LastReviewComment != nil {
		t.Fatalf("finding was mutated despite rollback! Finding: %+v", f)
	}

	// 3. Original check results and HTTP evidence unchanged
	checkResults, _ := env.runService.ListCheckResultsByRun(ctx, run.ID)
	if len(checkResults) == 0 || checkResults[0].ID != origCR.ID || checkResults[0].Status != origCR.Status {
		t.Fatalf("check result mutated during rollback")
	}
	toolCalls, _ := env.runService.ListAgentToolCalls(ctx, run.ID)
	if len(toolCalls) == 0 || toolCalls[0].ID != origTC.ID {
		t.Fatalf("tool call mutated during rollback")
	}

	// 4. Remove injected trigger and verify the same business request now succeeds
	if _, err := env.pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS trg_test_fail_finding_review ON findings;
		DROP FUNCTION IF EXISTS test_fail_finding_review_trigger();
	`); err != nil {
		t.Fatalf("drop test trigger: %v", err)
	}

	_, revSuccess, replayed, err := env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Retry after trigger removed",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       reqID,
	})
	if err != nil {
		t.Fatalf("retry after cleaning trigger failed: %v", err)
	}
	if replayed {
		t.Fatalf("expected fresh execution, not replayed")
	}
	if revSuccess.ToVersion != 1 || revSuccess.ToStatus != model.FindingStatusConfirmed {
		t.Fatalf("unexpected review record: %+v", revSuccess)
	}
}

// 7. Run 在 completed、failed、stopped 等已有终态时均可审核；终态、finish_reason、会话、工作项和预算不改变。运行中的 Finding 也可审核。
func TestPhase9_TerminalStateRunReview(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	terminalStatuses := []struct {
		status       model.RunStatus
		finishReason string
	}{
		{model.RunStatusCompleted, "completed"},
		{model.RunStatusFailed, "failed_by_error"},
		{model.RunStatusStopped, "user_cancelled"},
	}

	for _, ts := range terminalStatuses {
		t.Run(string(ts.status), func(t *testing.T) {
			run, finding, _, _ := createRunAndFinding(t, env, srv, "Terminal Review "+string(ts.status))

			// Transition run to terminal status
			if ts.status == model.RunStatusStopped {
				_, err := env.runService.TransitionState(context.Background(), run.ID, model.RunStatusRunning, model.RunStatusStopping, nil)
				if err != nil {
					t.Fatalf("transition to stopping: %v", err)
				}
				_, err = env.runService.TransitionState(context.Background(), run.ID, model.RunStatusStopping, model.RunStatusStopped, &ts.finishReason)
				if err != nil {
					t.Fatalf("transition to stopped: %v", err)
				}
			} else {
				_, err := env.runService.TransitionState(context.Background(), run.ID, model.RunStatusRunning, ts.status, &ts.finishReason)
				if err != nil {
					t.Fatalf("transition to terminal state %s: %v", ts.status, err)
				}
			}

			// Capture work items and sessions count
			workItemsBefore, _ := env.runService.ListWorkItemsByRun(context.Background(), run.ID)
			sessionsBefore, _ := env.runService.ListSessionsByRun(context.Background(), run.ID)

			// Review finding in terminal run
			v0 := 0
			reqID := uuid.New()
			body, _ := json.Marshal(map[string]interface{}{
				"status":           "confirmed",
				"comment":          "Reviewed after run finished",
				"reviewer":         "Auditor",
				"expected_version": v0,
				"request_id":       reqID,
			})
			req := httptest.NewRequest("POST", fmt.Sprintf("/api/runs/%s/findings/%s/reviews", run.ID, finding.ID), bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			env.router.ServeHTTP(w, req)

			if w.Code != http.StatusCreated {
				t.Fatalf("expected 201 Created for terminal run review, got %d: %s", w.Code, w.Body.String())
			}

			// Verify run remains in terminal state and finish_reason is intact
			runAfter, err := env.runService.Get(context.Background(), run.ID)
			if err != nil {
				t.Fatalf("get run: %v", err)
			}
			if runAfter.Status != ts.status {
				t.Fatalf("run status mutated! Expected %s, got %s", ts.status, runAfter.Status)
			}
			if runAfter.FinishReason == nil || *runAfter.FinishReason != ts.finishReason {
				t.Fatalf("finish_reason mutated! Expected %s, got %v", ts.finishReason, runAfter.FinishReason)
			}

			// Verify work items and sessions are untouched
			workItemsAfter, _ := env.runService.ListWorkItemsByRun(context.Background(), run.ID)
			sessionsAfter, _ := env.runService.ListSessionsByRun(context.Background(), run.ID)
			if len(workItemsAfter) != len(workItemsBefore) {
				t.Fatalf("work items count changed!")
			}
			if len(sessionsAfter) != len(sessionsBefore) {
				t.Fatalf("sessions count changed!")
			}
		})
	}
}

// 8. 重新执行相同检查复用已有结果时，保留已审核 Finding，历史和版本不变。
func TestPhase9_DuplicateCheckPreservesReviewedFinding(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	ctx := context.Background()
	run, finding, _, origTC := createRunAndFinding(t, env, srv, "Dedup Preserve Finding Task")

	// Review finding to confirmed
	v0 := 0
	_, _, _, err := env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Confirmed by security analyst",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})
	if err != nil {
		t.Fatalf("review finding: %v", err)
	}

	// Verify it is confirmed v1
	reviewedF, _ := env.runService.ListFindingsByRun(ctx, run.ID)
	if reviewedF[0].Status != model.FindingStatusConfirmed || reviewedF[0].ReviewVersion != 1 {
		t.Fatalf("expected confirmed v1, got %s v%d", reviewedF[0].Status, reviewedF[0].ReviewVersion)
	}

	// Now re-run check via RecordCheckResults using a new work item / session in the same run
	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	_, err = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "check_home_second_time",
			Title:     "Second Check",
			Objective: "Re-run check",
			WorkType:  "run_security_check",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String(), "check_id": "http_x_content_type_options"},
		},
	})
	if err != nil {
		t.Fatalf("submit second plan: %v", err)
	}

	wi2, att2, sess2, err := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	if err != nil {
		t.Fatalf("claim second item: %v", err)
	}

	out, err := env.runService.RecordCheckResults(ctx, run.ID, &att2.ID, &sess2.ID, &wi2.ID, []service.CheckResultInput{
		{
			AssetID:          homeAsset.ID,
			CheckID:          "http_x_content_type_options",
			CheckName:        "X-Content-Type-Options Header Check",
			CheckVersion:     "1.0.0",
			Status:           model.CheckStatusNeedsAttention,
			Explanation:      "Missing header",
			SourceToolCallID: &origTC.ID,
			GenerateFinding:  true,
		},
	})
	if err != nil {
		t.Fatalf("record second check: %v", err)
	}

	if out.DeduplicatedCount != 1 {
		t.Fatalf("expected deduplicated count 1, got %d", out.DeduplicatedCount)
	}

	// Assert the returned finding in deduplication retained its confirmed status and version 1!
	if len(out.Findings) != 1 {
		t.Fatalf("expected 1 finding returned from dedup, got %d", len(out.Findings))
	}
	if out.Findings[0].Status != model.FindingStatusConfirmed || out.Findings[0].ReviewVersion != 1 {
		t.Fatalf("deduplication reset reviewed finding status! Got status %s, ver %d", out.Findings[0].Status, out.Findings[0].ReviewVersion)
	}

	// Verify DB state
	finalFindings, _ := env.runService.ListFindingsByRun(ctx, run.ID)
	if finalFindings[0].Status != model.FindingStatusConfirmed || finalFindings[0].ReviewVersion != 1 {
		t.Fatalf("DB finding status was overwritten! Got %s v%d", finalFindings[0].Status, finalFindings[0].ReviewVersion)
	}
}

// 9. 当前审核报告四类计数与列表一致；审核后及时刷新，原执行总结保留快照语义。
func TestPhase9_FindingReportConsistencyAndSnapshotSummary(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()
	_ = srv

	ctx := context.Background()

	task, _ := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Report Consistency Task",
		Objective: "Test report counts and execution summary",
		Scope:     json.RawMessage(`{"endpoints": ["http_home"]}`),
	})
	run, _, _ := env.runService.Create(ctx, task.ID, model.CreateRunRequest{}, nil)
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	runAssets, _ := env.runService.ListAssetsByRun(ctx, run.ID)
	homeAsset := runAssets[0]

	// Setup inspect and 4 checks that produce 4 candidate findings
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "inspect",
			Title:     "Inspect",
			Objective: "Fetch",
			WorkType:  "inspect_http_endpoint",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String()},
		},
	})
	wiI, attI, sessI, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	httpTool := tools.NewGetEndpointTool(run.ID, wiI.ID, env.runService)
	getArgs, _ := json.Marshal(map[string]string{"asset_id": homeAsset.ID.String()})
	tcI, _ := env.runService.CreateAgentToolCallWithSession(ctx, run.ID, &sessI.ID, nil, "c_get", "get_endpoint", getArgs)
	getOut, _ := httpTool.Execute(ctx, getArgs)
	now := time.Now()
	_ = env.runService.UpdateAgentToolCall(ctx, tcI.ID, "success", getOut, nil, &now, &now)
	_ = env.runService.FinishWorkItem(ctx, wiI.ID, attI.ID, sessI.ID, true, json.RawMessage(`{}`), "completed")

	// Submit check plan for http_security_headers (which evaluates nosniff, csp, hsts)
	_, _ = env.runService.SubmitPlan(ctx, run.ID, []service.PlanItemSpec{
		{
			Key:       "check_suite",
			Title:     "Check Suite",
			Objective: "Check suite",
			WorkType:  "run_security_check",
			AssetIDs:  []uuid.UUID{homeAsset.ID},
			InputData: map[string]interface{}{"asset_id": homeAsset.ID.String(), "check_id": "http_security_headers"},
		},
	})
	wiC, attC, sessC, _ := env.runService.ClaimNextPendingWorkItem(ctx, run.ID)
	checkTool := tools.NewRunSecurityCheckTool(run.ID, wiC.ID, attC.ID, sessC.ID, env.runService)
	checkArgs, _ := json.Marshal(map[string]string{
		"asset_id": homeAsset.ID.String(),
		"check_id": "http_security_headers",
	})
	checkOut, err := checkTool.Execute(ctx, checkArgs)
	if err != nil {
		t.Fatalf("checkTool error: %v", err)
	}
	_ = env.runService.FinishWorkItem(ctx, wiC.ID, attC.ID, sessC.ID, true, json.RawMessage(checkOut), "completed")

	findings, _ := env.runService.ListFindingsByRun(ctx, run.ID)
	if len(findings) < 2 {
		t.Fatalf("expected at least 2 findings for report test, got %d", len(findings))
	}

	// Review finding 0 to confirmed
	v0 := 0
	_, _, _, _ = env.runService.ReviewFinding(ctx, run.ID, findings[0].ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Confirmed",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})

	// Review finding 1 to false_positive
	_, _, _, _ = env.runService.ReviewFinding(ctx, run.ID, findings[1].ID, service.ReviewFindingInput{
		Status:          model.FindingStatusFalsePositive,
		Comment:         "False positive",
		Reviewer:        "Bob",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})

	// Query finding-report endpoint
	reqReport := httptest.NewRequest("GET", fmt.Sprintf("/api/runs/%s/finding-report", run.ID), nil)
	wReport := httptest.NewRecorder()
	env.router.ServeHTTP(wReport, reqReport)
	if wReport.Code != http.StatusOK {
		t.Fatalf("get finding report failed: %s", wReport.Body.String())
	}

	var report model.FindingReport
	if err := json.Unmarshal(wReport.Body.Bytes(), &report); err != nil {
		t.Fatalf("unmarshal report: %v", err)
	}

	if report.Summary.Total != len(findings) {
		t.Fatalf("expected total %d, got %d", len(findings), report.Summary.Total)
	}
	if report.Summary.Confirmed != 1 {
		t.Fatalf("expected confirmed 1, got %d", report.Summary.Confirmed)
	}
	if report.Summary.FalsePositive != 1 {
		t.Fatalf("expected false_positive 1, got %d", report.Summary.FalsePositive)
	}
	if report.Summary.PendingReview != len(findings)-2 {
		t.Fatalf("expected pending_review %d, got %d", len(findings)-2, report.Summary.PendingReview)
	}
	if len(report.Findings) != len(findings) {
		t.Fatalf("expected %d items in report findings list, got %d", len(findings), len(report.Findings))
	}

	// Test GenerateProgrammaticSummary closeout snapshot semantics
	summary := orchestration.GenerateProgrammaticSummary(ctx, run.ID, env.runService, model.CauseCompleted)
	if !strings.Contains(summary, "Candidate Findings Snapshot at Closeout") {
		t.Fatalf("expected summary to contain 'Candidate Findings Snapshot at Closeout', got:\n%s", summary)
	}
	if strings.Contains(summary, "Candidate Findings (Pending Review)") {
		t.Fatalf("summary should not hardcode all findings as '(Pending Review)'")
	}
}

// 10. 审计保留与外键保护测试（第1组核心验收要求）：
// - 已结束任务产生审核后，调用删除 API 返回 409；任务、Run、Finding、历史均保留
// - 直接删除其 Finding 或 Run，被 RESTRICT 外键拒绝
// - 没有审核历史且没有活动 Run 的任务仍可正常删除
// - 并发审核与删除使用屏障控制，已提交的审核历史必须保留
// - 迁移 015 降级拒绝回滚级联语义
func TestPhase9_AuditRetention_CannotDeleteTaskWithReviews(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	ctx := context.Background()
	run, finding, _, _ := createRunAndFinding(t, env, srv, "Audit Protected Task")

	// Review finding to confirmed (producing an audit record in finding_reviews)
	v0 := 0
	_, _, _, err := env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Audit record that must be retained forever",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})
	if err != nil {
		t.Fatalf("review finding: %v", err)
	}

	// Finish the run so it is not active
	finishReason := "completed"
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusCompleted, &finishReason)

	// Call Task Delete API
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/tasks/%s", run.TaskID), nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	// Must return 409 Conflict with clear message
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409 Conflict when deleting task with reviews, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "该任务包含人工审核历史，暂不支持删除") {
		t.Fatalf("expected response message about review history, got: %s", w.Body.String())
	}

	// Verify Task, Run, Finding, and Reviews all still exist in DB
	taskAfter, err := env.taskService.Get(ctx, run.TaskID)
	if err != nil || taskAfter == nil {
		t.Fatalf("task was deleted! Expected it to be retained")
	}
	runAfter, err := env.runService.Get(ctx, run.ID)
	if err != nil || runAfter == nil {
		t.Fatalf("run was deleted! Expected it to be retained")
	}
	findingsAfter, _ := env.runService.ListFindingsByRun(ctx, run.ID)
	if len(findingsAfter) == 0 {
		t.Fatalf("findings were deleted! Expected retention")
	}
	reviewsAfter, _ := env.runService.ListFindingReviews(ctx, run.ID, finding.ID)
	if len(reviewsAfter) == 0 {
		t.Fatalf("finding_reviews were deleted! Expected retention")
	}
}

func TestPhase9_AuditRetention_DirectDeleteRunOrFindingRejectedByFK(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	ctx := context.Background()
	run, finding, _, _ := createRunAndFinding(t, env, srv, "FK Restrict Direct Delete Task")

	v0 := 0
	_, _, _, err := env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Review to lock finding and run with FK RESTRICT",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})
	if err != nil {
		t.Fatalf("review finding: %v", err)
	}

	// 1. Directly attempt DELETE FROM findings
	_, err = env.pool.Exec(ctx, `DELETE FROM findings WHERE id = $1`, finding.ID)
	if err == nil {
		t.Fatalf("expected DB foreign key RESTRICT violation on deleting finding, but delete succeeded!")
	}
	var pgErr *pgconn.PgError
	if !strings.Contains(err.Error(), "fk_finding_reviews_finding") && (!strings.Contains(err.Error(), "23503")) {
		t.Fatalf("expected foreign key violation on fk_finding_reviews_finding, got: %v", err)
	}

	// 2. Directly attempt DELETE FROM runs
	_, err = env.pool.Exec(ctx, `DELETE FROM runs WHERE id = $1`, run.ID)
	if err == nil {
		t.Fatalf("expected DB foreign key RESTRICT violation on deleting run, but delete succeeded!")
	}
	if !strings.Contains(err.Error(), "fk_finding_reviews_run") && (!strings.Contains(err.Error(), "23503")) {
		t.Fatalf("expected foreign key violation on fk_finding_reviews_run, got: %v", err)
	}
	_ = pgErr
}

func TestPhase9_AuditRetention_CleanTaskCanBeDeleted(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()
	_ = srv

	ctx := context.Background()
	task, err := env.taskService.Create(ctx, model.CreateTaskRequest{
		Name:      "Clean Task to Delete",
		Objective: "No reviews",
	})
	if err != nil {
		t.Fatalf("create task: %v", err)
	}

	// Task without runs can be deleted
	req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/tasks/%s", task.ID), nil)
	w := httptest.NewRecorder()
	env.router.ServeHTTP(w, req)

	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204 No Content, got %d: %s", w.Code, w.Body.String())
	}

	// Verify task is gone
	_, err = env.taskService.Get(ctx, task.ID)
	if err == nil {
		t.Fatalf("expected task to be deleted, but still found")
	}
}

func TestPhase9_AuditRetention_ConcurrentReviewAndDelete(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()

	ctx := context.Background()
	run, finding, _, _ := createRunAndFinding(t, env, srv, "Concurrent Review and Delete Task")

	// Set run to completed so active runs check doesn't block before delete tries DB delete
	reason := "completed"
	_, _ = env.runService.TransitionState(ctx, run.ID, model.RunStatusRunning, model.RunStatusCompleted, &reason)

	barrier := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)

	var reviewErr error
	var deleteCode int

	// Goroutine 1: Submit review
	go func() {
		defer wg.Done()
		<-barrier
		v0 := 0
		_, _, _, reviewErr = env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
			Status:          model.FindingStatusConfirmed,
			Comment:         "Concurrent review",
			Reviewer:        "Alice",
			ExpectedVersion: &v0,
			RequestID:       uuid.New(),
		})
	}()

	// Goroutine 2: Attempt Delete Task
	go func() {
		defer wg.Done()
		<-barrier
		req := httptest.NewRequest("DELETE", fmt.Sprintf("/api/tasks/%s", run.TaskID), nil)
		w := httptest.NewRecorder()
		env.router.ServeHTTP(w, req)
		deleteCode = w.Code
	}()

	close(barrier)
	wg.Wait()

	// Assertions:
	// If review succeeded (reviewErr == nil), delete MUST have failed (409 Conflict).
	// Under NO circumstances should review succeed AND delete succeed!
	if reviewErr == nil {
		if deleteCode != http.StatusConflict {
			t.Fatalf("review succeeded but delete did not return 409 conflict! Delete code: %d", deleteCode)
		}
		// Confirm review history is intact
		reviews, _ := env.runService.ListFindingReviews(ctx, run.ID, finding.ID)
		if len(reviews) == 0 {
			t.Fatalf("audit review was deleted during race!")
		}
	}
}

// 11. 迁移在独立数据库验证，014 降级保护与 015 降级拒绝级联保护
func TestPhase9_Migration_014And015_DowngradeProtection(t *testing.T) {
	env, srv, cleanup := setupPhase9Env(t)
	defer cleanup()
	_ = srv

	ctx := context.Background()

	// 11a. Test 015 down.sql refuses rollback
	downSQL015, err := os.ReadFile("../migrations/015_audit_retention_restrict_fk.down.sql")
	if err != nil {
		t.Fatalf("read 015 down.sql: %v", err)
	}
	_, err = env.pool.Exec(ctx, string(downSQL015))
	if err == nil {
		t.Fatalf("expected 015 down.sql to fail, but succeeded")
	}
	if !strings.Contains(err.Error(), "audit retention policy forbids reverting to CASCADE") {
		t.Fatalf("expected audit retention policy message, got: %v", err)
	}

	// 11b. Test 014 down.sql refuses rollback when review exists
	run, finding, _, _ := createRunAndFinding(t, env, srv, "Migration 014 Test Task")
	v0 := 0
	_, _, _, err = env.runService.ReviewFinding(ctx, run.ID, finding.ID, service.ReviewFindingInput{
		Status:          model.FindingStatusConfirmed,
		Comment:         "Audit record for 014 down protection",
		Reviewer:        "Alice",
		ExpectedVersion: &v0,
		RequestID:       uuid.New(),
	})
	if err != nil {
		t.Fatalf("review: %v", err)
	}

	downSQL014, err := os.ReadFile("../migrations/014_finding_reviews.down.sql")
	if err != nil {
		t.Fatalf("read 014 down.sql: %v", err)
	}
	_, err = env.pool.Exec(ctx, string(downSQL014))
	if err == nil {
		t.Fatalf("expected 014 down.sql to fail with audit records, but succeeded")
	}
}

func ptr[T any](v T) *T {
	return &v
}
