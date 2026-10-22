package netcap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/db"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func TestDestinationAllowedDoesNotTreatManagementPortsAsTargets(t *testing.T) {
	rule := OriginRule{Scheme: "http", Host: "127.0.0.1", Port: "25432", AllowPrivate: true}
	if err := DestinationAllowed("127.0.0.1", "25432", rule); err == nil {
		t.Fatal("management database port was treated as a target")
	}
	if err := DestinationAllowed("169.254.169.254", "80", OriginRule{AllowPrivate: true}); err == nil {
		t.Fatal("metadata address was treated as a target")
	}
	if err := DestinationAllowed("127.0.0.1", "8080", OriginRule{AllowPrivate: true}); err != nil {
		t.Fatalf("explicit private target was rejected: %v", err)
	}
}

func TestHistoricalSnapshotDoesNotGainHTTPSOrMethods(t *testing.T) {
	policy, err := PolicyFromExecutionInput([]byte(`{"discovery_policy":{"origin":"http://127.0.0.1:9","mode":"same_origin_html","allowed_methods":["GET","POST"]}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := policy.AllowsMethod("POST"); err == nil {
		t.Fatal("discovery snapshot authorized POST")
	}
	if _, err := policy.Match(&url.URL{Scheme: "https", Host: "127.0.0.1:9", Path: "/"}); err == nil {
		t.Fatal("discovery snapshot authorized https")
	}
}

func TestMemGatewayRefusesEmpiricalReceipts(t *testing.T) {
	var standIn MemGateway
	if _, err := standIn.Exchange(context.Background(), contract.ExecutionScope{}, contract.HTTPRequestInput{}); !errors.Is(err, contract.ErrNotImplemented) {
		t.Fatalf("stand-in exchanged: %v", err)
	}
	if err := RejectNonProduction(&standIn); err == nil {
		t.Fatal("stand-in was accepted as production")
	}
	if err := RejectNonProduction(NewTransport(nil)); err != nil {
		t.Fatalf("real transport rejected: %v", err)
	}
}

func TestMAHTTPControlledExchange(t *testing.T) {
	pool := ma3Pool(t)
	ctx := context.Background()
	hits := map[string]*atomic.Int32{}
	var mu sync.Mutex
	count := func(path string) {
		mu.Lock()
		defer mu.Unlock()
		if hits[path] == nil {
			hits[path] = &atomic.Int32{}
		}
		hits[path].Add(1)
	}
	get := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		if hits[path] == nil {
			return 0
		}
		return int(hits[path].Load())
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/login", func(w http.ResponseWriter, r *http.Request) {
		count("/login")
		_ = r.ParseForm()
		user := r.FormValue("username")
		http.SetCookie(w, &http.Cookie{Name: "sid", Value: user + "-token", Path: "/"})
		_, _ = w.Write([]byte(`{"ok":true}`))
	})
	mux.HandleFunc("/whoami", func(w http.ResponseWriter, r *http.Request) {
		count("/whoami")
		c, err := r.Cookie("sid")
		if err != nil {
			http.Error(w, "missing", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, c.Value)
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		count("/echo")
		body, _ := io.ReadAll(r.Body)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"method": r.Method,
			"type":   r.Header.Get("Content-Type"),
			"body":   string(body),
			"cookie": r.Header.Get("Cookie"),
		})
	})
	mux.HandleFunc("/slow", func(w http.ResponseWriter, r *http.Request) {
		count("/slow")
		select {
		case <-r.Context().Done():
			return
		case <-time.After(3 * time.Second):
			_, _ = w.Write([]byte("late"))
		}
	})
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		count("/health")
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/large", func(w http.ResponseWriter, r *http.Request) {
		count("/large")
		_, _ = w.Write(bytes.Repeat([]byte("x"), 128))
	})
	httpSrv := httptestServer(t, mux)
	httpPort := portOf(httpSrv)

	ca, err := NewAuthority("fixture-ca")
	if err != nil {
		t.Fatal(err)
	}
	tlsMux := http.NewServeMux()
	tlsMux.HandleFunc("/secret", func(w http.ResponseWriter, r *http.Request) {
		count("/secret")
		_, _ = w.Write([]byte("tls-body"))
	})
	tlsSrv := tlsFixture(t, ca, tlsMux)
	tlsPort := portOf(tlsSrv)
	other := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count("/landed")
		_, _ = io.WriteString(w, "cookie="+r.Header.Get("Cookie"))
	}))
	otherPort := portOf(other)
	var redirectCookie atomic.Value
	redirector := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count("/jump")
		redirectCookie.Store(r.Header.Get("Cookie"))
		http.Redirect(w, r, other+"/landed", http.StatusFound)
	}))
	redirectPort := portOf(redirector)

	policy := fmt.Sprintf(`{"network_policy":{"template":"active","allowed_methods":["GET","HEAD","POST"],"origins":[
		{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true},
		{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true},
		{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true},
		{"scheme":"https","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}
	],"max_pages":20,"max_requests":80,"timeout_seconds":5}}`, httpPort, otherPort, redirectPort, tlsPort)
	scope := seedRun(t, pool, policy)
	transport := NewTransport(NewStore(pool))
	origin := "https://127.0.0.1:" + tlsPort
	transport.TrustFixtureCA(ca.CertPEM(), []string{origin})
	transport.SetBeforeDial(func() {
		if transport.store.TransactionDepth() != 0 {
			t.Errorf("dialed while pace transaction depth=%d", transport.store.TransactionDepth())
		}
	})

	t.Run("tls and json and form", func(t *testing.T) {
		rec, err := transport.Do(ctx, scope, contract.HTTPRequestInput{URL: origin + "/secret", Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		if rec.CaptureMode != contract.CaptureDirect || !rec.EmpiricalSource() || rec.StatusCode != 200 || !strings.Contains(rec.ResponsePreview, "tls-body") {
			t.Fatalf("tls receipt = %+v", rec)
		}
		form, err := transport.Do(ctx, scope, contract.HTTPRequestInput{URL: httpSrv + "/echo", Method: "POST", Form: map[string]string{"item": "a"}})
		if err != nil {
			t.Fatal(err)
		}
		if form.StatusCode != 200 || !strings.Contains(form.ResponsePreview, "application/x-www-form-urlencoded") || !strings.Contains(form.ResponsePreview, "item=a") {
			t.Fatalf("form receipt preview %s", form.ResponsePreview)
		}
		raw, err := transport.Do(ctx, scope, contract.HTTPRequestInput{URL: httpSrv + "/echo", Method: "POST", BodyJSON: json.RawMessage(`{"n":1}`)})
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(raw.ResponsePreview, "application/json") || (!strings.Contains(raw.ResponsePreview, `"n":1`) && !strings.Contains(raw.ResponsePreview, `n\":1`)) {
			t.Fatalf("json preview %s", raw.ResponsePreview)
		}
	})

	t.Run("two identities do not share cookies", func(t *testing.T) {
		store := NewStore(pool)
		if err := store.PutCredential(ctx, Credential{Alias: "alice", TargetOrigin: httpOrigin(httpSrv), Kind: "login", Username: "alice", Secret: "p@ss-alice", LoginPath: "/login"}); err != nil {
			t.Fatal(err)
		}
		if err := store.PutCredential(ctx, Credential{Alias: "bob", TargetOrigin: httpOrigin(httpSrv), Kind: "login", Username: "bob", Secret: "p@ss-bob", LoginPath: "/login"}); err != nil {
			t.Fatal(err)
		}
		left := cloneScope(scope)
		right := cloneScope(scope)
		rightItem := uuid.New()
		insertWorkItem(t, pool, scope.RunID, rightItem, "direction-b")
		right.WorkItemID = &rightItem
		var wg sync.WaitGroup
		var leftBody, rightBody string
		var leftErr, rightErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			rec, err := transport.Do(ctx, left, contract.HTTPRequestInput{URL: httpSrv + "/whoami", Method: "GET", CredentialRef: "ref:alice", SessionRef: "ref:alice"})
			leftErr = err
			if rec != nil {
				leftBody = rec.ResponsePreview
			}
		}()
		go func() {
			defer wg.Done()
			rec, err := transport.Do(ctx, right, contract.HTTPRequestInput{URL: httpSrv + "/whoami", Method: "GET", CredentialRef: "ref:bob", SessionRef: "ref:bob"})
			rightErr = err
			if rec != nil {
				rightBody = rec.ResponsePreview
			}
		}()
		wg.Wait()
		if leftErr != nil || rightErr != nil {
			t.Fatalf("identities: %v %v", leftErr, rightErr)
		}
		if !strings.Contains(leftBody, "alice-token") || strings.Contains(leftBody, "bob-token") {
			t.Fatalf("alice saw %q", leftBody)
		}
		if !strings.Contains(rightBody, "bob-token") || strings.Contains(rightBody, "alice-token") {
			t.Fatalf("bob saw %q", rightBody)
		}
		var leaked int
		_ = pool.QueryRow(ctx, `SELECT count(*) FROM http_capture_artifacts WHERE encode(content,'escape') LIKE '%p@ss-alice%' OR encode(content,'escape') LIKE '%p@ss-bob%'`).Scan(&leaked)
		if leaked != 0 {
			t.Fatal("password stored in a capture artifact")
		}
		isolated := cloneScope(scope)
		other := uuid.New()
		insertWorkItem(t, pool, scope.RunID, other, "direction-c")
		isolated.WorkItemID = &other
		if _, err := transport.Do(ctx, isolated, contract.HTTPRequestInput{URL: httpSrv + "/whoami", Method: "GET", SessionRef: "ref:alice"}); err == nil {
			t.Fatal("isolated direction reused another session")
		}
	})

	t.Run("cross origin redirect strips auth", func(t *testing.T) {
		store := NewStore(pool)
		if err := store.PutCredential(ctx, Credential{Alias: "redir-alice", TargetOrigin: httpOrigin(redirector), Kind: "cookie", Secret: "sid=alice-token"}); err != nil {
			t.Fatal(err)
		}
		rec, err := transport.Do(ctx, scope, contract.HTTPRequestInput{
			URL: redirector + "/jump", Method: "GET", Headers: map[string]string{"X-Trace": "1"},
			CredentialRef: "ref:redir-alice",
		})
		if err != nil {
			t.Fatal(err)
		}
		seen, _ := redirectCookie.Load().(string)
		if !strings.Contains(seen, "alice-token") {
			t.Fatalf("first hop did not receive the origin-bound cookie: %q", seen)
		}
		if rec.StatusCode != 200 || strings.Contains(rec.ResponsePreview, "alice-token") || strings.Contains(rec.ResponsePreview, "sid=") {
			t.Fatalf("auth survived redirect: %s", rec.ResponsePreview)
		}
	})

	t.Run("missing credential does not dial", func(t *testing.T) {
		before := get("/whoami")
		_, err := transport.Do(ctx, scope, contract.HTTPRequestInput{URL: httpSrv + "/whoami", Method: "GET", CredentialRef: "ref:missing"})
		var ce *contract.Error
		if !errors.As(err, &ce) || ce.Code != contract.CodeCredentialUnavailable {
			t.Fatalf("missing credential: %v", err)
		}
		if get("/whoami") != before {
			t.Fatal("missing credential still dialed")
		}
	})

	t.Run("certificate error is not empirical", func(t *testing.T) {
		bad, err := NewAuthority("untrusted")
		if err != nil {
			t.Fatal(err)
		}
		badSrv := tlsFixture(t, bad, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			count("/bad")
			_, _ = w.Write([]byte("nope"))
		}))
		badPolicy := fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"https","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}]}}`, portOf(badSrv))
		badScope := seedRun(t, pool, badPolicy)
		_, err = transport.Do(ctx, badScope, contract.HTTPRequestInput{URL: badSrv + "/bad", Method: "GET"})
		if err == nil {
			t.Fatal("untrusted certificate succeeded")
		}
		var outcome string
		scanErr := pool.QueryRow(ctx, `SELECT outcome FROM http_receipts WHERE run_id=$1 ORDER BY created_at DESC LIMIT 1`, badScope.RunID).Scan(&outcome)
		if scanErr != nil || outcome == "completed" {
			t.Fatalf("cert failure outcome %q err %v", outcome, scanErr)
		}
	})

	t.Run("cancel and truncation", func(t *testing.T) {
		cctx, cancel := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := transport.Do(cctx, scope, contract.HTTPRequestInput{URL: httpSrv + "/slow", Method: "GET"})
			done <- err
		}()
		time.Sleep(80 * time.Millisecond)
		cancel()
		if err := <-done; err == nil {
			t.Fatal("cancel returned success")
		}
		var outcome string
		if err := pool.QueryRow(ctx, `SELECT outcome FROM http_receipts WHERE run_id=$1 AND url LIKE '%/slow%' ORDER BY created_at DESC LIMIT 1`, scope.RunID).Scan(&outcome); err != nil {
			t.Fatal(err)
		}
		if outcome == "completed" {
			t.Fatalf("cancelled exchange stored as completed")
		}
		limitScope := seedRun(t, pool, fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}],"max_body_bytes":32}}`, httpPort))
		rec, err := NewTransport(NewStore(pool)).Do(ctx, limitScope, contract.HTTPRequestInput{URL: httpSrv + "/large", Method: "GET"})
		if err != nil {
			t.Fatal(err)
		}
		if !rec.Truncated {
			t.Fatal("over-limit body was not marked truncated")
		}
		_, _, _, err = VerifyToolReceipt(ctx, mustTx(t, pool), limitScope.RunID, uuid.New(), uuid.New(), 1, fmt.Sprintf(`{"receipt_id":"%s"}`, rec.ReceiptID))
		if err == nil {
			t.Fatal("truncated receipt was empirical")
		}
	})
}

func TestMAHTTPConcurrencyAndGateway(t *testing.T) {
	pool := ma3Pool(t)
	ctx := context.Background()
	var hits atomic.Int32
	var current atomic.Int32
	var maxCurrent atomic.Int32
	release := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		n := current.Add(1)
		for {
			old := maxCurrent.Load()
			if n <= old || maxCurrent.CompareAndSwap(old, n) {
				break
			}
		}
		if r.URL.Path == "/block" {
			<-release
		}
		current.Add(-1)
		_, _ = w.Write([]byte("page " + r.URL.Path))
	})
	srv := httptestServer(t, mux)
	port := portOf(srv)
	policy := fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET","HEAD"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}],"max_pages":2,"max_requests":8,"request_interval_ms":120}}`, port)
	scope := seedRun(t, pool, policy)
	transport := NewTransport(NewStore(pool))
	var dials []time.Time
	var dialMu sync.Mutex
	transport.SetBeforeDial(func() {
		if transport.store.TransactionDepth() != 0 {
			t.Errorf("network while transaction depth=%d", transport.store.TransactionDepth())
		}
		dialMu.Lock()
		dials = append(dials, time.Now())
		dialMu.Unlock()
	})
	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := transport.Do(ctx, scope, contract.HTTPRequestInput{URL: fmt.Sprintf("%s/p%d", srv, i), Method: "GET"})
			errCh <- err
		}(i)
	}
	wg.Wait()
	close(errCh)
	denied := 0
	for err := range errCh {
		if err != nil {
			denied++
		}
	}
	if denied < 2 {
		t.Fatalf("page cap denied %d of 4", denied)
	}
	dialMu.Lock()
	times := append([]time.Time(nil), dials...)
	dialMu.Unlock()
	if len(times) < 2 {
		t.Fatalf("expected paced dials, got %d", len(times))
	}
	for i := 1; i < len(times); i++ {
		if times[i].Sub(times[i-1]) < 80*time.Millisecond {
			t.Fatalf("origin pace gap %s", times[i].Sub(times[i-1]))
		}
	}

	blockScope := seedRun(t, pool, fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}],"max_pages":5,"max_requests":10}}`, port))
	blockTransport := NewTransport(NewStore(pool))
	before := hits.Load()
	start := make(chan struct{})
	var flight sync.WaitGroup
	flight.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer flight.Done()
			<-start
			_, _ = blockTransport.Do(ctx, blockScope, contract.HTTPRequestInput{URL: srv + "/block", Method: "GET"})
		}()
	}
	close(start)
	time.Sleep(150 * time.Millisecond)
	if got := hits.Load() - before; got != 1 {
		t.Fatalf("in-flight dedup hits=%d", got)
	}
	close(release)
	flight.Wait()
	_, _ = blockTransport.Do(ctx, blockScope, contract.HTTPRequestInput{URL: srv + "/block", Method: "GET"})
	if got := hits.Load() - before; got < 2 {
		t.Fatalf("completed GET was cached, hits=%d", got)
	}

	ca, err := NewAuthority("intercept")
	if err != nil {
		t.Fatal(err)
	}
	gw := NewGateway(transport, ca)
	if err := gw.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Close() })
	if err := gw.AllowContainerDial(srv[len("http://"):]); err == nil {
		t.Fatal("direct fixture dial was allowed")
	}
	if err := gw.AllowContainerDial(gw.ln.Addr().String()); err != nil {
		t.Fatalf("proxy dial denied: %v", err)
	}
	job := uuid.New()
	capScope := cloneScope(scope)
	capScope.JobID = &job
	token, err := gw.IssueJobCapability(ctx, capScope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := net.Dial("tcp", gw.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET %s/health HTTP/1.1\r\nHost: 127.0.0.1\r\nX-Run-Id: %s\r\n\r\n", srv, uuid.New())
	buf := make([]byte, 128)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, _ := conn.Read(buf)
	conn.Close()
	if !strings.Contains(string(buf[:n]), "407") {
		t.Fatalf("unauthenticated proxy body %q", buf[:n])
	}
	conn, err = net.Dial("tcp", gw.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(conn, "GET http://127.0.0.1:9/nope HTTP/1.1\r\nHost: 127.0.0.1:9\r\nProxy-Authorization: Bearer %s\r\nX-Run-Id: %s\r\n\r\n", token, uuid.New())
	n, _ = conn.Read(buf)
	conn.Close()
	if strings.Contains(string(buf[:n]), "200") && strings.Contains(string(buf[:n]), "page") {
		t.Fatalf("forged run id proxied an out-of-scope origin: %q", buf[:n])
	}

	secretCA, err := NewAuthority("upstream")
	if err != nil {
		t.Fatal(err)
	}
	secretSrv := tlsFixture(t, secretCA, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("captured-body"))
	}))
	secretPort := portOf(secretSrv)
	secretScope := seedRun(t, pool, fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"https","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}]}}`, secretPort))
	secretScope.JobID = &job
	transport.TrustFixtureCA(secretCA.CertPEM(), []string{"https://127.0.0.1:" + secretPort})
	secretToken, err := gw.IssueJobCapability(ctx, secretScope, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	body := proxyGET(t, gw, secretToken, secretSrv, "/secret")
	if !strings.Contains(body, "captured-body") {
		t.Fatalf("mitm body %q", body)
	}
	var mode, preview string
	if err := pool.QueryRow(ctx, `SELECT capture_mode, preview FROM http_receipts WHERE run_id=$1 AND capture_mode='tls_intercept' ORDER BY created_at DESC LIMIT 1`, secretScope.RunID).Scan(&mode, &preview); err != nil {
		t.Fatal(err)
	}
	if mode != contract.CaptureTLSIntercept || !strings.Contains(preview, "captured-body") {
		t.Fatalf("intercept receipt %s %s", mode, preview)
	}
}

