package test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/controlplane"
	"agent-workbench/backend/internal/handler"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

func requireMA5(t *testing.T) {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	parsed, err := url.Parse(raw)
	name := ""
	if err == nil {
		name = strings.TrimPrefix(parsed.Path, "/")
	}
	switch name {
	case "", "agent_workbench", "agent_workbench_test", "oma_artex_test", "oma_phase2_review_4484633",
		"agent_workbench_ma1", "agent_workbench_ma2", "agent_workbench_ma3", "agent_workbench_ma4",
		"agent_workbench_ma5", "agent_workbench_ma6", "agent_workbench_ma7":
		t.Fatalf("refusing TEST_DATABASE_URL %q; control tests truncate and require /agent_workbench_ma_integration", raw)
	}
	if name != "agent_workbench_ma_integration" {
		t.Fatalf("refusing TEST_DATABASE_URL %q; control tests truncate and require /agent_workbench_ma_integration", raw)
	}
}

func connectMA5(t *testing.T) *TestEnv {
	t.Helper()
	requireMA5(t)
	env := setupTestEnv(t, false)
	var name string
	if err := env.pool.QueryRow(context.Background(), `SELECT current_database()`).Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "agent_workbench_ma_integration" {
		t.Fatalf("connected to %s", name)
	}
	return env
}

type holdExec struct {
	epoch  int
	mu     sync.Mutex
	kills  []uuid.UUID
	steers int
	pauses int
}

func (e *holdExec) Run(ctx context.Context) { <-ctx.Done() }
func (e *holdExec) Epoch() int              { return e.epoch }
func (e *holdExec) Pause(context.Context) error {
	e.mu.Lock()
	e.pauses++
	e.mu.Unlock()
	return nil
}
func (e *holdExec) Resume(context.Context) error        { return nil }
func (e *holdExec) RefreshBudget(context.Context) error { return nil }
func (e *holdExec) KillWorkItem(_ context.Context, id uuid.UUID, _ string) error {
	e.mu.Lock()
	e.kills = append(e.kills, id)
	e.mu.Unlock()
	return nil
}
func (e *holdExec) SteerWorkItem(context.Context, uuid.UUID, string) error {
	e.mu.Lock()
	e.steers++
	e.mu.Unlock()
	return nil
}
func (e *holdExec) Nudge() {}

func (e *holdExec) killCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.kills)
}

func (e *holdExec) steerCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.steers
}

type scriptProvider struct {
	mu    sync.Mutex
	steps []provider.ModelResponse
	seen  [][]provider.ChatMessage
	gate  chan struct{}
}

