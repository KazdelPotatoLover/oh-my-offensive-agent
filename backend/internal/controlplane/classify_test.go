package controlplane

import (
	"testing"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
)

func TestClassifyExplicitCommands(t *testing.T) {
	id := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	cases := []struct {
		message string
		kind    contract.CommandType
		wantID  bool
	}{
		{"暂停", contract.CmdPauseRun, false},
		{"继续这个任务", contract.CmdResumeRun, false},
		{"停止这个任务", contract.CmdStopRun, false},
		{"终止工作项 " + id.String(), contract.CmdKillWork, true},
		{"纠偏工作项 " + id.String() + "：只读取授权入口", contract.CmdSteerWork, true},
		{"开启新纪元", contract.CmdStartEpoch, false},
	}
	for _, tc := range cases {
		got, ok := ClassifyCommand(tc.message)
		if !ok || got.Type != tc.kind {
			t.Fatalf("%q classified as %+v ok=%v", tc.message, got, ok)
		}
		if tc.wantID && (got.WorkItemID == nil || *got.WorkItemID != id) {
			t.Fatalf("%q target = %v", tc.message, got.WorkItemID)
		}
		if !ExplicitControlRequested(tc.message) {
			t.Fatalf("%q was not an explicit control request", tc.message)
		}
	}
	if _, ok := ClassifyCommand("为什么要停止这个任务？"); ok {
		t.Fatal("a question was classified as a command")
	}
	if ExplicitControlRequested("把总预算增加 10") || !BudgetChangeRequested("把总预算增加 10") {
		t.Fatal("budget phrase was treated as an executable main command")
	}
	if _, ok := ClassifyCommand("我已经终止了工作项"); ok {
		t.Fatal("a status sentence was classified as a command")
	}
}

func TestSupervisionProfilesShareControlTools(t *testing.T) {
	runs := service.NewRunService(nil)
	sup := NewSupervision(runs, NewController(runs, nil, nil, nil))
	scope := contract.ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: contract.RolePlanner}
	initial, err := sup.Assemble(t.Context(), scope, contract.ProfilePlannerInitial)
	if err != nil {
		t.Fatal(err)
	}
	followup, err := sup.Assemble(t.Context(), scope, contract.ProfilePlannerFollowup)
	if err != nil {
		t.Fatal(err)
	}
	if err := contract.RequirePlannerSupervision(initial.Names, followup.Names); err != nil {
		t.Fatal(err)
	}
	mainScope := contract.ExecutionScope{RunID: scope.RunID, EpochNumber: 1, Role: contract.RoleMainAgent}
	mainSet, err := sup.Assemble(t.Context(), mainScope, contract.ProfileMainAgent)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range mainSet.Names {
		if name == "grant_budget" || name == "shell" || name == "start_worker" || name == "http_request" || name == "run_command" {
			t.Fatalf("main profile registered %s", name)
		}
	}
	principal := contract.Principal{Kind: contract.PrincipalMainAgent, ActorID: "main", RunID: scope.RunID, ExplicitUserCommand: true}
	reg, err := sup.Registry(mainScope, principal, nil, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, banned := range []string{"grant_budget", "shell", "start_worker", "http_request", "run_command"} {
		if _, ok := reg.Get(banned); ok {
			t.Fatalf("main registry registered %s", banned)
		}
	}
	if _, ok := reg.Get("kill_work"); !ok {
		t.Fatal("explicit main registry missing kill_work")
	}
	quiet, err := sup.Registry(mainScope, principal, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := quiet.Get("kill_work"); ok {
		t.Fatal("status question registry exposed kill_work")
	}
	workerID := uuid.New()
	attemptID := uuid.New()
	if _, err := sup.Assemble(t.Context(), contract.ExecutionScope{RunID: scope.RunID, EpochNumber: 1, Role: contract.RoleWorker, WorkItemID: &workerID, AttemptID: &attemptID}, contract.ProfileWorker); err == nil {
		t.Fatal("worker profile was assembled by the supervision factory")
	}
}
