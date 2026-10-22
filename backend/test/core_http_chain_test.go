package test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/netcap"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
)

func TestCoreHTTPReadArtifactObservationAndFact(t *testing.T) {
	pool := ma3EvidencePool(t)
	ctx := context.Background()
	body := strings.Repeat("captured-body-", 100)
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(body)) }))
	defer app.Close()
	u, _ := url.Parse(app.URL)
	policy := fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":%q,"path_mode":"same_origin","allow_private":true}]}}`, u.Port())
	runID, assetID, workID := seedHTTPAsset(t, pool, policy, app.URL+"/", "GET "+app.URL+"/")
	var sessionID, attemptID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO agent_sessions(run_id,role,status,work_item_id) VALUES($1,'worker','running',$2) RETURNING id`, runID, workID).Scan(&sessionID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO attempts(work_item_id,attempt_number,status,session_id) VALUES($1,1,'running',$2) RETURNING id`, workID, sessionID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	svc := service.NewRunService(pool)
	args := []byte(fmt.Sprintf(`{"url":%q,"method":"GET"}`, app.URL+"/"))
	callID := insertToolCall(t, pool, runID, &sessionID, "http_request", args, "", "pending", 1)
	tool := tools.NewHTTPRequestTool(runID, workID, attemptID, sessionID, svc, netcap.NewTransport(netcap.NewStore(pool)))
	output, err := tool.Execute(ctx, args)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = pool.Exec(ctx, `UPDATE agent_tool_calls SET status='success',output=$2 WHERE id=$1`, callID, output); err != nil {
		t.Fatal(err)
	}
	var result struct {
		Receipt contract.HTTPReceipt `json:"receipt"`
	}
	if err = json.Unmarshal([]byte(output), &result); err != nil {
		t.Fatal(err)
	}
	if result.Receipt.AssetID == nil || *result.Receipt.AssetID != assetID {
		t.Fatal("URL-only request lost its assigned asset")
	}
	if result.Receipt.ResponseArtifactID == nil {
		t.Fatal("missing response artifact")
	}
	page, err := svc.ReadArtifact(ctx, runID, *result.Receipt.ResponseArtifactID, 0, 8192)
	if err != nil || page["content"] != body {
		t.Fatalf("capture artifact unreadable: %v %v", page, err)
	}
	if _, err = svc.ReadArtifact(ctx, uuid.New(), *result.Receipt.ResponseArtifactID, 0, 8192); !errors.Is(err, service.ErrCrossRunBoundary) {
		t.Fatalf("cross-run body read: %v", err)
	}
	obs, err := svc.CreateObservationsBatchWithAssets(ctx, runID, attemptID, []service.ObservationInput{{ObsType: "page_observation", Content: "captured response", SourceToolCallID: callID, AssetID: &assetID}})
	if err != nil || len(obs) != 1 {
		t.Fatalf("HTTP observation: %v %v", obs, err)
	}
	fact, err := svc.RecordFact(ctx, runID, "Captured response", body, "observed", callID.String(), &workID, []uuid.UUID{assetID})
	if err != nil || fact.State != "verified" {
		t.Fatalf("trusted HTTP fact: %+v %v", fact, err)
	}
	fake := insertToolCall(t, pool, runID, &sessionID, "http_request", args, output, "success", 1)
	forged, err := svc.RecordFact(ctx, runID, "Forged response", body, "observed", fake.String(), &workID, []uuid.UUID{assetID})
	if err != nil || forged.State == "verified" {
		t.Fatalf("copied receipt verified a different call: %+v %v", forged, err)
	}
}
