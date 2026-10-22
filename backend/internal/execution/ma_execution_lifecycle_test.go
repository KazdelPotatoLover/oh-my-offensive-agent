package execution

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

func TestMaExecutionTimeoutKillsTheContainer(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	marker := "oma-ma4-timeout-" + uuid.NewString()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "import time; print('" + marker + "'); time.sleep(60)"},
		TimeoutSeconds: 2,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobInterrupted || view.ExitCode != nil {
		t.Fatalf("timeout job = %+v", view)
	}
	if h.ownedRunning(t) != 0 || hostCmdlineHas(marker) {
		t.Fatal("timed-out command left a process or container")
	}
}

func TestMaExecutionWorkerKillAndRunStop(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	marker := "oma-ma4-kill-" + uuid.NewString()
	cancelled := make(chan error, 1)
	go func() {
		id, err := h.waitJobContainer(t, seed.AttemptID)
		if err != nil {
			cancelled <- err
			return
		}
		_, err = h.exec.Cancel(context.Background(), seed.Scope, id)
		cancelled <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "print('before-kill', flush=True)\nimport time\ntime.sleep(60)\nprint('" + marker + "')"},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if cerr := <-cancelled; cerr != nil {
		t.Fatal(cerr)
	}
	if view.State != contract.JobKilled || view.ExitCode != nil {
		t.Fatalf("killed job = %+v", view)
	}
	if view.StdoutArtifactID == nil || !strings.Contains(h.artifactText(t, view.StdoutArtifactID), "before-kill") {
		t.Fatal("killed job did not keep its stdout artifact")
	}
	if h.ownedRunning(t) != 0 || hostCmdlineHas(marker) {
		t.Fatal("worker kill left a process or container")
	}

	stopSeed := h.seed(t)
	stopped := make(chan error, 1)
	go func() {
		id, err := h.waitJobContainer(t, stopSeed.AttemptID)
		if err != nil {
			stopped <- err
			return
		}
		_, err = h.exec.Revoke(context.Background(), contract.RevokeRequest{
			Scope: stopSeed.Scope,
			Kind:  contract.RevokeAttempt,
		})
		if err != nil {
			stopped <- err
			return
		}
		_ = id
		stopped <- nil
	}()
	view, err = h.exec.Run(ctx, stopSeed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "import time; time.sleep(60)"},
		TimeoutSeconds: 30,
		WorkItemID:     stopSeed.WorkItemID,
	})
	requireNoError(t, err)
	if serr := <-stopped; serr != nil {
		t.Fatal(serr)
	}
	if view.State != contract.JobKilled || view.ExitCode != nil {
		t.Fatalf("stopped job = %+v", view)
	}
	if h.ownedRunning(t) != 0 {
		t.Fatal("run stop left a container")
	}
}

func TestMaExecutionBackgroundChildDoesNotSurvive(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	marker := "oma-ma4-child-" + uuid.NewString()
	script := "import subprocess,sys\nsubprocess.Popen(['/usr/local/bin/python3','-c','import time; time.sleep(90); print(\"" + marker + "\")'])\nprint('parent-exit')\nsys.exit(0)\n"
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", script},
		TimeoutSeconds: 20,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobFinished || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("parent job = %+v", view)
	}
	if h.ownedRunning(t) != 0 || hostCmdlineHas(marker) {
		t.Fatal("background child survived the container")
	}
}

func TestMaExecutionOutputLimitKillsTheJob(t *testing.T) {
	h := newMa4Harness(t, Config{OutputLimitBytes: 4096})
	seed := h.seed(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "import sys\nwhile True:\n sys.stdout.write('x'*1024)\n sys.stdout.flush()\n"},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobKilled || view.ExitCode != nil {
		t.Fatalf("overflow job = %+v", view)
	}
	var truncated bool
	var code string
	if err := h.pool.QueryRow(ctx, `SELECT truncated, error_code FROM execution_jobs WHERE id=$1`, view.JobID).Scan(&truncated, &code); err != nil {
		t.Fatal(err)
	}
	if !truncated || code != string(contract.CodePayloadTooLarge) {
		t.Fatalf("truncated=%v code=%s", truncated, code)
	}
	if h.ownedRunning(t) != 0 {
		t.Fatal("overflow job left a container")
	}
}

