package execution

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DeadlineSource supplies the run and attempt remainder. A nil source uses the
// caller context only. This type does not extend a deadline that has already passed.
type DeadlineSource interface {
	Remaining(ctx context.Context, scope contract.ExecutionScope) (runRemaining, attemptRemaining time.Duration, err error)
}

// Executor implements contract.JobExecutor. It does not open the Docker socket.
type Executor struct {
	cfg       Config
	store     *Store
	brokerURL string
	client    *http.Client
	gateway   contract.CaptureGateway
	deadlines DeadlineSource
	capture   *captureHub

	mu       sync.Mutex
	security map[uuid.UUID]*SecurityReport
}

func NewExecutor(cfg Config, pool *pgxpool.Pool, brokerURL string, gateway contract.CaptureGateway) (*Executor, error) {
	cfg = cfg.withDefaults()
	if cfg.WorkspaceRoot == "" {
		return nil, unavailable("execution workspace root is empty")
	}
	if err := os.MkdirAll(cfg.WorkspaceRoot, 0700); err != nil {
		return nil, unavailable("execution workspace root is not ready")
	}
	if brokerURL == "" || cfg.BrokerToken == "" {
		return nil, unavailable("execution broker is not configured")
	}
	return &Executor{
		cfg:       cfg,
		store:     NewStore(pool),
		brokerURL: strings.TrimRight(brokerURL, "/"),
		client:    &http.Client{},
		gateway:   gateway,
		capture:   newCaptureHub(),
		security:  map[uuid.UUID]*SecurityReport{},
	}, nil
}

func (e *Executor) SetDeadlineSource(src DeadlineSource) { e.deadlines = src }

// SecurityReport returns the inspect subset captured for a job in this process.
func (e *Executor) SecurityReport(id uuid.UUID) *SecurityReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.security[id]
}

func (e *Executor) Run(ctx context.Context, scope contract.ExecutionScope, spec contract.JobSpec) (*contract.JobView, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if scope.Role != contract.RoleWorker || scope.WorkItemID == nil || *scope.WorkItemID != spec.WorkItemID {
		return nil, scopeDenied("command scope does not match the work item")
	}
	if PhaseFrom(ctx) == PhaseSettlement {
		return nil, scopeDenied("settlement cannot start commands")
	}
	if spec.TimeoutSeconds <= 0 {
		spec.TimeoutSeconds = int(contract.DefaultCommandTimeout / time.Second)
	}
	if err := spec.Validate(); err != nil {
		return nil, err
	}
	argv, err := NormalizeArgv(spec.Argv)
	if err != nil {
		return nil, err
	}
	image := spec.Image
	if image == "" {
		image = e.cfg.Image
	}
	if !imageAllowed(image, e.cfg.AllowedImages) {
		return nil, scopeDenied("execution image is not allowlisted")
	}
	timeout, err := e.clamp(ctx, scope, spec.TimeoutSeconds)
	if err != nil {
		return nil, err
	}
	if _, err := e.store.Admit(ctx, scope); err != nil {
		return nil, err
	}
	origins, err := e.store.Origins(ctx, scope)
	if err != nil {
		return nil, err
	}
	offline, err := networkMode(argv, origins, e.gateway)
	if err != nil {
		return nil, err
	}
	source := sourceToolCallFrom(ctx)
	if scope.SourceToolCallID != nil {
		source = scope.SourceToolCallID
	}
	jobID := uuid.New()
	if err := e.store.InsertJob(ctx, jobID, scope, image, spec.Argv, timeout, source); err != nil {
		return nil, err
	}
	workspace, err := e.prepareWorkspace(ctx, scope)
	if err != nil {
		_ = e.store.Finish(ctx, jobID, string(contract.JobInterrupted), nil, false, string(contract.CodeExecutionUnavailable), "", nil, nil, nil, nil)
		return nil, err
	}
	req := RunRequest{
		Protocol:       ProtocolVersion,
		JobID:          jobID,
		RunID:          scope.RunID,
		AttemptID:      *scope.AttemptID,
		Image:          image,
		Argv:           spec.Argv,
		TimeoutSeconds: timeout,
		WorkspaceHost:  workspace,
		Offline:        offline,
		AllowedOrigins: origins,
	}
	var token string
	if !offline {
		base, err := e.capture.ensure()
		if err != nil {
			_ = e.store.Finish(ctx, jobID, string(contract.JobInterrupted), nil, false, string(contract.CodeExecutionUnavailable), "", nil, nil, nil, nil)
			return nil, err
		}
		token = randomToken()
		req.CaptureURL = base
		req.CaptureToken = token
		if len(origins) == 0 {
			req.AllowedOrigins = nil
		}
		e.capture.register(&captureJob{
			id: jobID, scope: scope, origins: origins, token: token, gateway: e.gateway,
		})
		defer e.capture.forget(jobID)
	} else {
		req.AllowedOrigins = nil
	}
	// The broker enforces timeout. This wait is longer so a timeout result can be
	// recorded before the client gives up and the cleanup path sends Cancel.
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second+brokerResultGrace)
	defer cancel()
	result, callErr := e.callBroker(runCtx, req)
	receipts := e.capture.forget(jobID)
	if callErr != nil || result == nil {
		_ = e.store.Finish(context.WithoutCancel(ctx), jobID, string(contract.JobOutcomeUnknown), nil, false, string(contract.CodeExecutionUnavailable), "", nil, nil, nil, nil)
		if callErr == nil {
			callErr = unavailable("execution broker returned no result")
		}
		return nil, callErr
	}
	if result.Security != nil {
		e.mu.Lock()
		e.security[jobID] = result.Security
		e.mu.Unlock()
	}
	view, err := e.persistResult(context.WithoutCancel(ctx), scope, jobID, spec.Argv, result, receipts)
	if err != nil {
		return nil, err
	}
	return view, nil
}

