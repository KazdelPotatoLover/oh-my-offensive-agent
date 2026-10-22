package execution

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

func TestClampCommandTimeoutDoesNotUseFiveSecondsAsTheCap(t *testing.T) {
	got, err := ClampCommandTimeout(0, 0, 0)
	if err != nil || got != contract.DefaultCommandTimeout {
		t.Fatalf("default = %s, %v", got, err)
	}
	got, err = ClampCommandTimeout(10*time.Minute, 0, 0)
	if err != nil || got != contract.MaxCommandTimeout {
		t.Fatalf("max = %s, %v", got, err)
	}
	got, err = ClampCommandTimeout(time.Minute, 12*time.Second, 8*time.Second)
	if err != nil || got != 8*time.Second {
		t.Fatalf("attempt remainder = %s, %v", got, err)
	}
	got, err = ClampCommandTimeout(time.Minute, contract.LegacyImplicitToolTimeout, 0)
	if err != nil || got != contract.LegacyImplicitToolTimeout {
		t.Fatalf("legacy remainder must be honored, got %s, %v", got, err)
	}
	if _, err := ClampCommandTimeout(time.Minute, 0, 0); err != nil {
		t.Fatal(err)
	}
	got, err = ClampCommandTimeout(time.Minute, time.Millisecond, 0)
	if err != nil || got != time.Millisecond {
		t.Fatalf("sub-second remainder = %s, %v", got, err)
	}
}

func TestWorkspaceRejectsTraversalAndSymlinks(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.env")
	if err := os.WriteFile(outside, []byte("host-secret"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := SafeRelative("../secret.env"); err == nil {
		t.Fatal("parent traversal was accepted")
	}
	if _, err := SafeRelative("/etc/passwd"); err == nil {
		t.Fatal("absolute path was accepted")
	}
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := WriteWithin(root, "link", []byte("overwrite")); err == nil {
		t.Fatal("write through symlink was accepted")
	}
	body, err := os.ReadFile(outside)
	if err != nil || string(body) != "host-secret" {
		t.Fatalf("outside file changed: %q %v", body, err)
	}
	if err := os.Mkdir(filepath.Join(root, "sub"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "sub", "link")); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadWithin(root, "sub/link", 0, 0); err == nil {
		t.Fatal("read through symlink was accepted")
	}
	if err := WriteWithin(root, "ok.txt", []byte("kept")); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWithin(root, "ok.txt", 0, 0)
	if err != nil || string(got) != "kept" {
		t.Fatalf("read = %q, %v", got, err)
	}
}

func TestContainerCreateBodyIsFixed(t *testing.T) {
	body, err := containerCreateBody(containerCreate{
		Image:         DefaultImage,
		Argv:          []string{"python3", "-c", "print(1)"},
		Network:       "oma-net-test",
		WorkspaceHost: "/tmp/oma-work",
		CACertHost:    "/tmp/oma-ca.pem",
		ProxyURL:      "http://10.0.0.2:8080",
		Memory:        256 * 1024 * 1024,
		NanoCPUs:      500_000_000,
		Pids:          64,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(body)
	text := string(raw)
	for _, forbidden := range []string{"docker.sock", "Privileged\":true", "\"host\"", "PidMode\":\"host\""} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("create body contains %s: %s", forbidden, text)
		}
	}
	host := body["HostConfig"].(map[string]any)
	if host["Privileged"] != false || host["ReadonlyRootfs"] != true || host["NetworkMode"] != "oma-net-test" {
		t.Fatalf("host config = %#v", host)
	}
	if body["User"] != "10001:10001" {
		t.Fatalf("user = %#v", body["User"])
	}
	if _, err := containerCreateBody(containerCreate{Image: DefaultImage, Argv: []string{"bash"}, Network: "none", WorkspaceHost: "/tmp/oma-work"}); err == nil {
		t.Fatal("bash was accepted")
	}
	if _, err := containerCreateBody(containerCreate{Image: DefaultImage, Argv: []string{"python3"}, Network: "host", WorkspaceHost: "/tmp/w", CACertHost: "/tmp/ca"}); err == nil {
		t.Fatal("host network was accepted")
	}
}

func TestProtocolRejectsDockerFields(t *testing.T) {
	raw := []byte(`{"protocol":"execution.broker.v1","job_id":"` + uuid.NewString() + `","HostConfig":{"Privileged":true}}`)
	var req RunRequest
	if err := decodeStrict(raw, &req); err == nil {
		t.Fatal("HostConfig was accepted")
	}
	if ownedLabels(map[string]string{LabelOwner: OwnerValue, LabelBroker: "other"}, "mine") {
		t.Fatal("foreign broker id matched")
	}
	if ownedLabels(map[string]string{LabelOwner: OwnerValue}, "") {
		t.Fatal("empty broker id matched")
	}
}

func TestProxyDialsOnlyTheCallback(t *testing.T) {
	var dialed []string
	proxy := &Proxy{
		JobID:      uuid.New(),
		CaptureURL: "http://127.0.0.1:9/v1/capture",
		Token:      "token",
		Origins:    []string{"http://127.0.0.1:18080"},
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			dialed = append(dialed, addr)
			return nil, context.Canceled
		},
	}
	req := httptest.NewRequest(http.MethodGet, "http://evil.example/secret", nil)
	rec := httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d", rec.Code)
	}
	if len(dialed) != 0 {
		t.Fatalf("disallowed origin dialed %#v", dialed)
	}
	req = httptest.NewRequest(http.MethodGet, "http://127.0.0.1:18080/hello", nil)
	rec = httptest.NewRecorder()
	proxy.ServeHTTP(rec, req)
	if len(dialed) != 1 || dialed[0] != "127.0.0.1:9" {
		t.Fatalf("dialed %#v", dialed)
	}
}

func TestSettlementDoesNotStartCommands(t *testing.T) {
	fake := NewFake()
	tool := NewRunCommandTool(fake, tools.AdapterContext{RunID: uuid.New(), WorkItemID: uuid.New(), AttemptID: uuid.New()})
	ctx := WithPhase(WithEpoch(context.Background(), 1), PhaseSettlement)
	if _, err := tool.Execute(ctx, []byte(`{"argv":["python3","-c","print(1)"]}`)); err == nil {
		t.Fatal("settlement run_command was accepted")
	}
	if len(fake.Calls) != 0 {
		t.Fatalf("broker calls = %#v", fake.Calls)
	}
	write := NewWriteFileTool(fake, tools.AdapterContext{RunID: uuid.New(), WorkItemID: uuid.New(), AttemptID: uuid.New()})
	if _, err := write.Execute(ctx, []byte(`{"path":"a.txt","content":"x"}`)); err == nil {
		t.Fatal("settlement write_file was accepted")
	}
	inspect := NewInspectCommandTool(fake, tools.AdapterContext{RunID: uuid.New(), WorkItemID: uuid.New(), AttemptID: uuid.New()})
	id := uuid.New()
	fake.Views[id] = &contract.JobView{JobID: id, State: contract.JobKilled}
	out, err := inspect.Execute(ctx, []byte(`{"job_id":"`+id.String()+`"}`))
	if err != nil || !strings.Contains(out, `"empirical":false`) {
		t.Fatalf("inspect = %s, %v", out, err)
	}
}

func TestRejectModelMintedReceipt(t *testing.T) {
	if err := contract.RejectModelMintedReceipt([]byte(`{"receipt_id":"66666666-6666-4666-8666-666666666666"}`)); err == nil {
		t.Fatal("model minted receipt was accepted")
	}
}
