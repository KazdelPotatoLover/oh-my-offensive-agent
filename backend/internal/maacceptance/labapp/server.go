// Package labapp is a local catalog used by acceptance tests.
// Paths and markers are chosen per instance. It records real requests and
// does not insert success rows into the workbench database.
package labapp

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Identity is one lab login. Tokens stay on this server.
type Identity struct {
	Username string
	Password string
	Token    string
	Role     string
}

// Material is synthetic catalog data. A count mismatch is the harmless issue.
type Material struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	ListedCount int    `json:"listed_count"`
	BinCount    int    `json:"bin_count"`
	Note        string `json:"note"`
	Owner       string `json:"owner"`
}

// RequestLog is one request the app actually received.
type RequestLog struct {
	At       time.Time `json:"at"`
	Method   string    `json:"method"`
	Path     string    `json:"path"`
	Proto    string    `json:"proto"`
	TLS      bool      `json:"tls"`
	Status   int       `json:"status"`
	Identity string    `json:"identity"`
	Remote   string    `json:"remote"`
}

// Server serves the catalog on HTTP and HTTPS, plus a separate admin listener.
type Server struct {
	marker   string
	httpLn   net.Listener
	tlsLn    net.Listener
	adminLn  net.Listener
	caPEM    []byte
	caKey    any
	ident    map[string]*Identity
	byToken  map[string]*Identity
	byCookie map[string]*Identity
	material map[string][]Material

	mu       sync.Mutex
	sessions map[string]*Identity
	logs     []RequestLog
	patched  bool
	servers  []*http.Server
	held     bool
	release  chan struct{}
	inFlight int
	peak     int
}

// New starts HTTP, HTTPS, and admin listeners on 127.0.0.1:0.
func New() (*Server, error) {
	marker := "m-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
	clerk := &Identity{Username: "dana", Password: "clerk-pass", Token: "tok-" + uuid.NewString(), Role: "clerk"}
	auditor := &Identity{Username: "ravi", Password: "auditor-pass", Token: "tok-" + uuid.NewString(), Role: "auditor"}
	s := &Server{
		marker:   marker,
		ident:    map[string]*Identity{clerk.Username: clerk, auditor.Username: auditor},
		byToken:  map[string]*Identity{clerk.Token: clerk, auditor.Token: auditor},
		byCookie: map[string]*Identity{},
		sessions: map[string]*Identity{},
		material: map[string][]Material{
			clerk.Username: {{
				ID: "sku-clerk-" + marker, Title: "Synthetic label stock",
				ListedCount: 4, BinCount: 4, Note: "counts agree", Owner: clerk.Username,
			}},
			auditor.Username: {{
				ID: "sku-audit-" + marker, Title: "Synthetic bin card",
				ListedCount: 12, BinCount: 9,
				Note: "synthetic label mismatch for local verification", Owner: auditor.Username,
			}},
		},
	}
	var err error
	if s.httpLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		return nil, err
	}
	if s.tlsLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		_ = s.httpLn.Close()
		return nil, err
	}
	if s.adminLn, err = net.Listen("tcp", "127.0.0.1:0"); err != nil {
		_ = s.httpLn.Close()
		_ = s.tlsLn.Close()
		return nil, err
	}
	cert, err := s.certificate()
	if err != nil {
		s.closeListeners()
		return nil, err
	}
	app := http.HandlerFunc(s.handleApp)
	s.serve(s.httpLn, app, nil)
	s.serve(s.tlsLn, app, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
	s.serve(s.adminLn, http.HandlerFunc(s.handleAdmin), nil)
	return s, nil
}

func (s *Server) serve(ln net.Listener, h http.Handler, tlsCfg *tls.Config) {
	srv := &http.Server{Handler: h, TLSConfig: tlsCfg, ReadHeaderTimeout: 5 * time.Second}
	s.servers = append(s.servers, srv)
	go func() {
		if tlsCfg != nil {
			_ = srv.Serve(tls.NewListener(ln, tlsCfg))
			return
		}
		_ = srv.Serve(ln)
	}()
}

// Marker is the instance path prefix. Production code must not hardcode it.
func (s *Server) Marker() string { return s.marker }

// HTTPBase is the cleartext origin.
func (s *Server) HTTPBase() string { return "http://" + s.httpLn.Addr().String() }

// HTTPSBase is the TLS origin.
func (s *Server) HTTPSBase() string { return "https://" + s.tlsLn.Addr().String() }

