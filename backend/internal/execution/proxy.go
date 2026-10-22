package execution

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Proxy is the only egress path from an execution container. It dials the
// capture callback and never dials the target.
type Proxy struct {
	JobID        uuid.UUID
	CaptureURL   string
	Token        string
	Origins      []string
	CA           *interceptCA
	Listen       string
	DialContext  func(ctx context.Context, network, addr string) (net.Conn, error)
	callbackAddr string
}

func (p *Proxy) client() (*http.Client, error) {
	parsed, err := url.Parse(p.CaptureURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, invalidInput("capture callback URL is not allowed")
	}
	addr := parsed.Host
	if !strings.Contains(addr, ":") {
		if parsed.Scheme == "https" {
			addr += ":443"
		} else {
			addr += ":80"
		}
	}
	p.callbackAddr = addr
	dial := p.DialContext
	if dial == nil {
		dial = (&net.Dialer{Timeout: 10 * time.Second}).DialContext
	}
	wrapped := func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != p.callbackAddr {
			return nil, errors.New("egress proxy refused a non-callback dial")
		}
		return dial(ctx, network, address)
	}
	return &http.Client{Transport: &http.Transport{DialContext: wrapped}, Timeout: 60 * time.Second}, nil
}

func (p *Proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodConnect {
		p.handleConnect(w, r)
		return
	}
	p.handleHTTP(w, r)
}

func (p *Proxy) handleHTTP(w http.ResponseWriter, r *http.Request) {
	target := r.URL
	if target == nil || !target.IsAbs() {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	if !originAllowed(p.Origins, target) || p.loops(target) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	status, hdr, body, err := p.exchange(r.Context(), r.Method, target.String(), r.Header, r.Body)
	if err != nil {
		http.Error(w, "capture unavailable", http.StatusBadGateway)
		return
	}
	for k, v := range hdr {
		w.Header().Set(k, v)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

func (p *Proxy) handleConnect(w http.ResponseWriter, r *http.Request) {
	host, port, err := net.SplitHostPort(r.Host)
	if err != nil {
		host = r.Host
		port = "443"
	}
	target := &url.URL{Scheme: "https", Host: net.JoinHostPort(host, port)}
	if port == "443" {
		target.Host = host
	}
	if !originAllowed(p.Origins, target) || p.loops(target) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "capture unavailable", http.StatusBadGateway)
		return
	}
	conn, _, err := hj.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	if _, err = conn.Write([]byte("HTTP/1.1 200 Connection Established\r\n\r\n")); err != nil {
		return
	}
	leafHost := host
	cert, err := p.CA.leaf(leafHost)
	if err != nil {
		return
	}
	tlsConn := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	if err = tlsConn.Handshake(); err != nil {
		return
	}
	defer tlsConn.Close()
	req, err := http.ReadRequest(bufio.NewReader(tlsConn))
	if err != nil {
		return
	}
	defer req.Body.Close()
	// Keep the CONNECT target. An inner Host header must not retarget the exchange.
	full := &url.URL{Scheme: "https", Host: target.Host, Path: req.URL.Path, RawQuery: req.URL.RawQuery}
	status, hdr, body, err := p.exchange(r.Context(), req.Method, full.String(), req.Header, req.Body)
	if err != nil {
		_, _ = tlsConn.Write([]byte("HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\n\r\n"))
		return
	}
	resp := &http.Response{
		StatusCode:    status,
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        make(http.Header),
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
	}
	for k, v := range hdr {
		resp.Header.Set(k, v)
	}
	_ = resp.Write(tlsConn)
}

func (p *Proxy) loops(target *url.URL) bool {
	callback, err := url.Parse(p.CaptureURL)
	if err != nil {
		return true
	}
	return strings.EqualFold(target.Host, callback.Host) || strings.Contains(strings.ToLower(target.Host), "docker.sock")
}

func (p *Proxy) exchange(ctx context.Context, method, rawURL string, hdr http.Header, body io.Reader) (int, map[string]string, []byte, error) {
	client, err := p.client()
	if err != nil {
		return 0, nil, nil, err
	}
	payload, err := io.ReadAll(io.LimitReader(body, 1<<20+1))
	if err != nil {
		return 0, nil, nil, err
	}
	if len(payload) > 1<<20 {
		return 0, nil, nil, invalidInput("capture request body is too large")
	}
	reqBody := captureExchange{
		Protocol: ProtocolVersion,
		JobID:    p.JobID,
		Method:   method,
		URL:      rawURL,
		Headers:  flattenHeaders(hdr),
		BodyB64:  base64.StdEncoding.EncodeToString(payload),
	}
	raw, err := json.Marshal(reqBody)
	if err != nil {
		return 0, nil, nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.CaptureURL, bytes.NewReader(raw))
	if err != nil {
		return 0, nil, nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+p.Token)
	res, err := client.Do(httpReq)
	if err != nil {
		return 0, nil, nil, err
	}
	defer res.Body.Close()
	rawRes, err := io.ReadAll(io.LimitReader(res.Body, 2<<20))
	if err != nil {
		return 0, nil, nil, err
	}
	if res.StatusCode >= 300 {
		return 0, nil, nil, unavailable("capture callback rejected the exchange")
	}
	var out captureExchangeResult
	if err := json.Unmarshal(rawRes, &out); err != nil {
		return 0, nil, nil, err
	}
	decoded, err := base64.StdEncoding.DecodeString(out.BodyB64)
	if out.BodyB64 == "" {
		decoded = nil
		err = nil
	}
	if err != nil {
		return 0, nil, nil, err
	}
	if out.StatusCode == 0 {
		out.StatusCode = http.StatusBadGateway
	}
	return out.StatusCode, out.Headers, decoded, nil
}

func flattenHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for key, values := range h {
		switch strings.ToLower(key) {
		case "proxy-authorization", "proxy-connection", "connection", "transfer-encoding", "keep-alive", "upgrade":
			continue
		}
		if len(values) == 0 {
			continue
		}
		out[key] = values[0]
	}
	return out
}

