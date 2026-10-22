package execution

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

func TestMaExecutionOfflinePython(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	secret := "host-secret-" + uuid.NewString()
	if err := os.WriteFile(filepath.Join(h.root, ".env"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	other := filepath.Join(h.root, "work", uuid.NewString())
	if err := os.MkdirAll(other, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, "other.txt"), []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", pythonProbe(secret)},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobFinished || view.ExitCode == nil || *view.ExitCode != 0 {
		t.Fatalf("job = %+v", view)
	}
	stdout := h.artifactText(t, view.StdoutArtifactID)
	if !strings.Contains(stdout, "py-ok") {
		t.Fatalf("stdout = %q", stdout)
	}
	if strings.Contains(stdout, secret) {
		t.Fatal("container output contains a host secret")
	}
	if strings.Contains(stdout, "docker.sock") && strings.Contains(stdout, "sock-visible") {
		t.Fatal("container can see the docker socket")
	}
	report := h.exec.SecurityReport(view.JobID)
	if report == nil {
		t.Fatal("security report was not captured")
	}
	if report.Privileged || report.PidMode != "" || !report.ReadonlyRootfs || report.User != "10001:10001" {
		t.Fatalf("isolation report = %+v", report)
	}
	if report.NetworkMode != "none" {
		t.Fatalf("offline network = %q", report.NetworkMode)
	}
	if len(report.ExtraHosts) != 0 {
		t.Fatalf("extra hosts = %v", report.ExtraHosts)
	}
	for _, bind := range report.Binds {
		if strings.Contains(bind, "docker.sock") || strings.Contains(bind, ".env") {
			t.Fatalf("forbidden bind %s", bind)
		}
	}
	if h.ownedRunning(t) != 0 {
		t.Fatal("execution container still running after a finished job")
	}
}

func pythonProbe(secret string) string {
	return `
import os, pathlib
print("py-ok")
root = pathlib.Path("/")
names = [p.name for p in root.iterdir()]
print("root", ",".join(sorted(names)))
sock = pathlib.Path("/var/run/docker.sock")
print("sock-visible" if sock.exists() else "sock-absent")
for candidate in ["/workspace/../.env", "/.env", "/workspace/.env", "/proc/1/environ"]:
    try:
        text = pathlib.Path(candidate).read_text(errors="replace")
    except Exception:
        continue
    if "` + secret + `" in text:
        print("leaked", candidate)
print("probe-done")
`
}

func TestMaExecutionNonZeroExitIsAResult(t *testing.T) {
	h := newMa4Harness(t, Config{})
	seed := h.seed(t)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"python3", "-c", "import sys; print('ran'); sys.exit(3)"},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobFinished || view.ExitCode == nil || *view.ExitCode != 3 {
		t.Fatalf("job = %+v", view)
	}
	if !strings.Contains(h.artifactText(t, view.StdoutArtifactID), "ran") {
		t.Fatal("stdout artifact missing")
	}
	var runStatus string
	if err := h.pool.QueryRow(ctx, `SELECT status FROM runs WHERE id=$1`, seed.RunID).Scan(&runStatus); err != nil {
		t.Fatal(err)
	}
	if runStatus != "running" {
		t.Fatalf("run status became %s", runStatus)
	}
}