func (p *scriptProvider) Call(ctx context.Context, messages []provider.ChatMessage, _ []provider.ToolDefinition, _ provider.ModelOpts) (*provider.ModelResponse, error) {
	if p.gate != nil {
		select {
		case <-p.gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, append([]provider.ChatMessage{}, messages...))
	if len(p.steps) == 0 {
		return &provider.ModelResponse{Content: "noted", FinishReason: "stop"}, nil
	}
	step := p.steps[0]
	p.steps = p.steps[1:]
	return &step, nil
}

type directionRow struct {
	ItemID    uuid.UUID
	AttemptID uuid.UUID
	SessionID uuid.UUID
}

func insertRun(t *testing.T, env *TestEnv, taskID uuid.UUID, status, epochStatus string, workers int) uuid.UUID {
	t.Helper()
	id := uuid.New()
	input := fmt.Sprintf(`{"objective":"explore","budget":{"max_parallel_workers":%d,"max_model_calls":80,"main_reserved_model_calls":4}}`, workers)
	_, err := env.pool.Exec(context.Background(), `
		INSERT INTO runs (id, task_id, config_snapshot, execution_input, status, current_epoch, final_output)
		VALUES ($1,$2,'{}',$3,$4,1,'epoch-one summary')`, id, taskID, input, status)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(context.Background(), `
		INSERT INTO execution_epochs (run_id, epoch_number, status, summary)
		VALUES ($1,1,$2,'epoch-one summary')`, id, epochStatus)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func insertDirection(t *testing.T, env *TestEnv, runID uuid.UUID, key, objective string) directionRow {
	t.Helper()
	row := directionRow{ItemID: uuid.New(), AttemptID: uuid.New(), SessionID: uuid.New()}
	ctx := context.Background()
	_, err := env.pool.Exec(ctx, `
		INSERT INTO work_items (id, run_id, key, title, objective, work_type, status)
		VALUES ($1,$2,$3,$3,$4,'intent','running')`, row.ItemID, runID, key, objective)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(ctx, `
		INSERT INTO agent_sessions (id, run_id, role, work_item_id, status, epoch_number, input_snapshot)
		VALUES ($1,$2,'worker',$3,'running',1,'{}')`, row.SessionID, runID, row.ItemID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(ctx, `
		INSERT INTO attempts (id, work_item_id, attempt_number, session_id, status, epoch_number)
		VALUES ($1,$2,1,$3,'running',1)`, row.AttemptID, row.ItemID, row.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(ctx, `UPDATE work_items SET current_attempt_id=$2 WHERE id=$1`, row.ItemID, row.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(ctx, `UPDATE agent_sessions SET attempt_id=$2 WHERE id=$1`, row.SessionID, row.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = env.pool.Exec(ctx, `
		INSERT INTO agent_tool_calls (run_id, session_id, call_id, tool_name, arguments, output, status, epoch_number)
		VALUES ($1,$2,$3,'http_request','{}','seen','completed',1)`, runID, row.SessionID, "call-"+key)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func mountControl(controller contract.ExecutionController, workbench contract.WorkbenchQuery, main *controlplane.MainAgent) *chi.Mux {
	router := chi.NewRouter()
	router.Route("/api", func(r chi.Router) {
		handler.MountControlAPI(r, handler.NewControlAPI(controller, workbench, main))
	})
	return router
}

func postJSON(t *testing.T, router http.Handler, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	if key != "" {
		req.Header.Set(contract.HeaderIdempotencyKey, key)
	}
	req.Header.Set("X-Actor-Id", "operator-5")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func workStatus(t *testing.T, env *TestEnv, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := env.pool.QueryRow(context.Background(), `SELECT status FROM work_items WHERE id=$1`, id).Scan(&status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestMAControlPlannerSteerKillAndMainControls(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "ma-control-chain")
	runID := insertRun(t, env, task.ID, "running", "running", 3)
	a := insertDirection(t, env, runID, "dir-a", "读取授权入口 A")
	b := insertDirection(t, env, runID, "dir-b", "读取授权入口 B")
	c := insertDirection(t, env, runID, "dir-c", "读取授权入口 C")
	_, err := env.pool.Exec(ctx, `INSERT INTO exploration_nodes (run_id, kind, payload, state) VALUES ($1,'goal','{"text":"prove entry"}','open')`, runID)
	if err != nil {
		t.Fatal(err)
	}

	exec := &holdExec{epoch: 1}
	if err := env.manager.StartExecutor(runID, exec); err != nil {
		t.Fatal(err)
	}
	replan := controlplane.NewMemoryReplan()
	var recoveries int
	controller := controlplane.NewController(env.runService, &controlplane.ManagerLive{Manager: env.manager}, replan, func(context.Context, uuid.UUID, string, string, int, string) (int, bool, error) {
		recoveries++
		return 0, false, fmt.Errorf("recovery was not requested")
	})
	workbench := controlplane.NewWorkbench(env.runService, nil)
	model := &scriptProvider{steps: []provider.ModelResponse{
		{FinishReason: "tool_calls", ToolCalls: []provider.ToolCall{{ID: "call_sit", Type: "function", Function: provider.FunctionCall{Name: "get_situation", Arguments: "{}"}}}},
		{Content: "C remains in progress under the stored steer", FinishReason: "stop"},
	}}
	main := controlplane.NewMainAgent(env.runService, controller, workbench, model, nil)
	main.SyncDrain = true
	router := mountControl(controller, workbench, main)

	sup := controlplane.NewSupervision(env.runService, controller)
	plannerScope := contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RolePlanner, SessionID: &a.SessionID}
	initial, err := sup.Assemble(ctx, plannerScope, contract.ProfilePlannerInitial)
	if err != nil {
		t.Fatal(err)
	}
	followup, err := sup.Assemble(ctx, plannerScope, contract.ProfilePlannerFollowup)
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.RequirePlannerSupervision(initial.Names, followup.Names); err != nil {
		t.Fatal(err)
	}
	planner := contract.Principal{Kind: contract.PrincipalPlanner, ActorID: "planner:" + a.SessionID.String(), RunID: runID}
	source := uuid.New()
	plannerScope.SourceToolCallID = &source
	reg, err := sup.Registry(plannerScope, planner, func(cmd contract.Command) string {
		return "planner:" + string(cmd.Type) + ":" + cmd.WorkItemID.String()
	}, true)
	if err != nil {
		t.Fatal(err)
	}

	steerRaw, _ := json.Marshal(map[string]string{"work_item_id": a.ItemID.String(), "expected_attempt_id": a.AttemptID.String(), "instruction": "只读取授权入口"})
	steerTool, err := reg.Validate("steer_work", steerRaw)
	if err != nil {
		t.Fatal(err)
	}
	steerOut, err := steerTool.Execute(ctx, steerRaw)
	if err != nil {
		t.Fatal(err)
	}
	var steerResult contract.CommandResult
	if err := json.Unmarshal([]byte(steerOut), &steerResult); err != nil || steerResult.CommandID == uuid.Nil || steerResult.Status != contract.CommandAccepted || !steerResult.Delivered {
		t.Fatalf("steer result = %s", steerOut)
	}
	if exec.steerCount() != 0 {
		t.Fatalf("Manager.SteerWorkItem was called %d times", exec.steerCount())
	}
	var steerMessage string
	if err := env.pool.QueryRow(ctx, `SELECT message FROM work_control_messages WHERE run_id=$1 AND work_item_id=$2`, runID, a.ItemID).Scan(&steerMessage); err != nil {
		t.Fatal(err)
	}
	if steerMessage != "只读取授权入口" {
		t.Fatalf("stored steer = %q", steerMessage)
	}
	decision := replan.DecideBeforeTool(a.ItemID, []controlplane.ClosedToolCall{{ID: "call_done", Name: "read_file"}, {ID: "call_next", Name: "http_request"}}, 1)
	if !decision.CloseUnexecuted || len(decision.ClosedCalls) != 1 || decision.ClosedCalls[0].ID != "call_next" {
		t.Fatalf("replan decision = %+v", decision)
	}
	assertBothProtocolsClosed(t, decision)

	denied, err := reg.Validate("steer_work", []byte(`{"work_item_id":"`+a.ItemID.String()+`","instruction":"x","scope":{"allow":["*"]}}`))
	if err == nil {
		out, execErr := denied.Execute(ctx, []byte(`{"work_item_id":"`+a.ItemID.String()+`","instruction":"x","scope":{"allow":["*"]}}`))
		if execErr != nil {
			t.Fatal(execErr)
		}
		if !strings.Contains(out, "unauthorized") {
			t.Fatalf("scope expansion output = %s", out)
		}
	}
	if _, err := controller.Apply(ctx, planner, plannerScope, "planner-grant", contract.Command{Type: contract.CmdGrantBudget, ExpectedEpoch: 1, Payload: json.RawMessage(`{"additional_model_calls":10}`)}); err == nil {
		t.Fatal("planner grant_budget was accepted")
	}
	if _, err := controller.Apply(ctx, planner, plannerScope, "planner-epoch", contract.Command{Type: contract.CmdStartEpoch, ExpectedEpoch: 1, Payload: json.RawMessage(`{"reason":"no"}`)}); err == nil {
		t.Fatal("planner start_epoch was accepted")
	}
	if recoveries != 0 {
		t.Fatalf("recovery calls = %d", recoveries)
	}

	stale := postJSON(t, router, "/api/runs/"+runID.String()+"/commands", "kill-b", map[string]any{
		"type": "kill_work", "expected_epoch": 1, "work_item_id": b.ItemID, "expected_attempt_id": uuid.New(), "payload": map[string]string{"reason": "stop B"},
	})
	if stale.Code != http.StatusConflict || workStatus(t, env, b.ItemID) != "running" || exec.killCount() != 0 {
		t.Fatalf("stale attempt status=%d body=%s item=%s kills=%d", stale.Code, stale.Body.String(), workStatus(t, env, b.ItemID), exec.killCount())
	}
	kill := postJSON(t, router, "/api/runs/"+runID.String()+"/commands", "kill-b", map[string]any{
		"type": "kill_work", "expected_epoch": 1, "work_item_id": b.ItemID, "expected_attempt_id": b.AttemptID, "payload": map[string]string{"reason": "stop B"},
	})
	if kill.Code != http.StatusOK {
		t.Fatalf("kill status=%d body=%s", kill.Code, kill.Body.String())
	}
	var killResult contract.CommandResult
	if err := json.Unmarshal(kill.Body.Bytes(), &killResult); err != nil {
		t.Fatal(err)
	}
	if killResult.Status != contract.CommandApplied || !killResult.Delivered || workStatus(t, env, b.ItemID) != "cancelled" || exec.killCount() != 1 {
		t.Fatalf("kill result=%+v status=%s kills=%d", killResult, workStatus(t, env, b.ItemID), exec.killCount())
	}
	replay := postJSON(t, router, "/api/runs/"+runID.String()+"/commands", "kill-b", map[string]any{
		"type": "kill_work", "expected_epoch": 1, "work_item_id": b.ItemID, "expected_attempt_id": b.AttemptID, "payload": map[string]string{"reason": "stop B"},
	})
	var replayResult contract.CommandResult
	if err := json.Unmarshal(replay.Body.Bytes(), &replayResult); err != nil {
		t.Fatal(err)
	}
	if replay.Code != http.StatusOK || replayResult.CommandID != killResult.CommandID || exec.killCount() != 1 {
		t.Fatalf("replay code=%d id=%s kills=%d", replay.Code, replayResult.CommandID, exec.killCount())
	}
	conflict := postJSON(t, router, "/api/runs/"+runID.String()+"/commands", "kill-b", map[string]any{
		"type": "kill_work", "expected_epoch": 1, "work_item_id": b.ItemID, "expected_attempt_id": b.AttemptID, "payload": map[string]string{"reason": "different"},
	})
	if conflict.Code != http.StatusConflict || exec.killCount() != 1 {
		t.Fatalf("conflict code=%d body=%s", conflict.Code, conflict.Body.String())
	}
	if workStatus(t, env, c.ItemID) != "running" {
		t.Fatalf("C status = %s", workStatus(t, env, c.ItemID))
	}

	var audits int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM execution_commands WHERE run_id=$1 AND principal_kind IN ('planner','human')`, runID).Scan(&audits); err != nil || audits < 2 {
		t.Fatalf("audit rows=%d err=%v", audits, err)
	}

	accepted, err := main.Submit(ctx, contract.Principal{Kind: contract.PrincipalHuman, ActorID: "operator-5", RunID: runID}, "main-steer-c", contract.MainMessageSubmit{
		Message: "纠偏工作项 " + c.ItemID.String() + "：只读取授权入口", ExpectedEpoch: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	var cited uuid.UUID
	if err := env.pool.QueryRow(ctx, `SELECT command_id FROM main_messages WHERE turn_id=$1 AND role='action'`, accepted.TurnID).Scan(&cited); err != nil {
		t.Fatal(err)
	}
	if cited == uuid.Nil {
		t.Fatal("action message did not cite a command")
	}
	var mainExplicit bool
	if err := env.pool.QueryRow(ctx, `SELECT explicit_user_command FROM execution_commands WHERE id=$1 AND principal_kind='mainagent'`, cited).Scan(&mainExplicit); err != nil || !mainExplicit {
		t.Fatalf("main audit explicit=%v err=%v", mainExplicit, err)
	}
	if workStatus(t, env, c.ItemID) != "running" || exec.steerCount() != 0 {
		t.Fatalf("C after main steer status=%s managerSteers=%d", workStatus(t, env, c.ItemID), exec.steerCount())
	}
	var cSteer string
	if err := env.pool.QueryRow(ctx, `SELECT message FROM work_control_messages WHERE work_item_id=$1`, c.ItemID).Scan(&cSteer); err != nil || cSteer != "只读取授权入口" {
		t.Fatalf("main steer stored %q err=%v", cSteer, err)
	}
	model.mu.Lock()
	sawSituation := false
	for _, batch := range model.seen {
		for _, msg := range batch {
			if msg.Role == provider.RoleTool && strings.Contains(msg.Content, c.ItemID.String()) && strings.Contains(msg.Content, "workbench.v1") {
				sawSituation = true
			}
		}
	}
	model.mu.Unlock()
	if !sawSituation {
		t.Fatal("main agent did not receive a real situation snapshot")
	}

	snap, err := workbench.Snapshot(ctx, runID, contract.WorkbenchQueryParams{})
	if err != nil {
		t.Fatal(err)
	}
	if snap.Run.Status != "running" || snap.Run.EpochNumber != 1 || snap.WorkerCapacity.Active != 2 || snap.WorkerCapacity.Effective != 3 {
		t.Fatalf("snapshot capacity = %+v run=%+v", snap.WorkerCapacity, snap.Run)
	}
	if snap.Planner.Status != "waiting" || snap.Planner.Reason != "waiting for in-flight worker directions" {
		t.Fatalf("planner view = %+v", snap.Planner)
	}
	if len(snap.Goals) == 0 {
		t.Fatal("goal was missing from the snapshot")
	}
	foundC := false
	for _, d := range snap.Directions {
		if d.WorkItemID == c.ItemID {
			foundC = true
			if d.LatestStep != "http_request" || len(d.AllowedActions) != 2 {
				t.Fatalf("C direction = %+v", d)
			}
		}
		if d.WorkItemID == b.ItemID && len(d.AllowedActions) != 0 {
			t.Fatalf("cancelled B still offers %+v", d.AllowedActions)
		}
	}
	if !foundC {
		t.Fatal("snapshot omitted C")
	}
}

func assertBothProtocolsClosed(t *testing.T, decision controlplane.ToolBatchDecision) {
	t.Helper()
	calls := []provider.ToolCall{
		{ID: "call_done", Function: provider.FunctionCall{Name: "read_file"}},
		{ID: "call_next", Function: provider.FunctionCall{Name: "http_request"}},
	}
	chatAssistant := provider.ChatMessage{Role: provider.RoleAssistant, ToolCalls: calls}
	responsesAssistant := provider.ChatMessage{Role: provider.RoleAssistant, ToolCalls: calls, NativePayload: &provider.NativePayload{Format: "responses", Output: []json.RawMessage{
		json.RawMessage(`{"type":"function_call","call_id":"call_done","name":"read_file"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_next","name":"http_request"}`),
	}}}
	for _, assistant := range []provider.ChatMessage{chatAssistant, responsesAssistant} {
		closed := controlplane.CloseUnexecutedBatch(assistant, 1, decision)
		transcript := []provider.ChatMessage{assistant, {Role: provider.RoleTool, ToolCallID: "call_done", Content: "already executed"}}
		transcript = append(transcript, closed...)
		if err := controlplane.ProtocolPairingOK(transcript); err != nil {
			t.Fatal(err)
		}
		if len(closed) == 0 || !strings.Contains(closed[0].Content, "只读取授权入口") {
			t.Fatalf("closed batch = %+v", closed)
		}
		broken := []provider.ChatMessage{assistant, closed[len(closed)-1], closed[0]}
		if err := controlplane.ProtocolPairingOK(broken); err == nil {
			t.Fatal("guidance before the tool result was accepted")
		}
	}
}

func TestMAControlMainPauseQuestionAndBudget(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "ma-control-pause")
	runID := insertRun(t, env, task.ID, "running", "running", 1)
	item := insertDirection(t, env, runID, "dir-q", "读取授权入口")
	exec := &holdExec{epoch: 1}
	if err := env.manager.StartExecutor(runID, exec); err != nil {
		t.Fatal(err)
	}
	controller := controlplane.NewController(env.runService, &controlplane.ManagerLive{Manager: env.manager}, controlplane.NewMemoryReplan(), nil)
	workbench := controlplane.NewWorkbench(env.runService, nil)
	model := &scriptProvider{steps: []provider.ModelResponse{
		{Content: "still running", FinishReason: "stop"},
		{Content: "I killed it", FinishReason: "stop"},
	}}
	main := controlplane.NewMainAgent(env.runService, controller, workbench, model, nil)
	main.SyncDrain = true

	if _, err := main.Submit(ctx, contract.Principal{Kind: contract.PrincipalHuman, ActorID: "operator-5", RunID: runID}, "question", contract.MainMessageSubmit{
		Message: "为什么工作项还在运行？", ExpectedEpoch: 1,
	}); err != nil {
		t.Fatal(err)
	}
	if workStatus(t, env, item.ItemID) != "running" || exec.killCount() != 0 {
		t.Fatal("a question changed work state")
	}
	var commands int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM execution_commands WHERE run_id=$1`, runID).Scan(&commands); err != nil || commands != 0 {
		t.Fatalf("question stored %d commands", commands)
	}

	if _, err := main.Submit(ctx, contract.Principal{Kind: contract.PrincipalHuman, ActorID: "operator-5", RunID: runID}, "pause", contract.MainMessageSubmit{
		Message: "暂停", ExpectedEpoch: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var runStatus string
	if err := env.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus); err != nil || runStatus != "paused" {
		t.Fatalf("run status = %s err=%v", runStatus, err)
	}
	if exec.pauses == 0 {
		t.Fatal("pause did not reach the live executor")
	}
	if exec.killCount() != 0 || workStatus(t, env, item.ItemID) != "running" {
		t.Fatal("model text claiming a kill changed the worker")
	}
	var killedText int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM main_messages WHERE run_id=$1 AND role='assistant' AND text LIKE '%I killed it%'`, runID).Scan(&killedText); err != nil || killedText != 1 {
		t.Fatalf("pause turn did not keep the model text, count=%d err=%v", killedText, err)
	}

	before := runStatus
	if _, err := main.Submit(ctx, contract.Principal{Kind: contract.PrincipalHuman, ActorID: "operator-5", RunID: runID}, "budget", contract.MainMessageSubmit{
		Message: "把总预算增加 10", ExpectedEpoch: 1,
	}); err != nil {
		t.Fatal(err)
	}
	var grants int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM budget_grants WHERE run_id=$1`, runID).Scan(&grants); err != nil || grants != 0 {
		t.Fatalf("main conversation created %d grants", grants)
	}
	if err := env.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, runID).Scan(&runStatus); err != nil || runStatus != before {
		t.Fatalf("budget phrase changed status to %s", runStatus)
	}
	scope := contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RoleMainAgent}
	if _, err := controller.Apply(ctx, contract.Principal{Kind: contract.PrincipalMainAgent, ActorID: "mainagent:operator-5", RunID: runID, ExplicitUserCommand: true}, scope, "main-grant", contract.Command{Type: contract.CmdGrantBudget, ExpectedEpoch: 1, Payload: json.RawMessage(`{"additional_model_calls":10}`)}); err == nil {
		t.Fatal("main agent grant_budget was accepted")
	}
}

func TestMAControlTerminalRecoveryAndReadOnly(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "ma-control-recover")
	completedID := insertRun(t, env, task.ID, "completed", "completed", 1)
	var recoveries int
	controller := controlplane.NewController(env.runService, &controlplane.ManagerLive{Manager: env.manager}, controlplane.NewMemoryReplan(), func(ctx context.Context, runID uuid.UUID, reason, summary string, expected int, key string) (int, bool, error) {
		recoveries++
		epoch, err := env.runService.StartNewEpoch(ctx, runID, reason, summary, service.EpochStartOptions{ExpectedEpoch: expected, IdempotencyKey: key})
		if err != nil {
			return 0, false, err
		}
		if err := env.manager.StartExecutor(runID, &holdExec{epoch: epoch}); err != nil {
			return epoch, false, err
		}
		return epoch, true, nil
	})
	workbench := controlplane.NewWorkbench(env.runService, nil)
	router := mountControl(controller, workbench, nil)
	read := httptest.NewRequest(http.MethodGet, "/api/runs/"+completedID.String()+"/workbench", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, read)
	if rec.Code != http.StatusOK || recoveries != 0 {
		t.Fatalf("terminal read code=%d body=%s recoveries=%d", rec.Code, rec.Body.String(), recoveries)
	}
	rejected := postJSON(t, router, "/api/runs/"+completedID.String()+"/commands", "recover-completed", map[string]any{
		"type": "start_epoch", "expected_epoch": 1, "payload": map[string]string{"reason": "try"},
	})
	if rejected.Code != http.StatusConflict || recoveries != 0 {
		t.Fatalf("completed recovery code=%d body=%s recoveries=%d", rejected.Code, rejected.Body.String(), recoveries)
	}
	var finalOutput *string
	if err := env.pool.QueryRow(ctx, `SELECT final_output FROM runs WHERE id=$1`, completedID).Scan(&finalOutput); err != nil || finalOutput == nil || *finalOutput != "epoch-one summary" {
		t.Fatalf("completed summary changed: %v", finalOutput)
	}

	stoppedTask := createTestTask(t, env, "ma-control-stopped")
	stoppedID := insertRun(t, env, stoppedTask.ID, "stopped", "stopped", 1)
	if _, err := env.pool.Exec(ctx, `INSERT INTO model_calls (run_id, round, status, epoch_number) VALUES ($1,1,'completed',1),($1,2,'started',1)`, stoppedID); err != nil {
		t.Fatal(err)
	}
	var before int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_calls WHERE run_id=$1`, stoppedID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	started := postJSON(t, router, "/api/runs/"+stoppedID.String()+"/commands", "recover-stopped", map[string]any{
		"type": "start_epoch", "expected_epoch": 1, "payload": map[string]string{"reason": "operator requested recovery", "summary": "keep history"},
	})
	if started.Code != http.StatusOK {
		t.Fatalf("recovery code=%d body=%s", started.Code, started.Body.String())
	}
	replay := postJSON(t, router, "/api/runs/"+stoppedID.String()+"/commands", "recover-stopped", map[string]any{
		"type": "start_epoch", "expected_epoch": 1, "payload": map[string]string{"reason": "operator requested recovery", "summary": "keep history"},
	})
	if replay.Code != http.StatusOK || recoveries != 1 {
		t.Fatalf("replay code=%d recoveries=%d body=%s", replay.Code, recoveries, replay.Body.String())
	}
	var runs int
	var epoch int
	var summary string
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM runs WHERE task_id=$1`, stoppedTask.ID).Scan(&runs); err != nil || runs != 1 {
		t.Fatalf("runs=%d err=%v", runs, err)
	}
	if err := env.pool.QueryRow(ctx, `SELECT current_epoch FROM runs WHERE id=$1`, stoppedID).Scan(&epoch); err != nil || epoch != 2 {
		t.Fatalf("epoch=%d err=%v", epoch, err)
	}
	if err := env.pool.QueryRow(ctx, `SELECT summary FROM execution_epochs WHERE run_id=$1 AND epoch_number=1`, stoppedID).Scan(&summary); err != nil || summary != "epoch-one summary" {
		t.Fatalf("old summary = %q err=%v", summary, err)
	}
	var after int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM model_calls WHERE run_id=$1`, stoppedID).Scan(&after); err != nil || after != before {
		t.Fatalf("model_calls %d -> %d", before, after)
	}
}

func TestMAControlStaleEpochIsRetryable(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "ma-control-stale-epoch")
	runID := insertRun(t, env, task.ID, "running", "running", 1)
	item := insertDirection(t, env, runID, "dir-stale", "读取授权入口")
	controller := controlplane.NewController(env.runService, nil, controlplane.NewMemoryReplan(), nil)
	scope := contract.ExecutionScope{RunID: runID, EpochNumber: 2, Role: contract.RoleMainAgent}
	principal := contract.Principal{Kind: contract.PrincipalHuman, ActorID: "operator-5", RunID: runID, ExplicitUserCommand: true}
	cmd := contract.Command{Type: contract.CmdSteerWork, ExpectedEpoch: 2, WorkItemID: &item.ItemID, ExpectedAttemptID: &item.AttemptID, Payload: json.RawMessage(`{"instruction":"只读取授权入口"}`)}
	if _, err := controller.Apply(ctx, principal, scope, "stale-epoch", cmd); err == nil {
		t.Fatal("stale epoch was accepted")
	}
	var stored int
	if err := env.pool.QueryRow(ctx, `SELECT COUNT(*) FROM execution_commands WHERE run_id=$1 AND idempotency_key='stale-epoch'`, runID).Scan(&stored); err != nil || stored != 0 {
		t.Fatalf("stale epoch stored %d commands", stored)
	}
	if workStatus(t, env, item.ItemID) != "running" {
		t.Fatal("stale epoch changed the work item")
	}
	scope.EpochNumber = 1
	cmd.ExpectedEpoch = 1
	result, err := controller.Apply(ctx, principal, scope, "stale-epoch", cmd)
	if err != nil || result == nil || result.CommandID == uuid.Nil {
		t.Fatalf("corrected epoch was rejected: %v", err)
	}
	var message string
	if err := env.pool.QueryRow(ctx, `SELECT message FROM work_control_messages WHERE work_item_id=$1`, item.ItemID).Scan(&message); err != nil || message != "只读取授权入口" {
		t.Fatalf("retry stored %q err=%v", message, err)
	}
}

func TestMAControlWorkbenchConsistency(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	ctx := context.Background()
	task := createTestTask(t, env, "ma-control-view")
	runID := insertRun(t, env, task.ID, "running", "running", 1)
	for i, secret := range []string{"", "super-secret-value", ""} {
		payload := `{"note":"visible"}`
		if secret != "" {
			payload = `{"note":"visible","api_key":"super-secret-value","token":"tok"}`
		}
		if _, err := env.pool.Exec(ctx, `INSERT INTO run_events (id, run_id, seq, type, payload) VALUES (gen_random_uuid(),$1,$2,'note',$3)`, runID, i+1, payload); err != nil {
			t.Fatal(err)
		}
	}
	workbench := controlplane.NewWorkbench(env.runService, nil)
	router := mountControl(nil, workbench, nil)
	first := httptest.NewRecorder()
	router.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/workbench?after_event_seq=0&limit=1", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("page status=%d body=%s", first.Code, first.Body.String())
	}
	var page contract.WorkbenchSnapshot
	if err := json.Unmarshal(first.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.LastEventSeq != 1 || !page.HasMoreEvents || len(page.Events) != 1 || strings.Contains(first.Body.String(), "super-secret-value") {
		t.Fatalf("first page last=%d more=%v body=%s", page.LastEventSeq, page.HasMoreEvents, first.Body.String())
	}
	second := httptest.NewRecorder()
	router.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/workbench?after_event_seq=1&limit=1", nil))
	var page2 contract.WorkbenchSnapshot
	if err := json.Unmarshal(second.Body.Bytes(), &page2); err != nil {
		t.Fatal(err)
	}
	if page2.LastEventSeq != 2 || !page2.HasMoreEvents || strings.Contains(second.Body.String(), "super-secret-value") || !strings.Contains(second.Body.String(), "visible") {
		t.Fatalf("second page last=%d body=%s", page2.LastEventSeq, second.Body.String())
	}
	if _, err := env.pool.Exec(ctx, `UPDATE runs SET status='paused' WHERE id=$1`, runID); err != nil {
		t.Fatal(err)
	}
	held := httptest.NewRecorder()
	router.ServeHTTP(held, httptest.NewRequest(http.MethodGet, "/api/runs/"+runID.String()+"/workbench?after_event_seq=3&limit=1", nil))
	var heldSnap contract.WorkbenchSnapshot
	if err := json.Unmarshal(held.Body.Bytes(), &heldSnap); err != nil {
		t.Fatal(err)
	}
	if heldSnap.Run.Status != "paused" || heldSnap.LastEventSeq != 3 || len(heldSnap.Events) != 0 || heldSnap.Planner.Reason != "run admission is paused" {
		t.Fatalf("held snapshot = %+v planner=%+v", heldSnap.Run, heldSnap.Planner)
	}

	mixedTask := createTestTask(t, env, "ma-control-mixed")
	mixedID := insertRun(t, env, mixedTask.ID, "completed", "running", 1)
	mixed := httptest.NewRecorder()
	router.ServeHTTP(mixed, httptest.NewRequest(http.MethodGet, "/api/runs/"+mixedID.String()+"/workbench", nil))
	if mixed.Code == http.StatusOK || strings.Contains(mixed.Body.String(), "workbench.v1") {
		t.Fatalf("mixed snapshot was emitted: %d %s", mixed.Code, mixed.Body.String())
	}
	var envelope contract.Envelope
	if err := json.Unmarshal(mixed.Body.Bytes(), &envelope); err != nil || envelope.Error.Code != contract.CodeInvalidState {
		t.Fatalf("mixed error = %s body=%s", err, mixed.Body.String())
	}
}

func TestMAControlMessageAcceptedBeforeModel(t *testing.T) {
	env := connectMA5(t)
	defer env.teardown()
	task := createTestTask(t, env, "ma-control-accept")
	runID := insertRun(t, env, task.ID, "running", "running", 1)
	controller := controlplane.NewController(env.runService, nil, controlplane.NewMemoryReplan(), nil)
	workbench := controlplane.NewWorkbench(env.runService, nil)
	model := &scriptProvider{gate: make(chan struct{}), steps: []provider.ModelResponse{{Content: "later", FinishReason: "stop"}}}
	main := controlplane.NewMainAgent(env.runService, controller, workbench, model, nil)
	router := mountControl(controller, workbench, main)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, router, "/api/runs/"+runID.String()+"/main/messages", "msg-1", map[string]any{"message": "当前进度是什么", "expected_epoch": 1})
	}()
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("message submit waited for the model")
	}
	if rec.Code != http.StatusAccepted {
		t.Fatalf("submit status=%d body=%s", rec.Code, rec.Body.String())
	}
	var accepted contract.MainMessageAccepted
	if err := json.Unmarshal(rec.Body.Bytes(), &accepted); err != nil || accepted.TurnID == uuid.Nil {
		t.Fatalf("accepted = %s", rec.Body.String())
	}
	close(model.gate)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		err := env.pool.QueryRow(context.Background(), `SELECT status FROM main_turns WHERE id=$1`, accepted.TurnID).Scan(&status)
		if err == nil && (status == "completed" || status == "failed") {
			if status != "completed" {
				t.Fatalf("turn finished as %s", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("turn did not finish: %v %s", err, status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	replay := postJSON(t, router, "/api/runs/"+runID.String()+"/main/messages", "msg-1", map[string]any{"message": "当前进度是什么", "expected_epoch": 1})
	var again contract.MainMessageAccepted
	if err := json.Unmarshal(replay.Body.Bytes(), &again); err != nil || replay.Code != http.StatusAccepted || again.TurnID != accepted.TurnID {
		t.Fatalf("replay code=%d body=%s", replay.Code, replay.Body.String())
	}
	mismatch := postJSON(t, router, "/api/runs/"+runID.String()+"/main/messages", "msg-1", map[string]any{"message": "另一句", "expected_epoch": 1})
	if mismatch.Code != http.StatusConflict {
		t.Fatalf("mismatch code=%d body=%s", mismatch.Code, mismatch.Body.String())
	}
	missing := postJSON(t, router, "/api/runs/"+runID.String()+"/main/messages", "", map[string]any{"message": "当前进度是什么", "expected_epoch": 1})
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing key code=%d", missing.Code)
	}
}
