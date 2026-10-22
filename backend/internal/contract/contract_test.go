package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestFixturesMatchFrozenDTO(t *testing.T) {
	var snap WorkbenchSnapshot
	mustRead(t, "testdata/workbench.v1.json", &snap)
	if err := snap.Validate(); err != nil {
		t.Fatal(err)
	}
	if snap.WorkerCapacity.Requested != 4 || snap.WorkerCapacity.Effective != 4 {
		t.Fatalf("capacity = %+v", snap.WorkerCapacity)
	}

	var cmd Command
	mustRead(t, "testdata/command.steer_work.json", &cmd)
	if err := cmd.Validate(); err != nil {
		t.Fatal(err)
	}
	if cmd.Type != CmdSteerWork {
		t.Fatalf("type = %s", cmd.Type)
	}

	var env Envelope
	mustRead(t, "testdata/error.invalid_model_output.json", &env)
	if env.Error.Code != CodeInvalidModelOutput || !env.Error.Retryable || env.Error.SuggestedAction != ActionRetryDirection {
		t.Fatalf("envelope = %+v", env.Error)
	}

	var receipt HTTPReceipt
	mustRead(t, "testdata/http_receipt.json", &receipt)
	if err := receipt.Validate(); err != nil {
		t.Fatal(err)
	}
	if !receipt.EmpiricalSource() {
		t.Fatal("tls intercept receipt must be an empirical source")
	}
	if err := RejectModelMintedReceipt([]byte(`{"receipt_id":"66666666-6666-4666-8666-666666666666"}`)); !errors.Is(err, ErrNotImplemented) && !errors.Is(err, &Error{Code: CodeUnauthorized}) {
		t.Fatal(err)
	}

	var job JobView
	mustRead(t, "testdata/job.finished.json", &job)
	if err := job.Validate(); err != nil {
		t.Fatal(err)
	}
	if job.ExitCode == nil || *job.ExitCode != 0 {
		t.Fatal("finished fixture lost exit_code")
	}

	var wrapped struct {
		Agents AgentsConfig `json:"agents"`
	}
	mustRead(t, "testdata/agents.config.json", &wrapped)
	if err := wrapped.Agents.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestFixturesContainNoSecrets(t *testing.T) {
	matches, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatal(err)
	}
	banned := []string{"authorization:", "bearer ", "api_key", "password", "sk-", "cookie:"}
	for _, name := range matches {
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(body))
		for _, word := range banned {
			if strings.Contains(lower, word) {
				t.Fatalf("%s contains %q", name, word)
			}
		}
	}
}

func TestLegacyAgentsConfigStaysAbsent(t *testing.T) {
	_, present, err := ParseAgentsConfig(nil)
	if err != nil || present {
		t.Fatalf("present=%v err=%v", present, err)
	}
	_, present, err = ParseAgentsConfig([]byte(`{"budget":{"max_parallel_workers":1}}`))
	if err != nil || present {
		t.Fatalf("legacy present=%v err=%v", present, err)
	}
	cfg, present, err := ParseAgentsConfig([]byte(`{"agents":{"mainagent":{"runtime_profile":"default","model":"configured-model"},"planner":{"runtime_profile":"default","model":"configured-model"},"worker":{"runtime_profile":"default","model":"configured-model"},"verifier":{"runtime_profile":"default","model":"configured-model"}}}`))
	if err != nil || !present || cfg.Worker.Model != "configured-model" {
		t.Fatalf("cfg=%+v present=%v err=%v", cfg, present, err)
	}
	_, present, err = ParseAgentsConfig([]byte(`{"agents":{"worker":{"runtime_profile":"../bin","model":"x"}}}`))
	if err == nil || !present {
		t.Fatal("invalid profile accepted")
	}
}

