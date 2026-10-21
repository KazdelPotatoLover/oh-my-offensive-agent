package test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type localArtifactAdapter struct {
	name  string
	scope tools.AdapterContext
}

func (t *localArtifactAdapter) Name() string { return t.name }
func (t *localArtifactAdapter) Description() string {
	return "Read the recorded SHA-256 of an authorized local artifact"
}
func (t *localArtifactAdapter) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"artifact_id":{"type":"string"}},"required":["artifact_id"]}`)
}
func (t *localArtifactAdapter) Validate(b []byte) error {
	var in map[string]any
	return json.Unmarshal(b, &in)
}
func (t *localArtifactAdapter) Execute(ctx context.Context, b []byte) (string, error) {
	var in struct {
		ID uuid.UUID `json:"artifact_id"`
	}
	if err := json.Unmarshal(b, &in); err != nil {
		return "", err
	}
	page, err := t.scope.Service.ReadArtifact(ctx, t.scope.RunID, in.ID, 0, 128)
	if err != nil {
		return "", err
	}
	out, err := json.Marshal(map[string]any{"source_artifact_id": in.ID, "sha256": page["sha256"], "bytes": page["total_bytes"]})
	return string(out), err
}

func TestRemaining_LocalAdapterExtensionRequiresNoEngineSwitch(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	name := "local_artifact_digest_" + uuid.NewString()[:8]
	factory := func(c tools.AdapterContext) tools.Tool { return &localArtifactAdapter{name: name, scope: c} }
	require.NoError(t, tools.RegisterAdapter(tools.Adapter{Name: name, Version: "1", Factory: factory}))
	require.Error(t, tools.RegisterAdapter(tools.Adapter{Name: name, Version: "1", Factory: factory}), "adapter identity must be unique")
	a, err := e.runService.SaveArtifact(ctx, runID, nil, nil, nil, "sample.json", "application/json", `{"signal":"local evidence"}`)
	require.NoError(t, err)
	reg := tools.NewRegistry()
	p := model.CapabilityProfile{Name: "local-analysis", Version: "1", Tools: []model.CapabilityTool{{Name: name, Version: "1"}}}
	require.NoError(t, tools.RegisterProfileTools(reg, p, tools.AdapterContext{RunID: runID, Service: e.runService}))
	args, _ := json.Marshal(map[string]any{"artifact_id": a.ID})
	tool, err := reg.Validate(name, args)
	require.NoError(t, err)
	out, err := tool.Execute(ctx, args)
	require.NoError(t, err)
	require.Contains(t, out, a.SHA256)
	_, err = factory(tools.AdapterContext{RunID: uuid.New(), Service: e.runService}).Execute(ctx, args)
	require.ErrorIs(t, err, service.ErrCrossRunBoundary)
	p.Tools[0].Version = "2"
	require.Error(t, tools.RegisterProfileTools(tools.NewRegistry(), p, tools.AdapterContext{RunID: runID, Service: e.runService}))
	p.Tools[0].Version = "1"
	p.Tools[0].Disabled = true
	disabled := tools.NewRegistry()
	require.NoError(t, tools.RegisterProfileTools(disabled, p, tools.AdapterContext{RunID: runID, Service: e.runService}))
	_, exists := disabled.Get(name)
	require.False(t, exists)
}

func TestRemaining_TypedAssetsCanonicalizeWithoutExpandingScope(t *testing.T) {
	e, runID, assets, _ := review2ProbeSetup(t)
	ctx := context.Background()
	for _, kind := range []string{"app", "host", "ip", "service"} {
		derived, err := e.runService.DeriveTypedAsset(ctx, runID, assets[0].ID, kind, "")
		require.NoError(t, err)
		again, err := e.runService.DeriveTypedAsset(ctx, runID, assets[0].ID, kind, "")
		require.NoError(t, err)
		require.Equal(t, derived["asset_id"], again["asset_id"])
		rels, err := e.runService.ListAssetRelations(ctx, derived["asset_id"].(uuid.UUID))
		require.NoError(t, err)
		require.Len(t, rels, 1)
	}
	_, err := e.runService.DeriveTypedAsset(ctx, runID, assets[0].ID, "host", "outside.example")
	require.ErrorIs(t, err, service.ErrAssetOutOfScope)
	_, err = e.runService.DeriveTypedAsset(ctx, runID, uuid.New(), "host", "")
	require.ErrorIs(t, err, service.ErrAssetOutOfScope)
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "asset_scope", Title: "Scope", Objective: "Use assigned parent only", WorkType: "intent", AssetIDs: []uuid.UUID{assets[0].ID}}})
	require.NoError(t, err)
	reg := tools.NewRegistry()
	require.NoError(t, tools.RegisterProfileTools(reg, model.CapabilityProfile{Name: "derive", Version: "1", Tools: []model.CapabilityTool{{Name: "register_asset", Version: "1"}}}, tools.AdapterContext{RunID: runID, WorkItemID: items[0].ID, Service: e.runService}))
	args, _ := json.Marshal(map[string]any{"parent_asset_id": assets[1].ID, "kind": "host"})
	adapter, err := reg.Validate("register_asset", args)
	require.NoError(t, err)
	_, err = adapter.Execute(ctx, args)
	require.ErrorIs(t, err, service.ErrAssetOutOfScope, "Run scope alone must not broaden a WorkItem's assigned assets")
}

func TestRemaining_WorkspaceRejectsCrossRunSymlink(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "workspace", Title: "Workspace", Objective: "Store evidence", WorkType: "intent"}})
	require.NoError(t, err)
	root := t.TempDir()
	e.runService.SetWorkspaceRoot(root)
	workParent := filepath.Join(root, "runs", runID.String(), "work")
	require.NoError(t, os.MkdirAll(workParent, 0700))
	otherDir := filepath.Join(root, "runs", uuid.NewString())
	require.NoError(t, os.MkdirAll(otherDir, 0700))
	require.NoError(t, os.Symlink(otherDir, filepath.Join(workParent, items[0].ID.String())))
	_, err = e.runService.SaveArtifact(ctx, runID, &items[0].ID, nil, nil, "evidence.txt", "text/plain", "must stay in its own workspace")
	require.Error(t, err)
	entries, err := os.ReadDir(otherDir)
	require.NoError(t, err)
	require.Empty(t, entries)
}

func TestRemaining_StaleSessionCannotStartCallsOrWriteKnowledge(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	_, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "fenced", Title: "Fenced", Objective: "Preserve epoch", WorkType: "intent"}})
	require.NoError(t, err)
	_, _, session, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	_, err = e.runService.StartNewEpoch(ctx, runID, "Explicitly resume", "Keep old records")
	require.NoError(t, err)
	_, err = e.runService.CreateModelCallWithSession(ctx, runID, &session.ID, 1)
	require.ErrorIs(t, err, service.ErrStaleEpoch)
	_, err = e.runService.CreateAgentToolCallWithSession(ctx, runID, &session.ID, nil, "late", "get_endpoint", json.RawMessage(`{}`))
	require.ErrorIs(t, err, service.ErrStaleEpoch)
	oldCtx := service.WithExecutionEpoch(ctx, 1)
	_, err = e.runService.CreateSession(oldCtx, runID, "planner", nil, nil, nil, nil)
	require.ErrorIs(t, err, service.ErrStaleEpoch)
	_, err = e.runService.RecordHint(oldCtx, runID, "Late old guidance", nil)
	require.ErrorIs(t, err, service.ErrStaleEpoch)
	require.ErrorIs(t, e.runService.SaveContextCheckpoint(oldCtx, runID, session.ID, nil, "Late checkpoint"), service.ErrStaleEpoch)
}

func TestRemaining_InterruptedToolRestorationPreservesKnownAndUnknownOutcomes(t *testing.T) {
	e, runID, _, _ := review2ProbeSetup(t)
	ctx := context.Background()
	items, err := e.runService.SubmitPlan(ctx, runID, []service.PlanItemSpec{{Key: "restore_outcomes", Title: "Outcomes", Objective: "Never replay uncertain actions", WorkType: "intent"}})
	require.NoError(t, err)
	_, _, session, err := e.runService.ClaimNextPendingWorkItem(ctx, runID)
	require.NoError(t, err)
	calls := []provider.ToolCall{{ID: "saved_success", Function: provider.FunctionCall{Name: "read_sample", Arguments: `{}`}}, {ID: "unknown_outcome", Function: provider.FunctionCall{Name: "get_endpoint", Arguments: `{}`}}, {ID: "not_admitted", Function: provider.FunctionCall{Name: "get_endpoint", Arguments: `{}`}}}
	encoded, _ := json.Marshal(calls)
	_, err = e.runService.InsertAgentMessageWithSession(ctx, runID, &session.ID, "assistant", "", nil, encoded)
	require.NoError(t, err)
	success, err := e.runService.CreateAgentToolCallWithSession(ctx, runID, &session.ID, nil, "saved_success", "read_sample", json.RawMessage(`{}`))
	require.NoError(t, err)
	now := time.Now()
	require.NoError(t, e.runService.UpdateAgentToolCall(ctx, success.ID, "success", `{"actual":"persisted result"}`, nil, &now, &now))
	_, err = e.runService.CreateAgentToolCallWithSession(ctx, runID, &session.ID, nil, "unknown_outcome", "get_endpoint", json.RawMessage(`{}`))
	require.NoError(t, err)
	history, err := e.runService.LoadWorkItemConversation(ctx, items[0].ID)
	require.NoError(t, err)
	require.Len(t, history, 4)
	require.Contains(t, history[1].Content, `"status":"success"`)
	require.Contains(t, history[1].Content, "persisted result")
	require.Contains(t, history[2].Content, `"status":"outcome_unknown"`)
	require.Contains(t, history[3].Content, `"status":"unexecuted"`)
	page, err := e.runService.GetWorkerOutput(ctx, runID, items[0].ID, success.ID, 0, 128)
	require.NoError(t, err)
	require.Equal(t, `{"actual":"persisted result"}`, page["content"])
}
