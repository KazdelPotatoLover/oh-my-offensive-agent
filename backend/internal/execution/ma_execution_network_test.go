package execution

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/service"
)

type localFixture struct {
	Origin  string
	URL     string
	Client  *http.Client
	Direct  *atomic.Int32
	closeFn func()
}

func (f *localFixture) Close() { f.closeFn() }

func startLocalFixture(t *testing.T, https bool, body string) *localFixture {
	t.Helper()
	host := "fixture.test"
	direct := &atomic.Int32{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Oma-Capture") == "" {
			direct.Add(1)
		}
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	})
	var ln net.Listener
	var err error
	var client *http.Client
	if https {
		ca, caErr := newInterceptCA()
		if caErr != nil {
			t.Fatal(caErr)
		}
		leaf, leafErr := ca.leaf(host)
		if leafErr != nil {
			t.Fatal(leafErr)
		}
		ln, err = tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{leaf}})
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(ca.certPEM)
		client = &http.Client{Transport: fixtureTransport(host, &tls.Config{RootCAs: pool, ServerName: host})}
	} else {
		ln, err = net.Listen("tcp", "127.0.0.1:0")
		client = &http.Client{Transport: fixtureTransport(host, nil)}
	}
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(ln)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	scheme := "http"
	if https {
		scheme = "https"
	}
	origin := fmt.Sprintf("%s://%s:%s", scheme, host, port)
	fx := &localFixture{
		Origin: origin, URL: origin + "/ok", Client: client, Direct: direct,
		closeFn: func() { _ = server.Close() },
	}
	t.Cleanup(fx.Close)
	return fx
}

func captureClient(base *http.Client) *http.Client {
	return &http.Client{Transport: headerRoundTripper{base: base.Transport}, Timeout: 20 * time.Second}
}

type headerRoundTripper struct {
	base http.RoundTripper
}

func (h headerRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	clone := r.Clone(r.Context())
	clone.Header.Set("X-Oma-Capture", "1")
	return h.base.RoundTrip(clone)
}

func fixtureTransport(host string, tlsConfig *tls.Config) *http.Transport {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	return &http.Transport{
		TLSClientConfig: tlsConfig,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if strings.HasPrefix(addr, host+":") {
				_, port, err := net.SplitHostPort(addr)
				if err != nil {
					return nil, err
				}
				addr = net.JoinHostPort("127.0.0.1", port)
			}
			return dialer.DialContext(ctx, network, addr)
		},
	}
}

func TestMaExecutionCurlUsesCaptureAndRejectsDirectEgress(t *testing.T) {
	h := newMa4Harness(t, Config{})
	fx := startLocalFixture(t, false, "fixture-body-http")
	other := startLocalFixture(t, false, "other-body")
	seed := h.seed(t)
	assetID := h.bindEndpoint(t, seed, fx.URL)
	gw := NewStandInGateway([]string{fx.Origin}, captureClient(fx.Client))
	h.exec.gateway = gw

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"curl", "-fsS", "--max-time", "20", fx.URL},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobFinished || view.ExitCode == nil || *view.ExitCode != 0 {
		stderr := ""
		if view != nil && view.StderrArtifactID != nil {
			stderr = h.artifactText(t, view.StderrArtifactID)
		}
		t.Fatalf("curl job = %+v stderr=%s", view, stderr)
	}
	stdout := h.artifactText(t, view.StdoutArtifactID)
	if !strings.Contains(stdout, "fixture-body-http") {
		t.Fatalf("curl stdout = %q", stdout)
	}
	if len(view.TrafficRefs) == 0 || view.ExecutionReceiptID == nil {
		t.Fatal("curl result has no trusted receipt")
	}
	receipt, err := gw.Lookup(ctx, seed.Scope, *view.ExecutionReceiptID)
	requireNoError(t, err)
	if receipt == nil || !receipt.EmpiricalSource() || receipt.CaptureMode != contract.CaptureDirect {
		t.Fatalf("receipt = %+v", receipt)
	}
	report := h.exec.SecurityReport(view.JobID)
	if report == nil || report.NetworkMode == "none" || report.NetworkMode == "host" || len(report.ExtraHosts) != 0 {
		t.Fatalf("network report = %+v", report)
	}
	for _, bind := range report.Binds {
		if strings.Contains(bind, "docker.sock") {
			t.Fatalf("socket bind %s", bind)
		}
	}

	denied, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"curl", "-fsS", "--max-time", "15", other.URL},
		TimeoutSeconds: 20,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if denied.ExitCode == nil || *denied.ExitCode == 0 || len(denied.TrafficRefs) != 0 {
		t.Fatalf("out-of-scope curl = %+v", denied)
	}

	direct, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"curl", "-fsS", "--noproxy", "*", "--max-time", "8", fx.URL},
		TimeoutSeconds: 15,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if direct.ExitCode == nil || *direct.ExitCode == 0 {
		t.Fatal("direct egress curl succeeded")
	}
	if fx.Direct.Load() != 0 || other.Direct.Load() != 0 {
		t.Fatalf("fixture saw direct requests: allowed=%d other=%d", fx.Direct.Load(), other.Direct.Load())
	}

	callID := h.insertToolCall(t, seed.RunID, "run_command", stdout)
	_, err = service.NewRunService(h.pool).ReportFinding(ctx, seed.RunID, service.ReportFindingInput{
		AssetID:          assetID,
		Title:            "stdout is not a receipt",
		Category:         "test",
		Severity:         "medium",
		Description:      stdout,
		SourceKind:       "empirical",
		SourceToolCallID: &callID,
	})
	if !errors.Is(err, service.ErrInvalidEvidence) {
		t.Fatalf("stdout impersonated empirical evidence: %v", err)
	}
	if h.ownedRunning(t) != 0 {
		t.Fatal("curl job left a container")
	}
}

func TestMaExecutionCurlHTTPSReceipt(t *testing.T) {
	h := newMa4Harness(t, Config{})
	fx := startLocalFixture(t, true, "fixture-body-https")
	seed := h.seed(t)
	h.bindEndpoint(t, seed, fx.URL)
	gw := NewStandInGateway([]string{fx.Origin}, captureClient(fx.Client))
	h.exec.gateway = gw
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	view, err := h.exec.Run(ctx, seed.Scope, contract.JobSpec{
		Argv:           []string{"curl", "-fsS", "--max-time", "20", fx.URL},
		TimeoutSeconds: 30,
		WorkItemID:     seed.WorkItemID,
	})
	requireNoError(t, err)
	if view.State != contract.JobFinished || view.ExitCode == nil || *view.ExitCode != 0 {
		stderr := ""
		if view != nil && view.StderrArtifactID != nil {
			stderr = h.artifactText(t, view.StderrArtifactID)
		}
		t.Fatalf("https curl = %+v stderr=%s", view, stderr)
	}
	if !strings.Contains(h.artifactText(t, view.StdoutArtifactID), "fixture-body-https") {
		t.Fatal("https body was not returned through the proxy")
	}
	if view.ExecutionReceiptID == nil {
		t.Fatal("https curl has no receipt")
	}
	receipt, err := gw.Lookup(ctx, seed.Scope, *view.ExecutionReceiptID)
	requireNoError(t, err)
	if receipt.CaptureMode != contract.CaptureTLSIntercept || !receipt.EmpiricalSource() {
		t.Fatalf("https receipt = %+v", receipt)
	}
	if fx.Direct.Load() != 0 {
		t.Fatal("https fixture accepted a direct container connection")
	}
}