func networkMode(argv []string, origins []string, gateway contract.CaptureGateway) (offline bool, err error) {
	command := argv[0]
	networked := command == "/usr/bin/curl"
	if networked {
		if len(origins) == 0 {
			return true, scopeDenied("command origin is not in the work item scope")
		}
		if gateway == nil {
			return true, unavailable("capture gateway is not configured")
		}
		return false, nil
	}
	if gateway == nil || len(origins) == 0 {
		return true, nil
	}
	return false, nil
}

func (e *Executor) clamp(ctx context.Context, scope contract.ExecutionScope, requested int) (int, error) {
	want := time.Duration(requested) * time.Second
	var runRem, attemptRem time.Duration
	if e.deadlines != nil {
		var err error
		runRem, attemptRem, err = e.deadlines.Remaining(ctx, scope)
		if err != nil {
			return 0, err
		}
	}
	if deadline, ok := ctx.Deadline(); ok {
		remaining := time.Until(deadline)
		if remaining < 0 {
			remaining = 0
		}
		if runRem == 0 || remaining < runRem {
			runRem = remaining
		}
	}
	got, err := ClampCommandTimeout(want, runRem, attemptRem)
	if err != nil {
		return 0, err
	}
	secs := int(got / time.Second)
	if secs < 1 {
		return 0, invalidInput("command deadline has already passed")
	}
	if secs > int(contract.MaxCommandTimeout/time.Second) {
		secs = int(contract.MaxCommandTimeout / time.Second)
	}
	return secs, nil
}

