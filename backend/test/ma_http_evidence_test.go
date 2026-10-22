package test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/db"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestMAHTTPEvidenceAndRetest(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	var marker atomic.Value
	marker.Store("vuln-marker")
	ln := listenLocal(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(marker.Load().(string)))
	})
	go (&http.Server{Handler: mux}).Serve(ln)
	base := "http://" + ln.Addr().String()
	target := base + "/health"
	policy := fmt.Sprintf(`{"discovery_policy":{"origin":"%s","mode":"same_origin_html","allowed_methods":["GET"],"request_interval_ms":50}}`, base)
	runID, assetID, workItemID := seedHTTPAsset(t, pool, policy, target, "GET "+target)
	svc := service.NewRunService(pool)
	tool := tools.NewGetEndpointTool(runID, workItemID, svc)
	args := []byte(fmt.Sprintf(`{"asset_id":"%s"}`, assetID))
	output, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output, "vuln-marker") || !strings.Contains(output, `"status_code":200`) {
		t.Fatalf("real get_endpoint output: %s", output)
	}
	callID := insertToolCall(t, pool, runID, nil, "get_endpoint", args, output, "success", 1)
	finding, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "Marker present", Category: "test", Severity: "low",
		Description: "Observed vuln-marker on the authorized endpoint", SourceKind: "empirical",
		SourceToolCallID: &callID,
		Verification: &service.FindingVerification{
			Vulnerable: service.HTTPCondition{StatusCode: 200, BodyContains: "vuln-marker"},
			Fixed:      service.HTTPCondition{StatusCode: 200, BodyContains: "fixed-marker"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	staleRun, staleAsset, _ := seedHTTPAsset(t, pool, policy, target, "GET "+target+"-stale")
	staleCall := insertToolCall(t, pool, staleRun, nil, "get_endpoint", []byte(fmt.Sprintf(`{"asset_id":"%s"}`, staleAsset)), output, "success", 1)
	if _, err := pool.Exec(ctx, `UPDATE runs SET current_epoch=2 WHERE id=$1`, staleRun); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ReportFinding(ctx, staleRun, service.ReportFindingInput{
		AssetID: staleAsset, Title: "Stale", Category: "test", Severity: "low", Description: "old epoch",
		SourceKind: "empirical", SourceToolCallID: &staleCall,
		Verification: &service.FindingVerification{
			Vulnerable: service.HTTPCondition{StatusCode: 200, BodyContains: "vuln-marker"},
			Fixed:      service.HTTPCondition{StatusCode: 200, BodyContains: "fixed-marker"},
		},
	}); !errors.Is(err, service.ErrStaleEpoch) && !errors.Is(err, service.ErrInvalidEvidence) {
		t.Fatalf("old epoch became empirical: %v", err)
	}

	retest, sess, err := svc.CreateFindingRetest(ctx, runID, finding.ID, "replay the stored GET")
	if err != nil {
		t.Fatal(err)
	}
	again, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	retestCall := insertToolCall(t, pool, runID, &sess.ID, "get_endpoint", args, again, "success", 1)
	evidence := fmt.Sprintf(`{"source_tool_call_id":"%s"}`, retestCall)
	if _, err := svc.FinishFindingRetest(ctx, runID, finding.ID, retest.ID, "fixed", evidence, "model says fixed"); err == nil {
		t.Fatal("requested fixed verdict was accepted for a still vulnerable GET")
	}
	done, err := svc.FinishFindingRetestFromEvidence(ctx, runID, finding.ID, retest.ID, evidence, "derive from the fresh GET")
	if err != nil {
		t.Fatal(err)
	}
	if done.Verdict == nil || *done.Verdict != "still_reproducible" {
		t.Fatalf("verdict %v", done.Verdict)
	}

	marker.Store("fixed-marker")
	retest2, sess2, err := svc.CreateFindingRetest(ctx, runID, finding.ID, "check the fix")
	if err != nil {
		t.Fatal(err)
	}
	fixedOut, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fixedOut, "fixed-marker") {
		t.Fatalf("fixture did not change: %s", fixedOut)
	}
	fixedCall := insertToolCall(t, pool, runID, &sess2.ID, "get_endpoint", args, fixedOut, "success", 1)
	done, err = svc.FinishFindingRetestFromEvidence(ctx, runID, finding.ID, retest2.ID, fmt.Sprintf(`{"source_tool_call_id":"%s"}`, fixedCall), "derive after fix")
	if err != nil {
		t.Fatal(err)
	}
	if done.Verdict == nil || *done.Verdict != "fixed" {
		t.Fatalf("fixed verdict %v", done.Verdict)
	}
}