// AdminBase is not on the catalog origin.
func (s *Server) AdminBase() string { return "http://" + s.adminLn.Addr().String() }

// CAPEM is the fixture CA. It is not a production trust anchor.
func (s *Server) CAPEM() []byte { return append([]byte(nil), s.caPEM...) }

// Identity returns a copy of a seeded login. The token is for the test harness.
func (s *Server) Identity(username string) (Identity, bool) {
	id, ok := s.ident[username]
	if !ok {
		return Identity{}, false
	}
	return *id, true
}

// EntryPath is the discovery page for this instance.
func (s *Server) EntryPath() string { return "/" + s.marker + "/" }

// ScriptPath returns a harmless comparison script for the signed-in identity.
func (s *Server) ScriptPath() string { return "/" + s.marker + "/script" }

// VerifyPath reports the current synthetic mismatch for the signed-in identity.
func (s *Server) VerifyPath() string { return "/" + s.marker + "/verify" }

// RedirectPath sends one same-origin hop. Tests use it for per-hop checks.
func (s *Server) RedirectPath() string { return "/" + s.marker + "/redirect" }

// HoldRequests blocks catalog handlers after they are counted in flight.
func (s *Server) HoldRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.held {
		return
	}
	s.held = true
	s.release = make(chan struct{})
}

// ReleaseRequests unblocks catalog handlers.
func (s *Server) ReleaseRequests() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.held {
		return
	}
	s.held = false
	close(s.release)
}

// PeakInFlight is the highest number of catalog requests inside the handler together.
func (s *Server) PeakInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.peak
}

// WaitInFlight waits until n catalog requests are inside the handler.
func (s *Server) WaitInFlight(n int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		current := s.inFlight
		s.mu.Unlock()
		if current >= n {
			return true
		}
		time.Sleep(15 * time.Millisecond)
	}
	return s.PeakInFlight() >= n && s.inFlightNow() >= n
}

func (s *Server) inFlightNow() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inFlight
}

func (s *Server) beginRequest(ctx context.Context) func() {
	s.mu.Lock()
	s.inFlight++
	if s.inFlight > s.peak {
		s.peak = s.inFlight
	}
	held := s.held
	release := s.release
	s.mu.Unlock()
	if held && release != nil {
		select {
		case <-release:
		case <-ctx.Done():
		}
	}
	return func() {
		s.mu.Lock()
		s.inFlight--
		s.mu.Unlock()
	}
}

// Close shuts down every listener.
func (s *Server) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var first error
	for _, srv := range s.servers {
		if err := srv.Shutdown(ctx); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (s *Server) closeListeners() {
	_ = s.httpLn.Close()
	_ = s.tlsLn.Close()
	_ = s.adminLn.Close()
}

// Requests returns the catalog request log. Admin calls are not included.
func (s *Server) Requests() []RequestLog {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]RequestLog, len(s.logs))
	copy(out, s.logs)
	return out
}

// Patch aligns the synthetic mismatch. A later catalog read must observe it.
func (s *Server) Patch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.patched = true
	for user, items := range s.material {
		for i := range items {
			items[i].ListedCount = items[i].BinCount
			items[i].Note = "synthetic label aligned"
		}
		s.material[user] = items
	}
}