func (e *Executor) prepareWorkspace(ctx context.Context, scope contract.ExecutionScope) (string, error) {
	dir := filepath.Join(e.cfg.WorkspaceRoot, "work", scope.WorkItemID.String())
	if err := os.MkdirAll(dir, 0777); err != nil {
		return "", unavailable("execution workspace is not ready")
	}
	if err := os.Chmod(dir, 0777); err != nil {
		return "", unavailable("execution workspace is not ready")
	}
	if err := ApproveMount(e.cfg.WorkspaceRoot, dir); err != nil {
		return "", err
	}
	files, err := e.store.LatestFiles(ctx, scope)
	if err != nil {
		return "", err
	}
	for path, content := range files {
		if err := WriteWithin(dir, path, []byte(content)); err != nil {
			return "", err
		}
	}
	if err := e.store.TouchWorkspace(ctx, scope); err != nil {
		return "", err
	}
	return dir, nil
}

func (e *Executor) persistResult(ctx context.Context, scope contract.ExecutionScope, jobID uuid.UUID, argv []string, result *RunResult, receipts []uuid.UUID) (*contract.JobView, error) {
	stdoutID, err := e.store.SaveChildArtifact(ctx, scope, "stdout.txt", "text/plain", result.Stdout)
	if err != nil {
		return nil, err
	}
	stderrID, err := e.store.SaveChildArtifact(ctx, scope, "stderr.txt", "text/plain", result.Stderr)
	if err != nil {
		return nil, err
	}
	scriptID, err := e.store.SaveChildArtifact(ctx, scope, "command.txt", "text/plain", strings.Join(argv, "\n"))
	if err != nil {
		return nil, err
	}
	if err := e.store.LinkArtifact(ctx, jobID, stdoutID, "stdout", ""); err != nil {
		return nil, err
	}
	if err := e.store.LinkArtifact(ctx, jobID, stderrID, "stderr", ""); err != nil {
		return nil, err
	}
	if err := e.store.LinkArtifact(ctx, jobID, scriptID, "script", ""); err != nil {
		return nil, err
	}
	var receipt *uuid.UUID
	for _, id := range receipts {
		if err := e.store.AddTraffic(ctx, jobID, id); err != nil {
			return nil, err
		}
		if receipt == nil {
			copied := id
			receipt = &copied
		}
	}
	state := result.State
	if !contract.JobState(state).Known() {
		state = string(contract.JobOutcomeUnknown)
	}
	if err := e.store.Finish(ctx, jobID, state, result.ExitCode, result.Truncated, result.ErrorCode, result.ContainerID, &stdoutID, &stderrID, &scriptID, receipt); err != nil {
		return nil, err
	}
	if err := e.store.AttachArtifacts(ctx, jobID, &stdoutID, &stderrID, &scriptID, receipt); err != nil {
		return nil, err
	}
	return e.view(ctx, jobID)
}

func (e *Executor) Cancel(ctx context.Context, scope contract.ExecutionScope, jobID uuid.UUID) (*contract.JobView, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rec, err := e.store.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if err := e.canCancel(scope, rec); err != nil {
		return nil, err
	}
	if rec.State != string(contract.JobQueued) && rec.State != string(contract.JobRunning) {
		return e.view(ctx, jobID)
	}
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), contract.CommandCleanupWindow)
	defer cancel()
	_ = e.brokerCall(clean, http.MethodPost, "/v1/jobs/"+jobID.String()+"/cancel", nil, &RunResult{})
	_ = e.store.Finish(clean, jobID, string(contract.JobKilled), nil, false, "killed", rec.ContainerID, nil, nil, nil, nil)
	return e.view(ctx, jobID)
}

func (e *Executor) canCancel(scope contract.ExecutionScope, rec *jobRecord) error {
	if scope.RunID != rec.RunID {
		return scopeDenied("execution job is outside this run")
	}
	if scope.EpochNumber < rec.Epoch {
		return staleEpoch("execution epoch cannot cancel a newer job")
	}
	if scope.Role == contract.RoleWorker && scope.EpochNumber == rec.Epoch {
		if scope.AttemptID == nil || *scope.AttemptID != rec.AttemptID {
			return scopeDenied("execution job belongs to another attempt")
		}
	}
	return nil
}

