package netcap

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// Gateway is the production capture proxy. A model-supplied run id is ignored.
// Only a server-issued capability bound to Job, Run, and Epoch may use it.
type Gateway struct {
	transport *Transport
	store     *Store
	authority *Authority
	intercept bool
	ln        net.Listener
	srv       *http.Server
	mu        sync.Mutex
}

func NewGateway(transport *Transport, authority *Authority) *Gateway {
	intercept := authority != nil
	return &Gateway{transport: transport, store: transport.store, authority: authority, intercept: intercept}
}

// Listen starts the proxy on addr. Use 127.0.0.1:0 in tests.
func (g *Gateway) Listen(addr string) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	g.ln = ln
	g.srv = &http.Server{Handler: g, ReadHeaderTimeout: 10 * time.Second}
	go g.srv.Serve(ln)
	return nil
}

func (g *Gateway) Close() error {
	if g.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = g.srv.Shutdown(ctx)
	}
	if g.ln != nil {
		return g.ln.Close()
	}
	return nil
}

func (g *Gateway) ProxyURL() string {
	if g == nil || g.ln == nil {
		return ""
	}
	return "http://" + g.ln.Addr().String()
}

// CACertPEM is the public intercept certificate. The private key stays here.
func (g *Gateway) CACertPEM() []byte {
	if g == nil || g.authority == nil {
		return nil
	}
	return g.authority.CertPEM()
}

// IssueJobCapability returns a bearer token whose hash is stored with the job scope.
func (g *Gateway) IssueJobCapability(ctx context.Context, scope contract.ExecutionScope, ttl time.Duration) (string, error) {
	if err := scope.Validate(); err != nil {
		return "", err
	}
	if scope.JobID == nil || *scope.JobID == uuid.Nil {
		return "", contractError(contract.CodeInvalidInput, "job capability requires job_id")
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}
	if ttl > 2*time.Hour {
		ttl = 2 * time.Hour
	}
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	token := hex.EncodeToString(raw)
	sum := sha256.Sum256([]byte(token))
	if err := g.store.SaveCapability(ctx, hex.EncodeToString(sum[:]), scope, time.Now().Add(ttl)); err != nil {
		return "", err
	}
	return token, nil
}

// AllowContainerDial rejects every dial except the proxy listen address.
// Setting HTTP_PROXY is not a substitute for this check.
func (g *Gateway) AllowContainerDial(addr string) error {
	if g == nil || g.ln == nil {
		return contractError(contract.CodeExecutionUnavailable, "capture proxy is not listening")
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return contractError(contract.CodeScopeDenied, "direct egress denied")
	}
	proxyHost, proxyPort, err := net.SplitHostPort(g.ln.Addr().String())
	if err != nil || port != proxyPort {
		return contractError(contract.CodeScopeDenied, "direct egress denied")
	}
	if host == proxyHost {
		return nil
	}
	if proxyHost == "127.0.0.1" && (host == "127.0.0.1" || host == "localhost") {
		return nil
	}
	return contractError(contract.CodeScopeDenied, "direct egress denied")
}

func (g *Gateway) Exchange(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*contract.HTTPReceipt, error) {
	if g == nil || g.transport == nil {
		return nil, contract.ErrNotImplemented
	}
	return g.transport.Do(ctx, scope, in)
}

func (g *Gateway) Lookup(ctx context.Context, scope contract.ExecutionScope, receiptID uuid.UUID) (*contract.HTTPReceipt, error) {
	if g == nil || g.store == nil {
		return nil, contract.ErrNotImplemented
	}
	return g.store.LookupScoped(ctx, scope, receiptID)
}

// Revoke stops in-flight exchanges and revokes job capabilities for the scope.
func (g *Gateway) Revoke(ctx context.Context, req contract.RevokeRequest) (*contract.RevokeReport, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	report := &contract.RevokeReport{Kind: req.Kind}
	if req.Kind != contract.RevokeHTTPExchange && req.Kind != contract.RevokeJob && req.Kind != contract.RevokeAttempt {
		return report, nil
	}
	if g.transport != nil {
		report.Cancelled = g.transport.cancels.cancelScope(req.Scope, req.JobID)
	}
	if g.store != nil && (req.Kind == contract.RevokeJob || req.Kind == contract.RevokeHTTPExchange || req.Kind == contract.RevokeAttempt) {
		if _, err := g.store.RevokeCapabilities(ctx, req.Scope, req.JobID); err != nil {
			return report, err
		}
	}
	return report, nil
}