func TestCommandHashIgnoresWhitespaceButNotPayloadOrder(t *testing.T) {
	var cmd Command
	mustRead(t, "testdata/command.steer_work.json", &cmd)
	first, err := CommandRequestHash(cmd)
	if err != nil || len(first) != 64 {
		t.Fatal(err)
	}
	again, err := CommandRequestHash(cmd)
	if err != nil || again != first {
		t.Fatalf("hash changed: %s %s", first, again)
	}
	cmd.Payload = []byte(`{"instruction":"different"}`)
	other, err := CommandRequestHash(cmd)
	if err != nil || other == first {
		t.Fatal("different payload hashed equal")
	}
}

func TestWorkerCountIsNeverSilentlyClamped(t *testing.T) {
	got, err := DiscoveryMustHonorRequestedWorkers(4, 8)
	if err != nil || got != 4 {
		t.Fatalf("discovery N=4 => %d, %v", got, err)
	}
	if _, err := AcceptWorkerCount(9, 8); !errors.Is(err, (&Error{Code: CodeConcurrencyOutOfRange})) {
		t.Fatalf("out of range err = %v", err)
	}
	if _, err := AcceptWorkerCount(4, 1); !errors.Is(err, (&Error{Code: CodeConcurrencyOutOfRange})) {
		t.Fatalf("server max must reject 4, err = %v", err)
	}
	legacy, err := ResolveRequestedWorkers(false, 0, false, 8)
	if err != nil || legacy != LegacyMissingWorkerDefault {
		t.Fatalf("legacy = %d, %v", legacy, err)
	}
	fresh, err := ResolveRequestedWorkers(false, 0, true, 8)
	if err != nil || fresh != DefaultExplorationWorkers {
		t.Fatalf("new exploration = %d, %v", fresh, err)
	}
}