func (e *Executor) Inspect(ctx context.Context, scope contract.ExecutionScope, jobID uuid.UUID) (*contract.JobView, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rec, err := e.store.Get(ctx, jobID)
	if err != nil {
		return nil, err
	}
	if scope.RunID != rec.RunID {
		return nil, scopeDenied("execution job is outside this run")
	}
	if scope.Role == contract.RoleWorker {
		if scope.WorkItemID == nil || *scope.WorkItemID != rec.WorkItemID {
			return nil, scopeDenied("execution job belongs to another work item")
		}
		if scope.EpochNumber < rec.Epoch {
			return nil, staleEpoch("execution epoch cannot inspect a newer job")
		}
	}
	return e.view(ctx, jobID)
}

func (e *Executor) view(ctx context.Context, id uuid.UUID) (*contract.JobView, error) {
	rec, err := e.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	out := &contract.JobView{
		JobID:              rec.ID,
		State:              contract.JobState(rec.State),
		ExitCode:           rec.ExitCode,
		StdoutArtifactID:   rec.StdoutID,
		StderrArtifactID:   rec.StderrID,
		TrafficRefs:        rec.Traffic,
		ExecutionReceiptID: rec.ReceiptID,
	}
	if err := out.Validate(); err != nil {
		return nil, err
	}
	return out, nil
}

func (e *Executor) WriteFile(ctx context.Context, scope contract.ExecutionScope, rel string, content []byte) (uuid.UUID, error) {
	if err := scope.Validate(); err != nil {
		return uuid.Nil, err
	}
	if PhaseFrom(ctx) == PhaseSettlement {
		return uuid.Nil, scopeDenied("settlement cannot write workspace files")
	}
	if _, err := e.store.Admit(ctx, scope); err != nil {
		return uuid.Nil, err
	}
	dir, err := e.prepareWorkspace(ctx, scope)
	if err != nil {
		return uuid.Nil, err
	}
	if err := WriteWithin(dir, rel, content); err != nil {
		return uuid.Nil, err
	}
	safe, err := SafeRelative(rel)
	if err != nil {
		return uuid.Nil, err
	}
	id, err := e.store.SaveChildArtifact(ctx, scope, "file.txt", "text/plain", string(content))
	if err != nil {
		return uuid.Nil, err
	}
	if err := e.store.InsertFile(ctx, scope, id, safe); err != nil {
		return uuid.Nil, err
	}
	return id, nil
}

func (e *Executor) ReadFile(ctx context.Context, scope contract.ExecutionScope, rel string, offset, limit int) ([]byte, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if scope.Role != contract.RoleWorker || scope.WorkItemID == nil {
		return nil, scopeDenied("workspace read requires a worker scope")
	}
	dir := filepath.Join(e.cfg.WorkspaceRoot, "work", scope.WorkItemID.String())
	if _, err := os.Stat(dir); err != nil {
		if _, err := e.prepareWorkspace(ctx, scope); err != nil {
			return nil, err
		}
	}
	return ReadWithin(dir, rel, offset, limit)
}

func (e *Executor) callBroker(ctx context.Context, req RunRequest) (*RunResult, error) {
	var result RunResult
	err := e.brokerCall(ctx, http.MethodPost, "/v1/jobs", req, &result)
	if err == nil {
		return &result, nil
	}
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), contract.CommandCleanupWindow)
	defer cancel()
	var inspected RunResult
	if inspectErr := e.brokerCall(clean, http.MethodGet, "/v1/jobs/"+req.JobID.String(), nil, &inspected); inspectErr == nil && inspected.State != string(contract.JobRunning) && inspected.State != string(contract.JobQueued) && inspected.State != "" {
		return &inspected, nil
	}
	_ = e.brokerCall(clean, http.MethodPost, "/v1/jobs/"+req.JobID.String()+"/cancel", nil, &RunResult{})
	if inspectErr := e.brokerCall(clean, http.MethodGet, "/v1/jobs/"+req.JobID.String(), nil, &inspected); inspectErr == nil && inspected.State != "" {
		if inspected.State == string(contract.JobRunning) || inspected.State == string(contract.JobQueued) {
			return nil, unavailable("execution job was still running after cancel")
		}
		return &inspected, nil
	}
	return nil, err
}