func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Client-supplied run identifiers are not authorization.
	r.Header.Del("X-Run-Id")
	r.Header.Del("X-Epoch")
	r.Header.Del("X-Job-Id")
	scope, err := g.authorize(r.Context(), proxyToken(r))
	if err != nil {
		w.Header().Set("Proxy-Authenticate", "Bearer")
		http.Error(w, "proxy capability required", http.StatusProxyAuthRequired)
		return
	}
	if r.Method == http.MethodConnect {
		g.handleConnect(w, r, scope)
		return
	}
	g.forwardHTTP(w, r, scope)
}

func (g *Gateway) authorize(ctx context.Context, token string) (contract.ExecutionScope, error) {
	if token == "" {
		return contract.ExecutionScope{}, contractError(contract.CodeUnauthorized, "proxy capability is required")
	}
	sum := sha256.Sum256([]byte(token))
	scope, expires, revoked, err := g.store.LoadCapability(ctx, hex.EncodeToString(sum[:]))
	if err != nil {
		return contract.ExecutionScope{}, err
	}
	if revoked != nil || time.Now().After(expires) {
		return contract.ExecutionScope{}, contractError(contract.CodeUnauthorized, "proxy capability is expired or revoked")
	}
	current, err := g.store.CurrentEpoch(ctx, scope.RunID)
	if err != nil {
		return contract.ExecutionScope{}, err
	}
	if current != scope.EpochNumber {
		return contract.ExecutionScope{}, contractError(contract.CodeStaleEpoch, "proxy capability epoch is stale")
	}
	return scope, nil
}

func proxyToken(r *http.Request) string {
	value := r.Header.Get("Proxy-Authorization")
	lower := strings.ToLower(value)
	if strings.HasPrefix(lower, "bearer ") {
		return strings.TrimSpace(value[len("bearer "):])
	}
	if user, pass, ok := r.BasicAuth(); ok {
		if strings.EqualFold(user, "bearer") {
			return pass
		}
		if pass == "" {
			return user
		}
	}
	return ""
}

func (g *Gateway) forwardHTTP(w http.ResponseWriter, r *http.Request, scope contract.ExecutionScope) {
	if r.URL == nil || r.URL.Host == "" || (r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		http.Error(w, "proxy request requires an absolute http url", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, hardMaxBody+1))
	if err != nil {
		http.Error(w, "read proxy body", http.StatusBadRequest)
		return
	}
	if len(body) > hardMaxBody {
		http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
		return
	}
	in := contract.HTTPRequestInput{URL: r.URL.String(), Method: r.Method, BodyText: string(body)}
	if r.Header.Get("Content-Type") == "application/x-www-form-urlencoded" && len(body) > 0 {
		values, err := url.ParseQuery(string(body))
		if err == nil {
			in.BodyText = ""
			in.Form = map[string]string{}
			for key := range values {
				in.Form[key] = values.Get(key)
			}
		}
	}
	for key, values := range r.Header {
		if hopHeader(key) || isSensitiveHeader(key) {
			continue
		}
		if len(values) > 0 {
			if in.Headers == nil {
				in.Headers = map[string]string{}
			}
			in.Headers[key] = values[0]
		}
	}
	receipt, err := g.transport.Do(r.Context(), scope, in)
	if err != nil || receipt == nil || receipt.ResponseArtifactID == nil {
		http.Error(w, "upstream exchange failed", http.StatusBadGateway)
		return
	}
	view, err := g.store.ReadArtifact(r.Context(), scope, *receipt.ResponseArtifactID, true)
	if err != nil {
		http.Error(w, "capture artifact unavailable", http.StatusBadGateway)
		return
	}
	for key, values := range view.Headers {
		if hopHeader(key) || isSensitiveHeader(key) {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Capture-Receipt", receipt.ReceiptID.String())
	status := receipt.StatusCode
	if status < 100 || status > 599 {
		status = http.StatusBadGateway
	}
	w.WriteHeader(status)
	_, _ = w.Write(view.Body)
}

func (g *Gateway) handleConnect(w http.ResponseWriter, r *http.Request, scope contract.ExecutionScope) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
		port = "443"
	}
	policy, err := g.store.LoadPolicy(r.Context(), scope.RunID)
	if err != nil {
		http.Error(w, "policy unavailable", http.StatusBadGateway)
		return
	}
	probe := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port), Path: "/"}
	rule, err := policy.Match(probe)
	if err != nil {
		http.Error(w, "origin outside scope", http.StatusForbidden)
		return
	}
	if !g.intercept && rule.PathMode != "same_origin" && rule.PathMode != "same_origin_html" {
		http.Error(w, "opaque CONNECT cannot prove an exact path grant", http.StatusForbidden)
		return
	}
	hijacker, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unavailable", http.StatusInternalServerError)
		return
	}
	client, _, err := hijacker.Hijack()
	if err != nil {
		return
	}
	defer client.Close()
	if !g.intercept {
		g.tunnel(client, host, port, scope, policy)
		return
	}
	if _, err := client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	leaf, err := g.authority.Leaf(host)
	if err != nil {
		return
	}
	tlsConn := tls.Server(client, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()
	reader := bufio.NewReader(tlsConn)
	req, err := http.ReadRequest(reader)
	if err != nil {
		return
	}
	defer req.Body.Close()
	target := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port), Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	if _, err := policy.Match(target); err != nil {
		_, _ = tlsConn.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	body, _ := io.ReadAll(io.LimitReader(req.Body, hardMaxBody+1))
	in := contract.HTTPRequestInput{URL: target.String(), Method: req.Method, BodyText: string(body)}
	// Hijack keeps the handler context alive only until this function returns.
	// The upstream exchange must not inherit a cancel that fires when the
	// proxy request is finished, and it must not use a model-supplied run id.
	captureCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 30*time.Second)
	defer cancel()
	receipt, err := g.transport.DoCaptured(captureCtx, scope, in, contract.CaptureTLSIntercept)
	if err != nil || receipt == nil {
		msg := "capture failed"
		if err != nil {
			msg = err.Error()
		}
		_, _ = tlsConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n" + msg))
		return
	}
	g.writeCaptured(tlsConn, scope, receipt)
}