func TestConfigRevisionDoesNotChangeRunningEpoch(t *testing.T) {
	current := RuntimeConfigRevision{Version: 1, EpochNumber: 2, Agents: testAgents(), Protocol: ProtocolResponses}
	if _, err := NextConfigRevision(current, testAgents(), 2); !errors.Is(err, &Error{Code: CodeStaleEpoch}) {
		t.Fatal(err)
	}
	next, err := NextConfigRevision(current, testAgents(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if next.Version != 2 || next.EpochNumber != 3 {
		t.Fatalf("next = %+v", next)
	}
}

func TestCommandAuthorization(t *testing.T) {
	human := Principal{Kind: PrincipalHuman, ActorID: "operator", RunID: uuid.New()}
	planner := Principal{Kind: PrincipalPlanner, ActorID: "planner", RunID: human.RunID}
	main := Principal{Kind: PrincipalMainAgent, ActorID: "main", RunID: human.RunID}
	if err := CommandAllowed(planner, CmdGrantBudget); !errors.Is(err, &Error{Code: CodeUnauthorized}) {
		t.Fatal(err)
	}
	if err := CommandAllowed(planner, CmdStartEpoch); !errors.Is(err, &Error{Code: CodeUnauthorized}) {
		t.Fatal(err)
	}
	if err := CommandAllowed(planner, CmdSteerWork); err != nil {
		t.Fatal(err)
	}
	if err := CommandAllowed(main, CmdStopRun); !errors.Is(err, &Error{Code: CodeUnauthorized}) {
		t.Fatal(err)
	}
	main.ExplicitUserCommand = true
	if err := CommandAllowed(main, CmdStopRun); err != nil {
		t.Fatal(err)
	}
	if err := CommandAllowed(main, CmdGrantBudget); !errors.Is(err, &Error{Code: CodeUnauthorized}) {
		t.Fatal(err)
	}
	if err := CommandAllowed(human, CmdGrantBudget); err != nil {
		t.Fatal(err)
	}
	if _, err := PrincipalFromRequestJSON([]byte(`{"kind":"human"}`)); !errors.Is(err, &Error{Code: CodeUnauthorized}) {
		t.Fatal(err)
	}
	if err := RequireIdempotencyKey(""); err == nil {
		t.Fatal("empty idempotency key accepted")
	}
}

func TestIncompleteModelOutputIsNotExecutable(t *testing.T) {
	err := RejectIncompleteToolCalls(&ModelCallResult{
		Executable: false,
		ToolCalls:  []ModelToolCall{{ID: "call_1", Name: "http_request", Arguments: "{", Incomplete: true, Status: "incomplete"}},
		ProtocolError: &ErrorBody{
			Code:    CodeInvalidModelOutput,
			Message: "incomplete function arguments or status",
		},
	})
	if !errors.Is(err, &Error{Code: CodeInvalidModelOutput}) {
		t.Fatal(err)
	}
}

func TestCallOnceDoesNotRetryOrSucceedClosed(t *testing.T) {
	scope := workerScope()
	req := ModelCallRequest{Scope: scope}
	if _, err := CallOnce(context.Background(), UnimplementedModelPolicy{}, UnimplementedModelCaller{}, scope, req); !errors.Is(err, ErrNotImplemented) {
		t.Fatal(err)
	}

	policy := &countingPolicy{}
	caller := &countingCaller{err: errors.New("upstream timeout")}
	if _, err := CallOnce(context.Background(), policy, caller, scope, req); err == nil || caller.calls != 1 || policy.completes != 1 {
		t.Fatalf("err=%v calls=%d completes=%d", err, caller.calls, policy.completes)
	}

	caller.err = nil
	caller.result = &ModelCallResult{Executable: false, ProtocolError: &ErrorBody{Code: CodeInvalidModelOutput, Message: "incomplete"}}
	if _, err := CallOnce(context.Background(), policy, caller, scope, req); !errors.Is(err, &Error{Code: CodeInvalidModelOutput}) || caller.calls != 2 {
		t.Fatalf("err=%v calls=%d", err, caller.calls)
	}
}

func TestPortsFailClosed(t *testing.T) {
	if err := (ExecutionPorts{}).ProductionReady(); !errors.Is(err, ErrNotImplemented) {
		t.Fatal(err)
	}
	ports := ExecutionPorts{
		Runtime:     UnimplementedRuntime{},
		ModelPolicy: UnimplementedModelPolicy{},
		Controller:  UnimplementedController{},
		Tools:       UnimplementedToolset{},
		ToolPolicy:  UnimplementedToolPolicy{},
		Transport:   UnimplementedTransport{},
		Capture:     UnimplementedCapture{},
		Jobs:        UnimplementedJobs{},
		Revoker:     UnimplementedRevoker{},
		Workbench:   UnimplementedWorkbench{},
	}
	if err := ports.ProductionReady(); !errors.Is(err, ErrNotImplemented) {
		t.Fatal(err)
	}
	if _, err := (UnimplementedCapture{}).Exchange(context.Background(), ExecutionScope{}, HTTPRequestInput{}); !errors.Is(err, ErrNotImplemented) {
		t.Fatal(err)
	}
	if _, err := (UnimplementedJobs{}).Run(context.Background(), ExecutionScope{}, JobSpec{}); err == nil {
		t.Fatal("unimplemented job returned success")
	}
}

func TestScopeAndReceiptRules(t *testing.T) {
	if err := (ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: RolePlanner, WorkItemID: uuidPtr(uuid.New())}).Validate(); err == nil {
		t.Fatal("planner forged a worker id")
	}
	if err := workerScope().Validate(); err != nil {
		t.Fatal(err)
	}
	killed := JobView{JobID: uuid.New(), State: JobKilled, ExitCode: intPtr(0)}
	if err := killed.Validate(); err == nil {
		t.Fatal("killed job forged exit code 0")
	}
	meta := HTTPReceipt{RequestID: uuid.New(), ReceiptID: uuid.New(), CaptureMode: CaptureConnectMeta, ResponseArtifactID: uuidPtr(uuid.New())}
	if err := meta.Validate(); err == nil || meta.EmpiricalSource() {
		t.Fatal("connect metadata treated as a body")
	}
	in := HTTPRequestInput{URL: "https://user:pw@fixture.local", Method: "GET"}
	if err := in.Validate(); err == nil {
		t.Fatal("embedded url credential accepted")
	}
	raw := HTTPRequestInput{URL: "https://fixture.local", Method: "POST", Headers: map[string]string{"Authorization": "secret"}}
	if err := raw.Validate(); err == nil {
		t.Fatal("raw authorization accepted")
	}
}

func TestPlannerProfilesShareControlTools(t *testing.T) {
	initial := append(append([]string{}, PlannerControlTools...), PlannerReadTools...)
	followup := append([]string{}, initial...)
	if err := RequirePlannerSupervision(initial, followup); err != nil {
		t.Fatal(err)
	}
	followup = append([]string{}, PlannerReadTools...)
	if err := RequirePlannerSupervision(initial, followup); err == nil {
		t.Fatal("follow-up without control tools accepted")
	}
}

func TestWorkerReserveAndTemplates(t *testing.T) {
	if got := WorkerModelCallsAdmittable(20, 16, 8); got != 0 {
		t.Fatalf("reserve = %d", got)
	}
	if got := WorkerModelCallsAdmittable(30, 16, 8); got != 6 {
		t.Fatalf("admittable = %d", got)
	}
	explore := SuggestedExplorationTemplate()
	if explore.MaxParallelWorkers != DefaultExplorationWorkers || explore.PlannerReservedModelCalls <= 0 {
		t.Fatalf("template = %+v", explore)
	}
}

func TestContractPackageDoesNotImportImplementations(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range file.Imports {
			path := strings.Trim(imp.Path.Value, `"`)
			if strings.Contains(path, "agent-workbench/backend/internal/") && !strings.HasSuffix(path, "/contract") {
				t.Fatalf("%s imports %s", name, path)
			}
		}
	}
}