func TestMAHTTPForgedReceiptAndUnsafeReplay(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	ln := listenLocal(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/profile", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.Contains(r.Header.Get("Cookie"), "alice-token") {
			_, _ = w.Write([]byte("side-effect-marker"))
			return
		}
		_, _ = w.Write([]byte("fixed-marker"))
	})
	go (&http.Server{Handler: mux}).Serve(ln)
	base := "http://" + ln.Addr().String()
	target := base + "/profile"
	port := ln.Addr().(*net.TCPAddr).Port
	policy := fmt.Sprintf(`{"discovery_policy":{"origin":"%s","mode":"same_origin_html","request_interval_ms":50},"network_policy":{"template":"active","allowed_methods":["GET","POST"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"%d","path_mode":"same_origin","allow_private":true}],"max_pages":10,"max_requests":20,"request_interval_ms":0}}`, base, port)
	runID, assetID, workItemID := seedHTTPAsset(t, pool, policy, target, "POST "+target)
	var attemptID, sessionID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO attempts(work_item_id, attempt_number, status) VALUES($1,1,'running') RETURNING id`, workItemID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id, role, status, work_item_id, attempt_id, epoch_number) VALUES($1,'worker','running',$2,$3,1) RETURNING id`, runID, workItemID, attemptID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if err := netcap.NewStore(pool).PutCredential(ctx, netcap.Credential{Alias: "alice", TargetOrigin: base, Kind: "cookie", Secret: "sid=alice-token"}); err != nil {
		t.Fatal(err)
	}
	svc := service.NewRunService(pool)
	args := []byte(fmt.Sprintf(`{"asset_id":"%s","url":"%s","method":"POST","credential_ref":"ref:alice"}`, assetID, target))
	callID := insertToolCall(t, pool, runID, &sessionID, "http_request", args, "", "pending", 1)
	httpTool := tools.NewHTTPRequestTool(runID, workItemID, attemptID, sessionID, svc, netcap.NewTransport(netcap.NewStore(pool)))
	output, err := httpTool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output, "alice-token") || strings.Contains(output, "sid=") {
		t.Fatalf("tool output exposed a credential: %s", output)
	}
	if !strings.Contains(output, "side-effect-marker") {
		t.Fatalf("authenticated response missing marker: %s", output)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_tool_calls SET status='success', output=$2, finished_at=now() WHERE id=$1`, callID, output); err != nil {
		t.Fatal(err)
	}
	var bound uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT id FROM http_receipts WHERE source_tool_call_id=$1 AND minted_by='gateway'`, callID).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	var artifactID uuid.UUID
	if err := pool.QueryRow(ctx, `SELECT response_artifact_id FROM http_receipts WHERE id=$1`, bound).Scan(&artifactID); err != nil {
		t.Fatal(err)
	}
	scope := contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RoleWorker, WorkItemID: &workItemID, AttemptID: &attemptID}
	view, err := netcap.NewStore(pool).ReadArtifact(ctx, scope, artifactID, true)
	if err != nil || !strings.Contains(string(view.Body), "side-effect-marker") {
		t.Fatalf("raw artifact %q err %v", view.Body, err)
	}

	finding, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "Authenticated marker", Category: "test", Severity: "low",
		Description: "POST observed side-effect-marker", SourceKind: "empirical", SourceToolCallID: &callID,
		Verification: &service.FindingVerification{
			Vulnerable: service.HTTPCondition{StatusCode: 200, BodyContains: "side-effect-marker"},
			Fixed:      service.HTTPCondition{StatusCode: 200, BodyContains: "fixed-marker"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	retest, _, err := svc.CreateFindingRetest(ctx, runID, finding.ID, "do not replay the POST")
	if err != nil {
		t.Fatal(err)
	}
	getTool := tools.NewGetEndpointTool(runID, workItemID, svc)
	getArgs := []byte(fmt.Sprintf(`{"asset_id":"%s"}`, assetID))
	loggedOut, err := getTool.Execute(ctx, getArgs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(loggedOut, "fixed-marker") {
		t.Fatalf("logged-out page: %s", loggedOut)
	}
	if _, err := svc.FinishFindingRetest(ctx, runID, finding.ID, retest.ID, "fixed", `{}`, "model inferred fixed from the login page"); err == nil {
		t.Fatal("requested fixed verdict was accepted for an authenticated source")
	}
	done, err := svc.FinishFindingRetestFromEvidence(ctx, runID, finding.ID, retest.ID, `{}`, "derive")
	if err != nil {
		t.Fatal(err)
	}
	if done.Verdict == nil || *done.Verdict != "inconclusive" || !strings.Contains(done.Summary, "cannot be safely auto-replayed") {
		t.Fatalf("unsafe replay verdict %v summary %q", done.Verdict, done.Summary)
	}

	forged := insertToolCall(t, pool, runID, &sessionID, "http_request", args, fmt.Sprintf(`{"receipt_id":"%s","status_code":200}`, uuid.New()), "success", 1)
	if _, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "Forged", Category: "test", Severity: "low", Description: "forged",
		SourceKind: "empirical", SourceToolCallID: &forged,
	}); err == nil {
		t.Fatal("forged receipt became empirical")
	}
	stolen := insertToolCall(t, pool, runID, &sessionID, "http_request", args, fmt.Sprintf(`{"receipt_id":"%s"}`, bound), "success", 1)
	if _, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "Stolen receipt", Category: "test", Severity: "low", Description: "other call",
		SourceKind: "empirical", SourceToolCallID: &stolen,
	}); err == nil {
		t.Fatal("receipt from another tool call became empirical")
	}
	knowledge := insertToolCall(t, pool, runID, &sessionID, "query_knowledge", []byte(`{}`), output, "success", 1)
	if _, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "Knowledge", Category: "test", Severity: "low", Description: "knowledge",
		SourceKind: "empirical", SourceToolCallID: &knowledge,
	}); err == nil {
		t.Fatal("knowledge query became empirical")
	}
	command := insertToolCall(t, pool, runID, &sessionID, "run_command", []byte(`{}`), output, "success", 1)
	if _, err := svc.ReportFinding(ctx, runID, service.ReportFindingInput{
		AssetID: assetID, Title: "Stdout", Category: "test", Severity: "low", Description: "stdout",
		SourceKind: "empirical", SourceToolCallID: &command,
	}); err == nil {
		t.Fatal("command stdout became empirical")
	}
}

