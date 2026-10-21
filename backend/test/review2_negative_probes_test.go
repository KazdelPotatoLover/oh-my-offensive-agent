package test

import (
	"context"
	"encoding/json"
	"testing"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

type review2ScriptedRetestProvider struct{}

func (p *review2ScriptedRetestProvider) Call(ctx context.Context, messages []provider.ChatMessage, tools []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	return &provider.ModelResponse{
		Content:      "The vulnerability appears fixed after reviewing the patch.",
		FinishReason: "stop",
	}, nil
}

func review2ProbeSetup(t *testing.T) (*TestEnv, uuid.UUID, []model.Asset, uuid.UUID) {
	t.Helper()
	e := setupTestEnv(t)
	t.Cleanup(e.teardown)
	c := context.Background()
	task, err := e.taskService.Create(c, model.CreateTaskRequest{Name: "Independent review boundary probe", Scope: json.RawMessage(`{"endpoints":["/","/login"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	r, _, err := e.runService.Create(c, task.ID, model.CreateRunRequest{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.runService.TransitionState(c, r.ID, model.RunStatusQueued, model.RunStatusRunning, nil)
	if err != nil {
		t.Fatal(err)
	}
	assets, err := e.runService.ListAssetsByRun(c, r.ID)
	if err != nil || len(assets) < 2 {
		t.Fatalf("assets: %v (%d)", err, len(assets))
	}
	var tc uuid.UUID
	args, _ := json.Marshal(map[string]string{"asset_id": assets[0].ID.String()})
	err = e.pool.QueryRow(c, `INSERT INTO agent_tool_calls(run_id,call_id,tool_name,arguments,output,status,epoch_number) VALUES($1,'probe_call','get_endpoint',$2,'{"status_code":200}','success',1) RETURNING id`, r.ID, args).Scan(&tc)
	if err != nil {
		t.Fatal(err)
	}
	return e, r.ID, assets, tc
}

func TestReview2Probe_RejectEvidenceFromAnotherAsset(t *testing.T) {
	e, r, assets, tc := review2ProbeSetup(t)
	_, err := e.runService.ReportFinding(context.Background(), r, service.ReportFindingInput{AssetID: assets[1].ID, Title: "Probe", SourceKind: "empirical", SourceToolCallID: &tc})
	if err == nil {
		t.Fatal("accepted empirical finding for asset B using asset A evidence")
	}
}

func TestReview2Probe_RejectOldEpochEvidence(t *testing.T) {
	e, r, assets, tc := review2ProbeSetup(t)
	_, err := e.runService.StartNewEpoch(context.Background(), r, "probe epoch advance", "")
	if err != nil {
		t.Fatal(err)
	}
	_, err = e.runService.ReportFinding(context.Background(), r, service.ReportFindingInput{AssetID: assets[0].ID, Title: "Probe", SourceKind: "empirical", SourceToolCallID: &tc})
	if err == nil {
		t.Fatal("accepted epoch 1 evidence as empirical finding in epoch 2")
	}
}

func TestReview2Probe_RejectTextOnlyVerifiedFact(t *testing.T) {
	e, r, _, _ := review2ProbeSetup(t)
	n, err := e.runService.RecordFact(context.Background(), r, "Unbacked claim", "", "observed", "arbitrary text without a source", nil, nil)
	if err != nil {
		return
	}
	if n.State == "verified" {
		t.Fatal("unbacked caller text became a verified fact")
	}
}

func TestReview2Probe_NoFixedVerdictWithoutTools(t *testing.T) {
	e, r, assets, tc := review2ProbeSetup(t)
	f, err := e.runService.ReportFinding(context.Background(), r, service.ReportFindingInput{AssetID: assets[0].ID, Title: "Probe", SourceKind: "empirical", SourceToolCallID: &tc})
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := e.runService.CreateFindingRetest(context.Background(), r, f.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	ex := orchestration.NewRetestExecutor(e.runService, &review2ScriptedRetestProvider{}, nil)
	done, err := ex.ExecuteFindingRetest(context.Background(), r, f.ID, rt.ID)
	if err != nil {
		return
	}
	var count int
	err = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_tool_calls WHERE session_id=$1`, rt.SessionID).Scan(&count)
	if err != nil {
		t.Fatal(err)
	}
	if count == 0 && done.Verdict != nil && *done.Verdict == "fixed" {
		t.Fatal("no tools ran but retest marked finding fixed from model text")
	}
}
