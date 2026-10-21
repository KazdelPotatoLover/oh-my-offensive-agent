package test

import (
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"context"
	"encoding/json"
	"testing"
)

func TestReview3Probe_RejectUnboundEmpiricalSource(t *testing.T) {
	e, r, assets, tc := review2ProbeSetup(t)
	_, err := e.pool.Exec(context.Background(), `UPDATE agent_tool_calls SET tool_name='list_facts',arguments='{}',output='[]' WHERE id=$1`, tc)
	if err != nil {
		t.Fatal(err)
	}
	f, err := e.runService.ReportFinding(context.Background(), r, service.ReportFindingInput{AssetID: assets[0].ID, Title: "Unbound evidence probe", SourceKind: "empirical", SourceToolCallID: &tc})
	if err == nil && f.VerificationState == "verified" {
		t.Fatal("asset-free list_facts output accepted as verified empirical finding evidence")
	}
}

type review3TextAfterReadProvider struct{ turn int }

func (p *review3TextAfterReadProvider) Call(ctx context.Context, msgs []provider.ChatMessage, defs []provider.ToolDefinition, opts provider.ModelOpts) (*provider.ModelResponse, error) {
	p.turn++
	if p.turn == 1 {
		return &provider.ModelResponse{ToolCalls: []provider.ToolCall{{ID: "unrelated_read", Type: "function", Function: provider.FunctionCall{Name: "list_facts", Arguments: `{}`}}}, FinishReason: "tool_calls"}, nil
	}
	return &provider.ModelResponse{Content: "fixed", FinishReason: "stop"}, nil
}
func TestReview3Probe_NoFixedFromNonVerificationTool(t *testing.T) {
	e, r, assets, tc := review2ProbeSetup(t)
	f, err := e.runService.ReportFinding(context.Background(), r, service.ReportFindingInput{AssetID: assets[0].ID, Title: "Retest evidence probe", SourceKind: "empirical", SourceToolCallID: &tc})
	if err != nil {
		t.Fatal(err)
	}
	rt, _, err := e.runService.CreateFindingRetest(context.Background(), r, f.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	reg := tools.NewRegistry()
	reg.Register(tools.NewListFactsTool(r, e.runService))
	ex := orchestration.NewRetestExecutor(e.runService, &review3TextAfterReadProvider{}, reg)
	done, err := ex.ExecuteFindingRetest(context.Background(), r, f.ID, rt.ID)
	if err != nil {
		t.Fatal(err)
	}
	var actualVerificationCalls int
	err = e.pool.QueryRow(context.Background(), `SELECT count(*) FROM agent_tool_calls WHERE session_id=$1 AND tool_name='get_endpoint'`, rt.SessionID).Scan(&actualVerificationCalls)
	if err != nil {
		t.Fatal(err)
	}
	if actualVerificationCalls == 0 && done.Verdict != nil && *done.Verdict == "fixed" {
		t.Fatal("list_facts plus model text fixed marked the finding fixed without any target verification")
	}
}

var _ = json.RawMessage{}
var _ = model.RunStatusRunning