func TestMaExecutionOldEpochCannotStartOrCancelNewerWork(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	if _, err := h.pool.Exec(context.Background(), `UPDATE runs SET current_epoch=2 WHERE id=$1`, seed.RunID); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	_, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "print('stale')"},
		TimeoutSeconds: 10,
		WorkItemID:     seed.WorkItemID,
	})
	requireCode(t, err, contract.CodeStaleEpoch)

	attempt2 := h.addAttempt(t, seed, 2, 2)
	fresh := seed.Scope
	fresh.EpochNumber = 2
	fresh.AttemptID = &attempt2
	if _, err := h.pool.Exec(context.Background(), `UPDATE runs SET current_epoch=2 WHERE id=$1`, seed.RunID); err != nil {
		t.Fatal(err)
	}
	started := make(chan error, 1)
	go func() {
		id, err := h.waitJobContainer(t, attempt2)
		if err != nil {
			started <- err
			return
		}
		_, err = h.exec.Cancel(context.Background(), seed.Scope, id)
		started <- err
		if err != nil {
			_, _ = h.exec.Cancel(context.Background(), fresh, id)
		}
	}()
	runCtx, runCancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer runCancel()
	_, runErr := h.exec.Run(runCtx, fresh, contract.JobSpec{
		Argv:           []string{"python3", "-c", "import time; time.sleep(60)"},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	if cerr := <-started; cerr == nil {
		t.Fatal("old epoch cancelled a newer job")
	} else {
		requireCode(t, cerr, contract.CodeStaleEpoch)
	}
	if runErr != nil && h.ownedRunning(t) != 0 {
		t.Fatal(runErr)
	}
	jobID := mustJobID(t, h, attempt2)
	if _, err := h.exec.Cancel(context.Background(), fresh, jobID); err != nil {
		t.Fatal(err)
	}
	if h.ownedRunning(t) != 0 {
		t.Fatal("newer scope did not stop the old-epoch container")
	}
}

func mustJobID(t *testing.T, h *ma4Harness, attemptID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := h.pool.QueryRow(context.Background(), `SELECT id FROM execution_jobs WHERE attempt_id=$1 ORDER BY created_at DESC LIMIT 1`, attemptID).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func TestMaExecutionReconcileStopsLeftoversWithoutFlippingFinishedJobs(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	api, err := newDockerAPI("/var/run/docker.sock")
	requireNoError(t, err)
	ctx := context.Background()

	queuedID := uuid.New()
	queuedContainer := startSleepContainer(t, h, api, queuedID, h.brokerID)
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO execution_jobs (id, run_id, epoch_number, work_item_id, attempt_id, image, argv, timeout_seconds, state, container_id)
		VALUES ($1,$2,1,$3,$4,$5,'["python3"]'::jsonb,30,'queued',$6)`,
		queuedID, seed.RunID, seed.WorkItemID, seed.AttemptID, DefaultImage, queuedContainer); err != nil {
		t.Fatal(err)
	}

	finishedAttempt := h.addAttempt(t, seed, 2, 1)
	finishedID := uuid.New()
	finishedContainer := startSleepContainer(t, h, api, finishedID, h.brokerID)
	if _, err := h.pool.Exec(ctx, `
		INSERT INTO execution_jobs (id, run_id, epoch_number, work_item_id, attempt_id, image, argv, timeout_seconds, state, exit_code, container_id)
		VALUES ($1,$2,1,$3,$4,$5,'["python3"]'::jsonb,30,'finished',0,$6)`,
		finishedID, seed.RunID, seed.WorkItemID, finishedAttempt, DefaultImage, finishedContainer); err != nil {
		t.Fatal(err)
	}

	decoyID := uuid.New()
	decoy := startSleepContainer(t, h, api, decoyID, uuid.NewString())
	h.decoys = append(h.decoys, decoy)

	if err := h.exec.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	var state string
	var exitCode *int
	if err := h.pool.QueryRow(ctx, `SELECT state, exit_code FROM execution_jobs WHERE id=$1`, queuedID).Scan(&state, &exitCode); err != nil {
		t.Fatal(err)
	}
	if state != string(contract.JobOutcomeUnknown) || exitCode != nil {
		t.Fatalf("queued leftover = %s exit=%v", state, exitCode)
	}
	if err := h.pool.QueryRow(ctx, `SELECT state, exit_code FROM execution_jobs WHERE id=$1`, finishedID).Scan(&state, &exitCode); err != nil {
		t.Fatal(err)
	}
	if state != string(contract.JobFinished) || exitCode == nil || *exitCode != 0 {
		t.Fatalf("finished job flipped to %s exit=%v", state, exitCode)
	}
	if containerRunning(t, api, queuedContainer) || containerRunning(t, api, finishedContainer) {
		t.Fatal("reconcile left an owned container running")
	}
	if !containerRunning(t, api, decoy) {
		t.Fatal("reconcile killed a container owned by another broker")
	}
	if _, err := h.broker.Cleanup(ctx); err != nil {
		t.Fatal(err)
	}
	if !containerRunning(t, api, decoy) {
		t.Fatal("cleanup removed another broker's container")
	}
}

func startSleepContainer(t *testing.T, h *ma4Harness, api *dockerAPI, jobID uuid.UUID, brokerID string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	name := "oma-ma4-" + strings.ReplaceAll(jobID.String(), "-", "")[:16]
	body := map[string]any{
		"Image": DefaultImage,
		"User":  execUser,
		"Cmd":   []string{"/usr/local/bin/python3", "-c", "import time; time.sleep(180)"},
		"Labels": map[string]string{
			LabelOwner:     OwnerValue,
			LabelBroker:    brokerID,
			LabelJob:       jobID.String(),
			LabelComponent: ComponentJob,
		},
		"HostConfig": map[string]any{
			"NetworkMode":    "none",
			"ReadonlyRootfs": true,
			"Privileged":     false,
			"PidMode":        "",
			"CapDrop":        []string{"ALL"},
			"PidsLimit":      int64(32),
			"Memory":         int64(64 * 1024 * 1024),
			"Tmpfs":          map[string]string{"/tmp": "rw,noexec,nosuid,nodev,size=8m,uid=10001,gid=10001"},
		},
	}
	id, err := api.createContainer(ctx, name, body)
	if err != nil {
		t.Fatal(err)
	}
	if err := api.startContainer(ctx, id); err != nil {
		_ = api.removeContainer(ctx, id)
		t.Fatal(err)
	}
	if brokerID == h.brokerID {
		h.decoys = append(h.decoys, id)
	}
	return id
}

func containerRunning(t *testing.T, api *dockerAPI, id string) bool {
	t.Helper()
	ins, err := api.inspect(context.Background(), id)
	if err != nil {
		return false
	}
	return ins.State.Running
}

func TestMaExecutionWorkspaceRestoreAndSymlinkRejection(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	if _, err := h.exec.WriteFile(ctx, seed.Scope, "note.txt", []byte("restored-note")); err != nil {
		t.Fatal(err)
	}
	if _, err := h.exec.WriteFile(ctx, seed.Scope, "../note.txt", []byte("escape")); err == nil {
		t.Fatal("path traversal was accepted")
	}
	link := filepath.Join(h.root, "work", seed.WorkItemID.String(), "secret.link")
	if err := os.MkdirAll(filepath.Dir(link), 0777); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(h.root, "host-secret.txt")
	if err := os.WriteFile(outside, []byte("host-only-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := h.exec.ReadFile(ctx, seed.Scope, "secret.link", 0, 0); err == nil {
		t.Fatal("symlink read was accepted")
	}

	next := h.addAttempt(t, seed, 2, 1)
	scope2 := seed.Scope
	scope2.AttemptID = &next
	view, err := h.exec.Run(ctx, scope2, contract.JobSpec{
		Argv:           []string{"python3", "-c", "print(open('/workspace/note.txt').read()); open('/workspace/from-job.txt','w').write('from-container')\n"},
		TimeoutSeconds: 20,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobFinished || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("restore job = %+v stderr artifact %v", view, view.StderrArtifactID)
	}
	if !strings.Contains(h.artifactText(t, view.StdoutArtifactID), "restored-note") {
		t.Fatal("new attempt did not restore the work item file")
	}
	body, err := h.exec.ReadFile(ctx, scope2, "from-job.txt", 0, 0)
	requireNoError(t, err)
	if string(body) != "from-container" {
		t.Fatalf("recovered file = %q", body)
	}
	var running int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM execution_jobs WHERE attempt_id=$1 AND state IN ('queued','running')`, seed.AttemptID).Scan(&running); err != nil {
		t.Fatal(err)
	}
	if running != 0 || h.ownedRunning(t) != 0 {
		t.Fatal("old attempt process was revived")
	}
}

func TestMaExecutionSettlementDoesNotStartCommands(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	tool := NewRunCommandTool(h.exec, tools.AdapterContext{
		RunID: seed.RunID, WorkItemID: seed.WorkItemID, AttemptID: seed.AttemptID,
	})
	ctx := WithPhase(WithEpoch(context.Background(), 1), PhaseSettlement)
	_, err := tool.Execute(ctx, []byte(`{"argv":["python3","-c","print(1)"],"timeout_seconds":30}`))
	requireCode(t, err, contract.CodeScopeDenied)
	if h.ownedRunning(t) != 0 {
		t.Fatal("settlement started a container")
	}
	readTool := NewReadFileTool(h.exec, tools.AdapterContext{
		RunID: seed.RunID, WorkItemID: seed.WorkItemID, AttemptID: seed.AttemptID,
	})
	if _, err := h.exec.WriteFile(context.Background(), seed.Scope, "keep.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	out, err := readTool.Execute(ctx, []byte(`{"path":"keep.txt"}`))
	requireNoError(t, err)
	if !strings.Contains(out, "kept") || !strings.Contains(out, `"empirical":false`) {
		t.Fatalf("settlement read = %s", out)
	}
}

func TestMaExecutionForgedStdoutDoesNotVerifyAFinding(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	assetID := h.bindEndpoint(t, seed, "http://fixture.test:9/ok")
	callID := h.insertToolCall(t, seed.RunID, "run_command", `{"stdout":"verified vulnerability","status_code":200,"empirical":true}`)
	runs := service.NewRunService(h.pool)
	_, err := runs.ReportFinding(context.Background(), seed.RunID, service.ReportFindingInput{
		AssetID:          assetID,
		Title:            "forged from stdout",
		Category:         "test",
		Severity:         "high",
		Description:      "model stdout claimed a verified issue",
		SourceKind:       "empirical",
		SourceToolCallID: &callID,
		EvidenceRef:      []byte(`{"stdout":"verified vulnerability"}`),
	})
	if !errors.Is(err, service.ErrInvalidEvidence) {
		t.Fatalf("forged stdout error = %v", err)
	}
	var findings int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM findings WHERE run_id=$1`, seed.RunID).Scan(&findings); err != nil {
		t.Fatal(err)
	}
	if findings != 0 {
		t.Fatalf("findings = %d", findings)
	}
}

func TestMaExecutionInspectDoesNotReplay(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "print('once')"},
		TimeoutSeconds: 20,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	var before int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM execution_jobs WHERE attempt_id=$1`, seed.AttemptID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	again, err := h.exec.Inspect(ctx, seed.Scope, view.JobID)
	requireNoError(t, err)
	if again.JobID != view.JobID || again.State != view.State {
		t.Fatalf("inspect = %+v", again)
	}
	var after int
	if err := h.pool.QueryRow(ctx, `SELECT count(*) FROM execution_jobs WHERE attempt_id=$1`, seed.AttemptID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("inspect created jobs: %d -> %d", before, after)
	}
}