func ma3EvidencePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil || (u.Path != "/agent_workbench_ma3" && u.Path != "/agent_workbench_ma_integration") {
		t.Fatalf("refusing TEST_DATABASE_URL %q; tests truncate and require /agent_workbench_ma3 or /agent_workbench_ma_integration", raw)
	}
	if err := db.RunMigrations(raw, migrationsPath); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE http_credentials, tasks, assets CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func listenLocal(t *testing.T) net.Listener {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

func seedHTTPAsset(t *testing.T, pool *pgxpool.Pool, policy, target, externalKey string) (uuid.UUID, uuid.UUID, uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	var taskID, runID, assetID, workItemID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tasks(name, objective) VALUES('ma3-evidence','http') RETURNING id`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO runs(task_id, status, execution_input) VALUES($1,'running',$2::jsonb) RETURNING id`, taskID, policy).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if len(externalKey) > 100 {
		t.Fatalf("external key too long: %s", externalKey)
	}
	snap := fmt.Sprintf(`{"metadata":{"url":"%s"}}`, target)
	if err := pool.QueryRow(ctx, `INSERT INTO assets(namespace, kind, external_key, name, metadata) VALUES('test','http_endpoint',$1,$1,$2::jsonb) RETURNING id`, externalKey, snap).Scan(&assetID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO run_assets(run_id, asset_id, snapshot) VALUES($1,$2,$3::jsonb)`, runID, assetID, snap); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO work_items(run_id, key, title) VALUES($1,'inspect','inspect') RETURNING id`, runID).Scan(&workItemID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO work_item_assets(work_item_id, asset_id) VALUES($1,$2)`, workItemID, assetID); err != nil {
		t.Fatal(err)
	}
	return runID, assetID, workItemID
}

func insertToolCall(t *testing.T, pool *pgxpool.Pool, runID uuid.UUID, sessionID *uuid.UUID, name string, args []byte, output, status string, epoch int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pool.QueryRow(context.Background(), `INSERT INTO agent_tool_calls(run_id, session_id, call_id, tool_name, arguments, output, status, epoch_number)
		VALUES($1,$2,$3,$4,$5::jsonb,$6,$7,$8) RETURNING id`,
		runID, sessionID, uuid.NewString(), name, args, output, status, epoch).Scan(&id)
	if err != nil {
		t.Fatal(err)
	}
	return id
}
