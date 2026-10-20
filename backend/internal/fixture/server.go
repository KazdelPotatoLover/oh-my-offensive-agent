package fixture

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Server is a lightweight HTTP server used for local fixture endpoints.
type Server struct {
	server          *http.Server
	listener        net.Listener
	addr            string
	version         atomic.Pointer[string]
	enableLoginLink atomic.Bool
	totalReqs       atomic.Int64
	countsByPath    sync.Map // map[string]*atomic.Int64
}

// NewServer creates a new local fixture server bound to 127.0.0.1 with the specified port (e.g. "29090").
// If port is "0", a free port will be chosen by the listener.
func NewServer(port string) (*Server, error) {
	if port == "" {
		port = "29090"
	}
	bindAddr := fmt.Sprintf("127.0.0.1:%s", port)
	ln, err := net.Listen("tcp", bindAddr)
	if err != nil {
		return nil, fmt.Errorf("failed to bind fixture listener on %s: %w", bindAddr, err)
	}

	s := &Server{
		listener: ln,
		addr:     ln.Addr().String(),
	}

	initialVersion := "1.0.0"
	s.version.Store(&initialVersion)
	s.enableLoginLink.Store(true)

	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/login", s.handleLogin)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/control/version", s.handleControlVersion)
	mux.HandleFunc("/control/links", s.handleControlLinks)
	mux.HandleFunc("/control/stats", s.handleControlStats)

	// Test endpoints for edge case verification
	mux.HandleFunc("/slow", s.handleSlow)
	mux.HandleFunc("/large", s.handleLarge)
	mux.HandleFunc("/500", s.handle500)
	mux.HandleFunc("/redirect", s.handleRedirect)
	mux.HandleFunc("/target", s.handleTarget)

	s.server = &http.Server{
		Handler:      mux,
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 15 * time.Second,
	}

	return s, nil
}

// Start begins serving requests in a background goroutine.
func (s *Server) Start() {
	go func() {
		_ = s.server.Serve(s.listener)
	}()
}

// Close gracefully stops the fixture server.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return s.server.Shutdown(ctx)
}

// Addr returns the listening host:port address.
func (s *Server) Addr() string {
	return s.addr
}

// Port returns just the port number.
func (s *Server) Port() string {
	_, port, _ := net.SplitHostPort(s.addr)
	return port
}

// BaseURL returns the HTTP base URL, e.g. "http://127.0.0.1:29090".
func (s *Server) BaseURL() string {
	return fmt.Sprintf("http://127.0.0.1:%s", s.Port())
}

// SetVersion dynamically sets the version returned by /health.
func (s *Server) SetVersion(v string) {
	s.version.Store(&v)
}

// GetVersion returns the current health version.
func (s *Server) GetVersion() string {
	v := s.version.Load()
	if v == nil {
		return "1.0.0"
	}
	return *v
}

// SetEnableLoginLink configures whether root page includes a link to /login.
func (s *Server) SetEnableLoginLink(enabled bool) {
	s.enableLoginLink.Store(enabled)
}

// IsEnableLoginLink returns whether the root page includes a link to /login.
func (s *Server) IsEnableLoginLink() bool {
	return s.enableLoginLink.Load()
}

// RequestCount returns total requests handled.
func (s *Server) RequestCount() int64 {
	return s.totalReqs.Load()
}

// PathCount returns total requests for a specific path.
func (s *Server) PathCount(path string) int64 {
	if val, ok := s.countsByPath.Load(path); ok {
		return val.(*atomic.Int64).Load()
	}
	return 0
}

// ResetCounts resets all request counters.
func (s *Server) ResetCounts() {
	s.totalReqs.Store(0)
	s.countsByPath.Range(func(key, value any) bool {
		value.(*atomic.Int64).Store(0)
		return true
	})
}