func TestReservedMigrationsAreAbsentOrNonEmpty(t *testing.T) {
	for n := MigrationRuntimeConstraints; n <= MigrationMainControl; n++ {
		matches, err := filepath.Glob(filepath.Join("..", "..", "migrations", sprintf03(n)))
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range matches {
			body, err := os.ReadFile(name)
			if err != nil {
				t.Fatal(err)
			}
			if len(bytes.TrimSpace(body)) == 0 {
				t.Fatalf("empty reserved migration %s", name)
			}
		}
	}
}

type countingPolicy struct {
	acquires  int
	completes int
}

func (p *countingPolicy) Acquire(context.Context, ExecutionScope) (*ModelCallLease, error) {
	p.acquires++
	return &ModelCallLease{ID: uuid.New(), AcquiredAt: time.Now()}, nil
}

func (p *countingPolicy) Complete(context.Context, ModelCallLease, ModelCallRecord) error {
	p.completes++
	return nil
}

type countingCaller struct {
	calls  int
	err    error
	result *ModelCallResult
}

func (c *countingCaller) Call(context.Context, ModelCallRequest) (*ModelCallResult, error) {
	c.calls++
	return c.result, c.err
}

func workerScope() ExecutionScope {
	work := uuid.New()
	attempt := uuid.New()
	return ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: RoleWorker, WorkItemID: &work, AttemptID: &attempt}
}

func testAgents() AgentsConfig {
	cfg := AgentRoleConfig{RuntimeProfile: "default", Model: "configured-model"}
	return AgentsConfig{MainAgent: cfg, Planner: cfg, Worker: cfg, Verifier: cfg}
}

func mustRead(t *testing.T, name string, dest any) {
	t.Helper()
	body, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, dest); err != nil {
		t.Fatal(err)
	}
}

func uuidPtr(id uuid.UUID) *uuid.UUID { return &id }
func intPtr(n int) *int               { return &n }

func sprintf03(n int) string {
	return string([]byte{'0', byte('0' + n/10), byte('0' + n%10)}) + "_*.up.sql"
}