func (e *Executor) brokerCall(ctx context.Context, method, path string, in any, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, e.brokerURL+path, body)
	if err != nil {
		return unavailable("execution broker call failed")
	}
	req.Header.Set("Authorization", "Bearer "+e.cfg.BrokerToken)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := e.client.Do(req)
	if err != nil {
		return unavailable("execution broker call failed")
	}
	defer res.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(res.Body, 8<<20))
	if err != nil {
		return unavailable("execution broker call failed")
	}
	if res.StatusCode >= 300 {
		var env contract.Envelope
		if json.Unmarshal(payload, &env) == nil && env.Error.Code != "" {
			return &contract.Error{Code: env.Error.Code, Scope: env.Error.Scope, Message: env.Error.Message, Retryable: env.Error.Retryable, SuggestedAction: env.Error.SuggestedAction}
		}
		return unavailable("execution broker rejected the job")
	}
	if out == nil || len(payload) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return unavailable("execution broker returned an unknown result")
	}
	return nil
}

func randomToken() string {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return uuid.NewString()
	}
	return hex.EncodeToString(buf)
}

// Revoke implements the job portion of contract.ResourceRevoker. Other kinds
// are refused so this executor is not treated as the only revoker.
func (e *Executor) Revoke(ctx context.Context, req contract.RevokeRequest) (*contract.RevokeReport, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	switch req.Kind {
	case contract.RevokeJob:
		if _, err := e.Cancel(ctx, req.Scope, *req.JobID); err != nil {
			return nil, err
		}
		return &contract.RevokeReport{Kind: req.Kind, Cancelled: 1}, nil
	case contract.RevokeAttempt:
		if req.Scope.AttemptID == nil {
			return nil, invalidInput("attempt revoke requires attempt_id")
		}
		jobs, err := e.store.ListAttemptJobs(ctx, *req.Scope.AttemptID)
		if err != nil {
			return nil, err
		}
		report := &contract.RevokeReport{Kind: req.Kind}
		for _, job := range jobs {
			if _, err := e.Cancel(ctx, req.Scope, job.ID); err != nil {
				report.OutcomeUnknown++
				continue
			}
			report.Cancelled++
		}
		return report, nil
	default:
		return nil, invalidInput("execution revoker does not handle " + string(req.Kind))
	}
}

// Reconcile stops containers for non-terminal jobs and marks those rows
// outcome_unknown only after the broker reports the kill. It does not replay commands.
func (e *Executor) Reconcile(ctx context.Context) error {
	jobs, err := e.store.ListNonTerminal(ctx)
	if err != nil {
		return err
	}
	refs, err := e.store.ListContainerRefs(ctx)
	if err != nil {
		return err
	}
	kills := make([]reconcileKill, 0, len(refs))
	for _, ref := range refs {
		if ref.ContainerID == "" {
			continue
		}
		kills = append(kills, reconcileKill{ID: ref.ContainerID, JobID: ref.ID})
	}
	clean, cancel := context.WithTimeout(context.WithoutCancel(ctx), contract.CommandCleanupWindow)
	defer cancel()
	if err := e.brokerCall(clean, http.MethodPost, "/v1/reconcile", reconcileRequest{Protocol: ProtocolVersion, KillContainers: kills}, &struct {
		Killed int `json:"killed"`
	}{}); err != nil {
		return err
	}
	for _, job := range jobs {
		if err := e.store.Finish(clean, job.ID, string(contract.JobOutcomeUnknown), nil, false, string(contract.CodeExecutionUnavailable), job.ContainerID, nil, nil, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

var _ contract.JobExecutor = (*Executor)(nil)
var _ contract.ResourceRevoker = (*Executor)(nil)