func (s *Server) recordRequest(path string) {
	s.totalReqs.Add(1)
	val, _ := s.countsByPath.LoadOrStore(path, &atomic.Int64{})
	val.(*atomic.Int64).Add(1)
}

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	s.recordRequest("/")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Fixture-App", "local-fixture-v1")
	w.WriteHeader(http.StatusOK)

	var sb strings.Builder
	sb.WriteString(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Local Fixture Home</title>
</head>
<body>
    <h1>Welcome to Local Fixture App</h1>
    <p>A deterministic test application for agent workbench HTTP tools.</p>
    <nav>
        <ul>
            <li><a href="/">Home (Loop)</a></li>
`)
	if s.IsEnableLoginLink() {
		sb.WriteString(`            <li><a href="/login">Account Login</a></li>
            <li><a href="/login">Duplicate Login Link</a></li>
`)
	}
	sb.WriteString(`            <li><a href="/outside-scope">Outside Scope Page</a></li>
            <li><a href="http://127.0.0.1:9999/other">Other Port Target</a></li>
            <li><a href="https://example.com/external">External Site</a></li>
            <li><a href="mailto:admin@local.test">Admin Email</a></li>
            <li><a href="javascript:void(0)">Noop JS</a></li>
            <li><a href="/login?ref=home">Login with Query</a></li>
            <li><a href="#content">Anchor Fragment</a></li>
        </ul>
    </nav>
</body>
</html>`)
	_, _ = w.Write([]byte(sb.String()))
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/login" {
		http.NotFound(w, r)
		return
	}
	s.recordRequest("/login")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Fixture-App", "local-fixture-v1")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`<!DOCTYPE html>
<html>
<head>
    <meta charset="utf-8">
    <title>Local Fixture Login</title>
</head>
<body>
    <h1>Account Login</h1>
    <form method="POST" action="/login">
        <label for="username">Username:</label>
        <input type="text" id="username" name="username" />
        <label for="password">Password:</label>
        <input type="password" id="password" name="password" />
        <button type="submit">Sign In</button>
    </form>
    <div class="nav-links">
        <p><a href="/health">System Health Status</a></p>
        <p><a href="/">Back to Home</a></p>
    </div>
</body>
</html>`))
}

func (s *Server) handleControlLinks(w http.ResponseWriter, r *http.Request) {
	enableStr := r.URL.Query().Get("enable_login")
	if enableStr == "" && r.Body != nil {
		var req struct {
			EnableLogin *bool `json:"enable_login"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.EnableLogin != nil {
			s.SetEnableLoginLink(*req.EnableLogin)
		}
	} else if enableStr != "" {
		s.SetEnableLoginLink(enableStr == "true" || enableStr == "1")
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"enable_login": s.IsEnableLoginLink(),
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/health" {
		http.NotFound(w, r)
		return
	}
	s.recordRequest("/health")
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Fixture-App", "local-fixture-v1")
	w.WriteHeader(http.StatusOK)
	data, _ := json.Marshal(map[string]interface{}{
		"status":  "UP",
		"version": s.GetVersion(),
	})
	_, _ = w.Write(data)
}

func (s *Server) handleControlVersion(w http.ResponseWriter, r *http.Request) {
	newV := r.URL.Query().Get("v")
	if newV == "" {
		newV = r.URL.Query().Get("version")
	}
	if newV == "" && r.Body != nil {
		var req struct {
			Version string `json:"version"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err == nil && req.Version != "" {
			newV = req.Version
		}
	}
	if newV != "" {
		s.SetVersion(newV)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"version": s.GetVersion(),
	})
}

func (s *Server) handleControlStats(w http.ResponseWriter, r *http.Request) {
	stats := map[string]int64{}
	s.countsByPath.Range(func(key, value any) bool {
		stats[key.(string)] = value.(*atomic.Int64).Load()
		return true
	})
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"total_requests": s.RequestCount(),
		"counts_by_path": stats,
		"version":        s.GetVersion(),
	})
}

func (s *Server) handleSlow(w http.ResponseWriter, r *http.Request) {
	s.recordRequest("/slow")
	// Sleep for 6 seconds to trigger the 5-second HTTP tool timeout
	select {
	case <-r.Context().Done():
		return
	case <-time.After(6 * time.Second):
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"status":"delayed"}`))
}

func (s *Server) handleLarge(w http.ResponseWriter, r *http.Request) {
	s.recordRequest("/large")
	w.Header().Set("Content-Type", "text/plain")
	w.WriteHeader(http.StatusOK)
	// Return 70 KiB of text to test 64 KiB LimitReader truncation detection
	chunk := strings.Repeat("ABCDEFGH12345678", 64) // 1024 bytes
	for i := 0; i < 70; i++ {
		_, _ = w.Write([]byte(chunk))
	}
}

func (s *Server) handle500(w http.ResponseWriter, r *http.Request) {
	s.recordRequest("/500")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write([]byte(`{"error":"internal_server_error","code":500}`))
}

func (s *Server) handleRedirect(w http.ResponseWriter, r *http.Request) {
	s.recordRequest("/redirect")
	// Returns 302 pointing to /target
	target := fmt.Sprintf("http://127.0.0.1:%s/target", s.Port())
	http.Redirect(w, r, target, http.StatusFound)
}

func (s *Server) handleTarget(w http.ResponseWriter, r *http.Request) {
	s.recordRequest("/target")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"message":"redirect_target_reached"}`))
}