func (g *Gateway) writeCaptured(conn net.Conn, scope contract.ExecutionScope, receipt *contract.HTTPReceipt) {
	if receipt.ResponseArtifactID == nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	view, err := g.store.ReadArtifact(context.Background(), scope, *receipt.ResponseArtifactID, true)
	if err != nil {
		_, _ = conn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	status := receipt.StatusCode
	if status < 100 || status > 599 {
		status = http.StatusBadGateway
	}
	var b strings.Builder
	b.WriteString("HTTP/1.1 ")
	b.WriteString(http.StatusText(status))
	if http.StatusText(status) == "" {
		b.WriteString("OK")
	}
	b.WriteString("\r\n")
	// Status line needs the numeric code. Rebuild simply.
	_ = b
	resp := &http.Response{StatusCode: status, Status: strconvStatus(status), Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(string(view.Body))), ContentLength: int64(len(view.Body))}
	for key, values := range view.Headers {
		if hopHeader(key) {
			continue
		}
		for _, value := range values {
			resp.Header.Add(key, value)
		}
	}
	resp.Header.Set("X-Capture-Receipt", receipt.ReceiptID.String())
	_ = resp.Write(conn)
}

func (g *Gateway) tunnel(client net.Conn, host, port string, scope contract.ExecutionScope, policy Policy) {
	_, _ = client.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n"))
	upstream, err := net.DialTimeout("tcp", net.JoinHostPort(host, port), 10*time.Second)
	mode := contract.CaptureConnectMeta
	outcome := "completed"
	if err != nil {
		outcome = "connection_error"
	}
	ex := savedExchange{
		Scope: scope, Method: "CONNECT", URL: "https://" + net.JoinHostPort(host, port),
		FinalURL: "https://" + net.JoinHostPort(host, port), Mode: mode, Outcome: outcome,
		RequestHash: sha256Hex([]byte(host + ":" + port)), RequestType: "application/connect",
	}
	_, _ = g.store.SaveExchange(context.Background(), ex)
	if err != nil {
		return
	}
	defer upstream.Close()
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
	_ = policy
}

func hopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "proxy-connection", "keep-alive", "proxy-authenticate", "proxy-authorization", "te", "trailer", "transfer-encoding", "upgrade":
		return true
	default:
		return false
	}
}

func strconvStatus(code int) string {
	text := http.StatusText(code)
	if text == "" {
		text = "Status"
	}
	return strings.TrimSpace(strings.Join([]string{itoa(code), text}, " "))
}