func originAllowed(origins []string, target *url.URL) bool {
	if target == nil || target.Host == "" || (target.Scheme != "http" && target.Scheme != "https") {
		return false
	}
	got := originKey(target)
	for _, item := range origins {
		parsed, err := url.Parse(item)
		if err != nil {
			continue
		}
		if originKey(parsed) == got {
			return true
		}
	}
	return false
}

func originKey(u *url.URL) string {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		if u.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return strings.ToLower(u.Scheme) + "://" + strings.ToLower(host) + ":" + port
}

// ServeProxyFromEnv is the container entrypoint. It does not open a Docker socket.
func ServeProxyFromEnv() error {
	jobID, err := uuid.Parse(os.Getenv("OMA_JOB_ID"))
	if err != nil {
		return invalidInput("egress proxy job id is missing")
	}
	ca, err := loadInterceptCA(mustRead(os.Getenv("OMA_CA_CERT")), mustRead(os.Getenv("OMA_CA_KEY")))
	if err != nil {
		return err
	}
	origins := strings.Split(os.Getenv("OMA_ALLOWED_ORIGINS"), ",")
	cleaned := origins[:0]
	for _, item := range origins {
		item = strings.TrimSpace(item)
		if item != "" {
			cleaned = append(cleaned, item)
		}
	}
	p := &Proxy{
		JobID:      jobID,
		CaptureURL: os.Getenv("OMA_CAPTURE_URL"),
		Token:      os.Getenv("OMA_CAPTURE_TOKEN"),
		Origins:    cleaned,
		CA:         ca,
		Listen:     os.Getenv("OMA_LISTEN"),
	}
	if p.Listen == "" {
		p.Listen = proxyListen
	}
	ln, err := net.Listen("tcp", p.Listen)
	if err != nil {
		return err
	}
	// The broker treats this line as the ready signal. It does not dial the job network.
	os.Stdout.WriteString("listening\n")
	server := &http.Server{Handler: p, ReadHeaderTimeout: 10 * time.Second}
	return server.Serve(ln)
}

func mustRead(path string) []byte {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}