func proxyGET(t *testing.T, gw *Gateway, token, base, path string) string {
	t.Helper()
	u, _ := url.Parse(base)
	conn, err := net.Dial("tcp", gw.ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "CONNECT %s HTTP/1.1\r\nHost: %s\r\nProxy-Authorization: Bearer %s\r\n\r\n", u.Host, u.Host, token)
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	status := readHTTPHead(t, conn)
	if !strings.Contains(status, "200") {
		t.Fatalf("connect failed: %q", status)
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(gw.CACertPEM())
	tlsConn := tls.Client(conn, &tls.Config{RootCAs: roots, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatal(err)
	}
	fmt.Fprintf(tlsConn, "GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", path, u.Host)
	raw, err := io.ReadAll(tlsConn)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func ma3Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	raw := os.Getenv("TEST_DATABASE_URL")
	u, err := url.Parse(raw)
	if err != nil || u.Path != "/agent_workbench_ma3" {
		t.Fatalf("refusing TEST_DATABASE_URL %q; tests truncate and require /agent_workbench_ma3", raw)
	}
	_, file, _, _ := runtime.Caller(0)
	if err := db.RunMigrations(raw, filepath.Join(filepath.Dir(file), "..", "..", "migrations")); err != nil {
		t.Fatal(err)
	}
	pool, err := db.Connect(context.Background(), raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(context.Background(), `TRUNCATE http_credentials, tasks, assets CASCADE`); err != nil {
		t.Fatal(err)
	}
	return pool
}

func seedRun(t *testing.T, pool *pgxpool.Pool, policy string) contract.ExecutionScope {
	t.Helper()
	ctx := context.Background()
	var taskID, runID, itemID, attemptID uuid.UUID
	if err := pool.QueryRow(ctx, `INSERT INTO tasks(name, objective) VALUES('ma3','http') RETURNING id`).Scan(&taskID); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO runs(task_id, status, execution_input) VALUES($1,'running',$2::jsonb) RETURNING id`, taskID, policy).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	itemID = uuid.New()
	insertWorkItem(t, pool, runID, itemID, "direction-a")
	if err := pool.QueryRow(ctx, `INSERT INTO attempts(work_item_id, attempt_number, status) VALUES($1,1,'running') RETURNING id`, itemID).Scan(&attemptID); err != nil {
		t.Fatal(err)
	}
	return contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RoleWorker, WorkItemID: &itemID, AttemptID: &attemptID}
}

func insertWorkItem(t *testing.T, pool *pgxpool.Pool, runID, id uuid.UUID, key string) {
	t.Helper()
	_, err := pool.Exec(context.Background(), `INSERT INTO work_items(id, run_id, key, title) VALUES($1,$2,$3,$3)`, id, runID, key)
	if err != nil {
		t.Fatal(err)
	}
}

func cloneScope(in contract.ExecutionScope) contract.ExecutionScope {
	out := in
	if in.WorkItemID != nil {
		id := *in.WorkItemID
		out.WorkItemID = &id
	}
	if in.AttemptID != nil {
		id := *in.AttemptID
		out.AttemptID = &id
	}
	return out
}

func httptestServer(t *testing.T, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	go srv.Serve(ln)
	t.Cleanup(func() { _ = srv.Close() })
	return "http://" + ln.Addr().String()
}

func tlsFixture(t *testing.T, ca *Authority, h http.Handler) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: h}
	tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{leaf}, MinVersion: tls.VersionTLS12})
	go srv.Serve(tlsLn)
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + ln.Addr().String()
}

func readHTTPHead(t *testing.T, conn net.Conn) string {
	t.Helper()
	var buf []byte
	tmp := make([]byte, 1)
	for !strings.Contains(string(buf), "\r\n\r\n") && len(buf) < 4096 {
		n, err := conn.Read(tmp)
		if err != nil {
			t.Fatalf("read proxy head: %v (%q)", err, buf)
		}
		buf = append(buf, tmp[:n]...)
	}
	return string(buf)
}

func portOf(base string) string {
	u, _ := url.Parse(base)
	return u.Port()
}

func httpOrigin(base string) string {
	u, _ := url.Parse(base)
	return u.Scheme + "://" + u.Host
}

func mustTx(t *testing.T, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
	return tx
}