func (s *Server) handleAdmin(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/requests":
		_ = json.NewEncoder(w).Encode(s.Requests())
	case "/patch":
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		s.Patch()
		_, _ = w.Write([]byte(`{"patched":true}`))
	case "/health":
		_, _ = w.Write([]byte("ok"))
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) handleApp(w http.ResponseWriter, r *http.Request) {
	done := s.beginRequest(r.Context())
	defer done()
	status := http.StatusOK
	identity := ""
	defer func() {
		s.mu.Lock()
		s.logs = append(s.logs, RequestLog{
			At: time.Now(), Method: r.Method, Path: r.URL.Path, Proto: r.Proto,
			TLS: r.TLS != nil, Status: status, Identity: identity, Remote: r.RemoteAddr,
		})
		s.mu.Unlock()
	}()
	prefix := "/" + s.marker
	if r.URL.Path != prefix && !strings.HasPrefix(r.URL.Path, prefix+"/") {
		status = http.StatusNotFound
		http.NotFound(w, r)
		return
	}
	rel := strings.TrimPrefix(r.URL.Path, prefix)
	if rel == "" {
		rel = "/"
	}
	user := s.authenticate(r)
	if user != nil {
		identity = user.Username
	}
	switch {
	case rel == "/" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><h1>Lab catalog %s</h1><a href=\"/%s/login\">Sign in</a> <a href=\"/%s/catalog\">Catalog</a> <a href=\"/%s/script\">Script</a> <a href=\"/%s/verify\">Verify</a></body></html>", s.marker, s.marker, s.marker, s.marker, s.marker)
	case rel == "/redirect" && r.Method == http.MethodGet:
		status = http.StatusFound
		http.Redirect(w, r, "/"+s.marker+"/materials", http.StatusFound)
		return
	case rel == "/login" && r.Method == http.MethodGet:
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<form method=\"post\" action=\"/%s/login\"><input name=\"username\"><input name=\"password\" type=\"password\"></form>", s.marker)
	case rel == "/login" && r.Method == http.MethodPost:
		username, password := s.readLogin(r)
		id := s.ident[username]
		if id == nil || id.Password != password {
			status = http.StatusUnauthorized
			http.Error(w, "unauthorized", status)
			return
		}
		cookie := "sess-" + uuid.NewString()
		s.mu.Lock()
		s.sessions[cookie] = id
		s.mu.Unlock()
		http.SetCookie(w, &http.Cookie{Name: "lab_session", Value: cookie, Path: "/", HttpOnly: true})
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"username": id.Username, "role": id.Role, "token": id.Token})
	case rel == "/catalog" && r.Method == http.MethodGet:
		if user == nil {
			status = http.StatusUnauthorized
			http.Error(w, "unauthorized", status)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, "<html><body><a href=\"/%s/materials\">Materials</a></body></html>", s.marker)
	case rel == "/materials" && r.Method == http.MethodGet:
		if user == nil {
			status = http.StatusUnauthorized
			http.Error(w, "unauthorized", status)
			return
		}
		s.writeMaterials(w, user.Username)
	case rel == "/script" && r.Method == http.MethodGet:
		if user == nil {
			status = http.StatusUnauthorized
			http.Error(w, "unauthorized", status)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "#!/bin/sh\n# local synthetic comparison for %s\nprintf '%%s listed-vs-bin\\n' %q\n", user.Username, s.marker)
	case rel == "/verify" && r.Method == http.MethodGet:
		if user == nil {
			status = http.StatusUnauthorized
			http.Error(w, "unauthorized", status)
			return
		}
		mismatch := false
		s.mu.Lock()
		for _, item := range s.material[user.Username] {
			if item.ListedCount != item.BinCount {
				mismatch = true
			}
		}
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"marker": s.marker, "owner": user.Username, "mismatch": mismatch})
	case strings.HasPrefix(rel, "/materials/") && r.Method == http.MethodGet:
		if user == nil {
			status = http.StatusUnauthorized
			http.Error(w, "unauthorized", status)
			return
		}
		id := strings.TrimPrefix(rel, "/materials/")
		item, ok := s.materialFor(user.Username, id)
		if !ok {
			status = http.StatusNotFound
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(item)
	default:
		status = http.StatusNotFound
		http.NotFound(w, r)
	}
}

func (s *Server) readLogin(r *http.Request) (string, string) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		var body struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		_ = json.Unmarshal(raw, &body)
		return body.Username, body.Password
	}
	values, _ := url.ParseQuery(string(raw))
	return values.Get("username"), values.Get("password")
}

func (s *Server) authenticate(r *http.Request) *Identity {
	if h := r.Header.Get("Authorization"); strings.HasPrefix(h, "Bearer ") {
		if id := s.byToken[strings.TrimPrefix(h, "Bearer ")]; id != nil {
			return id
		}
	}
	c, err := r.Cookie("lab_session")
	if err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[c.Value]
}

func (s *Server) writeMaterials(w http.ResponseWriter, username string) {
	s.mu.Lock()
	items := append([]Material(nil), s.material[username]...)
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"marker": s.marker, "materials": items})
}

func (s *Server) materialFor(username, id string) (Material, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range s.material[username] {
		if item.ID == id {
			return item, true
		}
	}
	return Material{}, false
}

func (s *Server) certificate() (tls.Certificate, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "ma7-lab-fixture-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	s.caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	s.caKey = caKey
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return tls.Certificate{}, err
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &leafKey.PublicKey, caKey)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{
		Certificate: [][]byte{leafDER, caDER},
		PrivateKey:  leafKey,
	}, nil
}
