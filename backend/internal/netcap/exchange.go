package netcap

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// Resolver looks up a target host. Tests inject it so a failed policy check
// never has to dial the development database.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

type systemResolver struct{}

func (systemResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	out := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if addr.IP != nil {
			out = append(out, addr.IP)
		}
	}
	return out, nil
}

// Transport is the production scoped HTTP client. It does not read HTTP_PROXY
// and it does not share a cookie jar between directions.
type Transport struct {
	store      *Store
	resolver   Resolver
	beforeDial func()
	mu         sync.Mutex
	fixtureCAs []FixtureCA
	flights    flightGroup
	cancels    *cancelRegistry
}

func NewTransport(store *Store) *Transport {
	return &Transport{store: store, resolver: systemResolver{}, cancels: newCancelRegistry()}
}

// TrustFixtureCA adds a CA for the listed origins only. It does not disable
// certificate verification for any other host.
func (t *Transport) TrustFixtureCA(pem []byte, origins []string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.fixtureCAs = append(t.fixtureCAs, FixtureCA{PEM: append([]byte(nil), pem...), Origins: append([]string(nil), origins...)})
}

func (t *Transport) SetBeforeDial(fn func()) { t.beforeDial = fn }

// Do implements contract.TargetTransport. The model cannot widen scope.
func (t *Transport) Do(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*contract.HTTPReceipt, error) {
	res, err := t.do(ctx, scope, in, callOpts{CaptureMode: contract.CaptureDirect, FollowRedirects: true, ValidateScope: true})
	if err != nil {
		return nil, err
	}
	return res.Receipt, nil
}

// DoCaptured is the gateway path. captureMode tls_intercept is set only after
// the gateway decrypted a CONNECT with its own CA. Direct clients stay direct.
func (t *Transport) DoCaptured(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput, captureMode string) (*contract.HTTPReceipt, error) {
	res, err := t.do(ctx, scope, in, callOpts{CaptureMode: captureMode, FollowRedirects: true, ValidateScope: true})
	if err != nil {
		return nil, err
	}
	return res.Receipt, nil
}

// LegacyResult is the observed GET used by the historical get_endpoint JSON.
type LegacyResult struct {
	StatusCode     int
	Headers        map[string]string
	Body           []byte
	Truncated      bool
	Note           string
	Started        time.Time
	HeaderReceived time.Time
	Received       time.Time
	ErrType        string
	ErrMessage     string
	Receipt        *contract.HTTPReceipt
}

// FetchLegacyGET performs one non-following HTTP GET through the shared quota
// ledger. It preserves the historical 5s / 64KiB limits.
func (t *Transport) FetchLegacyGET(ctx context.Context, scope contract.ExecutionScope, assetID uuid.UUID, rawURL string, toolArgs []byte) (LegacyResult, error) {
	in := contract.HTTPRequestInput{AssetID: &assetID, URL: rawURL, Method: http.MethodGet}
	res, err := t.do(ctx, scope, in, callOpts{
		CaptureMode:     contract.CaptureDirect,
		FollowRedirects: false,
		ValidateScope:   false,
		Legacy:          true,
		ToolName:        "get_endpoint",
		ToolArgs:        toolArgs,
		Timeout:         5 * time.Second,
		MaxBody:         defaultMaxBody,
		UserAgent:       "AgentWorkbench-Trace/1.0",
	})
	if err != nil && res.StatusCode == 0 {
		kind, msg := legacyErr(err)
		return LegacyResult{ErrType: kind, ErrMessage: msg, Started: res.Started, Receipt: res.Receipt}, err
	}
	out := LegacyResult{
		StatusCode: res.StatusCode, Headers: res.Headers, Body: res.Body, Truncated: res.Truncated,
		Note: res.Note, Started: res.Started, HeaderReceived: res.HeaderReceived, Received: res.Received,
		Receipt: res.Receipt,
	}
	if err != nil {
		out.ErrType, out.ErrMessage = legacyErr(err)
		if res.StatusCode != 0 {
			out.Note = err.Error()
		}
	}
	return out, err
}

type callOpts struct {
	CaptureMode     string
	FollowRedirects bool
	ValidateScope   bool
	Legacy          bool
	ToolName        string
	ToolArgs        []byte
	Timeout         time.Duration
	MaxBody         int
	UserAgent       string
}

type observed struct {
	Receipt        *contract.HTTPReceipt
	StatusCode     int
	Headers        map[string]string
	Body           []byte
	Truncated      bool
	Note           string
	Started        time.Time
	HeaderReceived time.Time
	Received       time.Time
}

type prepared struct {
	Policy      Policy
	Method      string
	URL         *url.URL
	Headers     http.Header
	Body        []byte
	ContentType string
	AssetID     *uuid.UUID
	CredRef     string
	CredVersion int
	SessionRef  string
	Sensitive   bool
	Opts        callOpts
}

func (t *Transport) do(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput, opts callOpts) (observed, error) {
	if t == nil || t.store == nil {
		return observed{}, contract.ErrNotImplemented
	}
	if opts.ValidateScope {
		if err := scope.Validate(); err != nil {
			return observed{}, err
		}
	} else if scope.RunID == uuid.Nil || scope.EpochNumber < 1 {
		return observed{}, contractError(contract.CodeInvalidInput, "execution scope requires run_id and epoch")
	}
	if err := in.Validate(); err != nil {
		return observed{}, err
	}
	prep, err := t.prepare(ctx, scope, in, opts)
	if err != nil {
		return observed{}, err
	}
	if prep.Opts.FollowRedirects && (prep.Method == http.MethodGet || prep.Method == http.MethodHead) && len(prep.Body) == 0 {
		key := flightKey(scope, prep)
		if follower, leader := t.flights.begin(key); !leader {
			return follower.wait(ctx)
		}
		res, execErr := t.execute(ctx, scope, prep)
		t.flights.complete(key, res, execErr)
		return res, execErr
	}
	return t.execute(ctx, scope, prep)
}

func (t *Transport) prepare(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput, opts callOpts) (prepared, error) {
	policy, err := t.store.LoadPolicy(ctx, scope.RunID)
	if err != nil {
		return prepared{}, err
	}
	t.mu.Lock()
	policy.FixtureCAs = append(policy.FixtureCAs, t.fixtureCAs...)
	t.mu.Unlock()
	current, err := t.store.CurrentEpoch(ctx, scope.RunID)
	if err != nil {
		return prepared{}, err
	}
	if current != scope.EpochNumber {
		return prepared{}, contractError(contract.CodeStaleEpoch, "execution scope epoch is stale")
	}
	target, assetID, err := t.resolveTarget(ctx, scope, in)
	if err != nil {
		return prepared{}, err
	}
	if _, err := policy.Match(target); err != nil {
		return prepared{}, contractError(contract.CodeScopeDenied, err.Error())
	}
	method := strings.ToUpper(strings.TrimSpace(in.Method))
	if opts.Legacy {
		method = http.MethodGet
	}
	if err := policy.AllowsMethod(method); err != nil {
		return prepared{}, contractError(contract.CodeScopeDenied, err.Error())
	}
	if in.TimeoutSeconds > int(contract.MaxHTTPToolTimeout/time.Second) {
		return prepared{}, contractError(contract.CodeInvalidInput, "timeout_seconds exceeds 120")
	}
	body, contentType, err := buildBody(in)
	if err != nil {
		return prepared{}, err
	}
	maxBody := policy.MaxBodyBytes
	if opts.MaxBody > 0 {
		maxBody = opts.MaxBody
	}
	if len(body) > maxBody || len(body) > hardMaxBody {
		return prepared{}, contractError(contract.CodePayloadTooLarge, "request body exceeds the frozen limit")
	}
	origin := canonicalOrigin(target)
	headers, sensitive, credVersion, sessionRef, err := t.resolveAuth(ctx, scope, policy, in, origin)
	if err != nil {
		return prepared{}, err
	}
	if contentType != "" && headers.Get("Content-Type") == "" {
		headers.Set("Content-Type", contentType)
	}
	ua := opts.UserAgent
	if ua == "" {
		ua = "AgentWorkbench-HTTP/1.0"
	}
	if headers.Get("User-Agent") == "" {
		headers.Set("User-Agent", ua)
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		if in.TimeoutSeconds > 0 {
			timeout = time.Duration(in.TimeoutSeconds) * time.Second
		} else {
			timeout = policy.DefaultTimeout
		}
	}
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	if timeout > policy.MaxTimeout && policy.MaxTimeout > 0 {
		timeout = policy.MaxTimeout
	}
	if timeout > contract.MaxHTTPToolTimeout {
		timeout = contract.MaxHTTPToolTimeout
	}
	opts.Timeout = timeout
	opts.MaxBody = maxBody
	if opts.CaptureMode == "" {
		opts.CaptureMode = contract.CaptureDirect
	}
	credRef := in.CredentialRef
	if credRef == "" {
		credRef = in.SessionRef
	}
	return prepared{
		Policy: policy, Method: method, URL: target, Headers: headers, Body: body, ContentType: contentType,
		AssetID: assetID, CredRef: credRef, CredVersion: credVersion, SessionRef: sessionRef, Sensitive: sensitive, Opts: opts,
	}, nil
}

func (t *Transport) resolveTarget(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*url.URL, *uuid.UUID, error) {
	// URL-only reads can cite an already assigned asset without requiring the
	// model to translate the URL into an ID. This never creates new scope.
	if in.AssetID == nil && strings.TrimSpace(in.URL) != "" {
		id, err := t.store.AssetForURL(ctx, scope, strings.TrimSpace(in.URL))
		if err != nil {
			return nil, nil, err
		}
		in.AssetID = id
	}
	var assetURL string
	if in.AssetID != nil {
		ok, err := t.store.AssetInRun(ctx, scope.RunID, *in.AssetID)
		if err != nil {
			return nil, nil, err
		}
		if !ok {
			return nil, nil, contractError(contract.CodeScopeDenied, "asset is outside run scope")
		}
		if scope.WorkItemID != nil && *scope.WorkItemID != uuid.Nil {
			inItem, err := t.store.AssetInWorkItem(ctx, *scope.WorkItemID, *in.AssetID)
			if err != nil {
				return nil, nil, err
			}
			if !inItem {
				return nil, nil, contractError(contract.CodeScopeDenied, "asset is outside work item scope")
			}
		}
		assetURL, _, err = t.store.AssetURL(ctx, scope.RunID, *in.AssetID)
		if err != nil {
			return nil, nil, err
		}
	}
	raw := strings.TrimSpace(in.URL)
	if raw == "" {
		raw = assetURL
	}
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, nil, contractError(contract.CodeInvalidInput, "url must be an absolute http or https URL")
	}
	if assetURL != "" && in.URL != "" {
		asset, err := url.Parse(assetURL)
		if err != nil || asset.Scheme != parsed.Scheme || !strings.EqualFold(asset.Host, parsed.Host) || asset.EscapedPath() != parsed.EscapedPath() {
			return nil, nil, contractError(contract.CodeScopeDenied, "url does not match the frozen asset")
		}
	}
	if len(in.Query) > 0 {
		q := parsed.Query()
		for key, value := range in.Query {
			q.Set(key, value)
		}
		parsed.RawQuery = q.Encode()
	}
	if parsed.Path == "" {
		parsed.Path = "/"
	}
	return parsed, in.AssetID, nil
}

func (t *Transport) resolveAuth(ctx context.Context, scope contract.ExecutionScope, policy Policy, in contract.HTTPRequestInput, origin string) (http.Header, bool, int, string, error) {
	headers := make(http.Header)
	sensitive := false
	version := 0
	for name, value := range in.Headers {
		if isSensitiveHeader(name) || contract.IsCredentialReference(value) {
			alias := strings.TrimPrefix(value, contract.CredentialRefPrefix)
			cred, err := t.store.LoadCredential(ctx, alias, origin)
			if err != nil {
				return nil, false, 0, "", err
			}
			headerName := name
			if cred.HeaderName != "" {
				headerName = cred.HeaderName
			}
			headers.Set(headerName, cred.Secret)
			sensitive = true
			if cred.Version > version {
				version = cred.Version
			}
			continue
		}
		headers.Set(name, value)
	}
	var cred Credential
	var err error
	if in.CredentialRef != "" {
		cred, err = t.store.LoadCredential(ctx, strings.TrimPrefix(in.CredentialRef, contract.CredentialRefPrefix), origin)
		if err != nil {
			return nil, false, 0, "", err
		}
		version = cred.Version
		sensitive = true
	}
	sessionRef := ""
	if in.SessionRef != "" {
		sessionRef = strings.TrimPrefix(in.SessionRef, contract.CredentialRefPrefix)
	}
	if sessionRef != "" || cred.Kind == "login" || cred.Kind == "cookie" {
		ref := sessionRef
		if ref == "" {
			ref = "cred-" + cred.Alias
		}
		sess, err := t.store.OpenSession(ctx, scope, ref, origin, cred.Alias, version)
		if err != nil {
			return nil, false, 0, "", err
		}
		sessionRef = ref
		switch {
		case sess.Cookie != "" || sess.Authorization != "":
			if sess.Cookie != "" {
				headers.Set("Cookie", sess.Cookie)
			}
			if sess.Authorization != "" {
				headers.Set("Authorization", sess.Authorization)
			}
		case cred.Kind == "login":
			cookie, err := t.login(ctx, scope, policy, cred, origin)
			if err != nil {
				return nil, false, 0, "", err
			}
			if err := t.store.SaveSessionAuth(ctx, scope.RunID, ref, cookie, "", cred.Version); err != nil {
				return nil, false, 0, "", err
			}
			headers.Set("Cookie", cookie)
		case cred.Kind == "cookie":
			if err := t.store.SaveSessionAuth(ctx, scope.RunID, ref, cred.Secret, "", cred.Version); err != nil {
				return nil, false, 0, "", err
			}
			headers.Set("Cookie", cred.Secret)
		case cred.Kind == "token":
			name := cred.HeaderName
			if name == "" {
				name = "Authorization"
			}
			headers.Set(name, cred.Secret)
			if err := t.store.SaveSessionAuth(ctx, scope.RunID, ref, "", cred.Secret, cred.Version); err != nil {
				return nil, false, 0, "", err
			}
		default:
			return nil, false, 0, "", contractError(contract.CodeCredentialUnavailable, "session has no stored authentication")
		}
		sensitive = true
	} else if cred.Kind == "token" {
		name := cred.HeaderName
		if name == "" {
			name = "Authorization"
		}
		headers.Set(name, cred.Secret)
		sensitive = true
	}
	return headers, sensitive, version, sessionRef, nil
}

func (t *Transport) login(ctx context.Context, scope contract.ExecutionScope, policy Policy, cred Credential, origin string) (string, error) {
	loginURL, err := url.Parse(origin)
	if err != nil {
		return "", contractError(contract.CodeCredentialUnavailable, "credential origin is invalid")
	}
	path := cred.LoginPath
	if path == "" {
		path = "/login"
	}
	loginURL.Path = path
	if _, err := policy.Match(loginURL); err != nil {
		return "", contractError(contract.CodeScopeDenied, "login path is outside the frozen origin grant")
	}
	form := url.Values{}
	form.Set("username", cred.Username)
	form.Set("password", cred.Secret)
	spec := prepared{
		Policy: policy, Method: http.MethodPost, URL: loginURL,
		Headers: http.Header{"Content-Type": []string{"application/x-www-form-urlencoded"}},
		Body:    []byte(form.Encode()), ContentType: "application/x-www-form-urlencoded",
		Sensitive: true, CredRef: contract.CredentialRefPrefix + cred.Alias, CredVersion: cred.Version,
		Opts: callOpts{CaptureMode: contract.CaptureDirect, FollowRedirects: false, Timeout: policy.DefaultTimeout, MaxBody: policy.MaxBodyBytes, UserAgent: "AgentWorkbench-HTTP/1.0"},
	}
	res, err := t.execute(ctx, scope, spec)
	if err != nil && res.StatusCode == 0 {
		return "", err
	}
	cookie := res.Headers["Set-Cookie"]
	if cookie == "" {
		for key, value := range res.Headers {
			if strings.EqualFold(key, "Set-Cookie") && value != "" {
				cookie = value
				break
			}
		}
	}
	if cookie == "" {
		return "", contractError(contract.CodeCredentialUnavailable, "login did not establish a session")
	}
	return cookiePair(cookie), nil
}

func (t *Transport) execute(ctx context.Context, scope contract.ExecutionScope, spec prepared) (observed, error) {
	timeout := spec.Opts.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := t.cancels.track(scope, cancel)
	defer stop()

	origin := canonicalOrigin(spec.URL)
	page := pageKey(spec.Method, spec.URL)
	countPage := spec.Method == http.MethodGet || spec.Method == http.MethodHead
	resv, err := t.store.Reserve(ctx, scope, origin, spec.Policy, page, countPage)
	if err != nil {
		return observed{}, err
	}
	finished := false
	defer func() {
		if !finished {
			t.store.FinishReservation(context.Background(), resv.ID, "cancelled_before_send", nil)
			if resv.ClaimedPage {
				t.store.ReleaseUnsentPage(context.Background(), scope, resv.PageKey)
			}
		}
	}()
	if resv.Wait > 0 {
		timer := time.NewTimer(resv.Wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return observed{}, ctx.Err()
		case <-timer.C:
		}
	}
	started := time.Now()
	resp, finalURL, err := t.roundTrip(ctx, spec)
	headerAt := time.Now()
	saveCtx, saveCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer saveCancel()
	if err != nil {
		outcome := classifyOutcome(err, spec.Method, true)
		rec := t.persist(saveCtx, scope, spec, finalURL, 0, nil, nil, false, outcome, started)
		finished = true
		var id *uuid.UUID
		if rec != nil {
			id = &rec.ReceiptID
		}
		t.store.FinishReservation(context.Background(), resv.ID, outcome, id)
		return observed{Receipt: rec, Started: started, HeaderReceived: headerAt}, err
	}
	defer resp.Body.Close()
	t.store.NotePage(ctx, scope, origin, page)
	body, truncated, readErr := readLimited(resp.Body, spec.Opts.MaxBody)
	received := time.Now()
	outcome := "completed"
	if readErr != nil {
		outcome = classifyOutcome(readErr, spec.Method, true)
	}
	binary := !utf8.Valid(body)
	preview := redactBodyPreview(body, binary)
	note := ""
	if truncated {
		note = "saved partial response: response body exceeded the frozen limit"
		if outcome == "completed" {
			// A truncated body is recorded, but verification will not treat it as empirical.
		}
	}
	reqHeader := spec.Headers.Clone()
	respHeader := resp.Header.Clone()
	reqHash := hashParts(0, reqHeader, spec.Body, spec.Method, spec.URL.String())
	respHash := hashParts(resp.StatusCode, respHeader, body, "", "")
	replay := outcome == "completed" && !truncated &&
		(spec.Method == http.MethodGet || spec.Method == http.MethodHead) &&
		spec.CredRef == "" && spec.SessionRef == "" && len(spec.Body) == 0 &&
		(spec.Opts.CaptureMode == contract.CaptureDirect || spec.Opts.CaptureMode == contract.CaptureTLSIntercept)
	ex := savedExchange{
		Scope: scope, AssetID: spec.AssetID, Method: spec.Method, URL: redactURL(spec.URL.String()),
		FinalURL: redactURL(finalURL.String()), Status: resp.StatusCode, Mode: spec.Opts.CaptureMode,
		RequestHash: reqHash, ResponseHash: respHash, CredRef: spec.CredRef, CredVersion: spec.CredVersion,
		SessionRef: spec.SessionRef, Truncated: truncated, Outcome: outcome, Preview: preview, ReplaySafe: replay,
		RequestHead: map[string][]string(redactHeaders(reqHeader)), RequestBody: redactStoredBody(spec.Body, spec.Sensitive),
		RequestType: spec.ContentType, ResponseHead: map[string][]string(respHeader), ResponseBody: body,
		ResponseType: resp.Header.Get("Content-Type"), Binary: binary, Sensitive: spec.Sensitive || responseSensitive(respHeader),
		ToolName: spec.Opts.ToolName, ToolArgs: spec.Opts.ToolArgs,
	}
	if spec.Opts.ToolName == "" {
		ex.ToolName = "http_request"
	}
	rec, saveErr := t.store.SaveExchange(saveCtx, ex)
	finished = true
	var receiptID *uuid.UUID
	if rec != nil {
		receiptID = &rec.ReceiptID
	}
	t.store.FinishReservation(context.Background(), resv.ID, outcome, receiptID)
	obs := observed{
		Receipt: rec, StatusCode: resp.StatusCode, Headers: headerMap(resp.Header), Body: body,
		Truncated: truncated, Note: note, Started: started, HeaderReceived: headerAt, Received: received,
	}
	if saveErr != nil {
		return obs, saveErr
	}
	if readErr != nil {
		return obs, readErr
	}
	return obs, nil
}

func (t *Transport) persist(ctx context.Context, scope contract.ExecutionScope, spec prepared, final *url.URL, status int, respHeader http.Header, body []byte, truncated bool, outcome string, started time.Time) *contract.HTTPReceipt {
	if final == nil {
		final = spec.URL
	}
	if respHeader == nil {
		respHeader = http.Header{}
	}
	ex := savedExchange{
		Scope: scope, AssetID: spec.AssetID, Method: spec.Method, URL: redactURL(spec.URL.String()),
		FinalURL: redactURL(final.String()), Status: status, Mode: spec.Opts.CaptureMode,
		RequestHash:  hashParts(0, spec.Headers, spec.Body, spec.Method, spec.URL.String()),
		ResponseHash: hashParts(status, respHeader, body, "", ""),
		CredRef:      spec.CredRef, CredVersion: spec.CredVersion, SessionRef: spec.SessionRef,
		Truncated: truncated, Outcome: outcome, Preview: redactBodyPreview(body, !utf8.Valid(body)),
		RequestHead: map[string][]string(redactHeaders(spec.Headers)), RequestBody: redactStoredBody(spec.Body, spec.Sensitive),
		ResponseHead: map[string][]string(respHeader), ResponseBody: body, Sensitive: spec.Sensitive,
		ToolName: spec.Opts.ToolName, ToolArgs: spec.Opts.ToolArgs,
	}
	rec, err := t.store.SaveExchange(ctx, ex)
	if err != nil {
		return nil
	}
	_ = started
	return rec
}

func (t *Transport) roundTrip(ctx context.Context, spec prepared) (*http.Response, *url.URL, error) {
	method := spec.Method
	dest := spec.URL
	body := append([]byte(nil), spec.Body...)
	headers := spec.Headers.Clone()
	authOrigin := canonicalOrigin(dest)
	hops := 0
	if spec.Opts.FollowRedirects {
		hops = spec.Policy.MaxRedirects
	}
	for hop := 0; ; hop++ {
		rule, err := spec.Policy.Match(dest)
		if err != nil {
			return nil, dest, contractError(contract.CodeScopeDenied, err.Error())
		}
		if err := spec.Policy.AllowsMethod(method); err != nil {
			return nil, dest, contractError(contract.CodeScopeDenied, err.Error())
		}
		if canonicalOrigin(dest) != authOrigin {
			headers.Del("Cookie")
			headers.Del("Authorization")
			headers.Del("X-Api-Key")
			authOrigin = canonicalOrigin(dest)
		}
		ip, err := t.pickIP(ctx, dest.Hostname(), portOrDefault(dest), rule)
		if err != nil {
			return nil, dest, err
		}
		resp, err := t.dial(ctx, method, dest, headers, body, ip, canonicalOrigin(dest), spec.Policy)
		if err != nil {
			return nil, dest, err
		}
		if hop >= hops || !isRedirect(resp.StatusCode) {
			return resp, dest, nil
		}
		loc, locErr := resp.Location()
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
		if locErr != nil {
			return nil, dest, contractError(contract.CodeScopeDenied, "redirect is missing a location")
		}
		next := dest.ResolveReference(loc)
		if next.User != nil {
			return nil, dest, contractError(contract.CodeScopeDenied, "redirect must not embed credentials")
		}
		if resp.StatusCode == http.StatusMovedPermanently || resp.StatusCode == http.StatusFound || resp.StatusCode == http.StatusSeeOther {
			method = http.MethodGet
			body = nil
			headers.Del("Content-Type")
		}
		dest = next
	}
}

func (t *Transport) pickIP(ctx context.Context, host, port string, rule OriginRule) (net.IP, error) {
	resolver := t.resolver
	if resolver == nil {
		resolver = systemResolver{}
	}
	ips, err := resolver.LookupIP(ctx, host)
	if err != nil {
		return nil, contractError(contract.CodeExecutionUnavailable, "dns lookup failed")
	}
	var denied error
	for _, ip := range ips {
		if err := DestinationAllowed(ip.String(), port, rule); err != nil {
			denied = err
			continue
		}
		return ip, nil
	}
	if denied != nil {
		return nil, contractError(contract.CodeScopeDenied, denied.Error())
	}
	return nil, contractError(contract.CodeExecutionUnavailable, "dns returned no addresses")
}

func (t *Transport) dial(ctx context.Context, method string, dest *url.URL, headers http.Header, body []byte, ip net.IP, origin string, policy Policy) (*http.Response, error) {
	pool, err := t.certPool(origin, policy)
	if err != nil {
		return nil, err
	}
	serverName := dest.Hostname()
	if net.ParseIP(serverName) != nil {
		serverName = ""
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	pinned := net.JoinHostPort(ip.String(), portOrDefault(dest))
	transport := &http.Transport{
		Proxy:              nil,
		DisableCompression: true,
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
			ServerName: serverName,
			RootCAs:    pool,
		},
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			if t.beforeDial != nil {
				t.beforeDial()
			}
			return dialer.DialContext(ctx, network, pinned)
		},
		MaxResponseHeaderBytes: 64 * 1024,
	}
	defer transport.CloseIdleConnections()
	var reader io.Reader
	if body != nil && method != http.MethodGet && method != http.MethodHead {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, dest.String(), reader)
	if err != nil {
		return nil, err
	}
	req.Header = headers.Clone()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (t *Transport) certPool(origin string, policy Policy) (*x509.CertPool, error) {
	pool, err := certPoolFromPEMs(policy.CAPool(origin))
	if err != nil {
		return nil, contractError(contract.CodeExecutionUnavailable, err.Error())
	}
	return pool, nil
}

func buildBody(in contract.HTTPRequestInput) ([]byte, string, error) {
	if err := in.Validate(); err != nil {
		return nil, "", err
	}
	if len(in.BodyJSON) > 0 && string(in.BodyJSON) != "null" {
		return append([]byte(nil), in.BodyJSON...), "application/json", nil
	}
	if in.BodyText != "" {
		return []byte(in.BodyText), "text/plain; charset=utf-8", nil
	}
	if len(in.Form) > 0 {
		values := url.Values{}
		for key, value := range in.Form {
			values.Set(key, value)
		}
		return []byte(values.Encode()), "application/x-www-form-urlencoded", nil
	}
	return nil, "", nil
}

func readLimited(r io.Reader, limit int) ([]byte, bool, error) {
	if limit <= 0 {
		limit = defaultMaxBody
	}
	if limit > hardMaxBody {
		limit = hardMaxBody
	}
	buf, err := io.ReadAll(io.LimitReader(r, int64(limit+1)))
	truncated := len(buf) > limit
	if truncated {
		buf = buf[:limit]
	}
	return buf, truncated, err
}

func isRedirect(code int) bool {
	return code == http.StatusMovedPermanently || code == http.StatusFound || code == http.StatusSeeOther || code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect
}

func classifyOutcome(err error, method string, sent bool) string {
	if !sent {
		return "cancelled_before_send"
	}
	side := method != http.MethodGet && method != http.MethodHead && method != http.MethodOptions
	if errors.Is(err, context.Canceled) {
		if side {
			return "outcome_unknown"
		}
		return "cancelled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if side {
			return "outcome_unknown"
		}
		return "timeout"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		if side {
			return "outcome_unknown"
		}
		return "timeout"
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "certificate") || strings.Contains(msg, "x509") || strings.Contains(msg, "tls") {
		return "tls_error"
	}
	if side {
		return "outcome_unknown"
	}
	return "connection_error"
}

func legacyErr(err error) (string, string) {
	if err == nil {
		return "", ""
	}
	if errors.Is(err, context.Canceled) {
		return "context_canceled", "request canceled by stop or timeout"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", "request timed out (5s limit exceeded)"
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout", "request timed out (5s limit exceeded)"
	}
	return "connection_error", err.Error()
}

func cookiePair(setCookie string) string {
	parts := strings.Split(setCookie, ";")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func responseSensitive(h http.Header) bool {
	return h.Get("Set-Cookie") != ""
}

func redactStoredBody(body []byte, sensitive bool) []byte {
	if !sensitive {
		return body
	}
	text := redactSensitiveText(string(body))
	return []byte(text)
}

func flightKey(scope contract.ExecutionScope, spec prepared) string {
	return strings.Join([]string{
		scope.RunID.String(),
		strconv.Itoa(scope.EpochNumber),
		spec.Method,
		spec.URL.String(),
		strconv.Itoa(spec.CredVersion),
		spec.SessionRef,
		sha256Hex(spec.Body),
	}, "|")
}

type flightGroup struct {
	mu sync.Mutex
	m  map[string]*flight
}

type flight struct {
	done   chan struct{}
	result observed
	err    error
}

func (g *flightGroup) begin(key string) (*flight, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*flight{}
	}
	if existing, ok := g.m[key]; ok {
		return existing, false
	}
	f := &flight{done: make(chan struct{})}
	g.m[key] = f
	return f, true
}

func (g *flightGroup) complete(key string, result observed, err error) {
	g.mu.Lock()
	f := g.m[key]
	delete(g.m, key)
	g.mu.Unlock()
	if f == nil {
		return
	}
	f.result = result
	f.err = err
	close(f.done)
}

func (f *flight) wait(ctx context.Context) (observed, error) {
	select {
	case <-ctx.Done():
		return observed{}, ctx.Err()
	case <-f.done:
		return f.result, f.err
	}
}

type cancelRegistry struct {
	mu    sync.Mutex
	items map[string]map[int]context.CancelFunc
	seq   int
}

func newCancelRegistry() *cancelRegistry {
	return &cancelRegistry{items: map[string]map[int]context.CancelFunc{}}
}

func (r *cancelRegistry) track(scope contract.ExecutionScope, cancel context.CancelFunc) func() {
	if r == nil || cancel == nil {
		return func() {}
	}
	key := scope.RunID.String() + ":" + strconv.Itoa(scope.EpochNumber)
	r.mu.Lock()
	r.seq++
	id := r.seq
	if r.items[key] == nil {
		r.items[key] = map[int]context.CancelFunc{}
	}
	r.items[key][id] = cancel
	r.mu.Unlock()
	return func() {
		r.mu.Lock()
		if set := r.items[key]; set != nil {
			delete(set, id)
		}
		r.mu.Unlock()
	}
}

func (r *cancelRegistry) cancelScope(scope contract.ExecutionScope, jobID *uuid.UUID) int {
	if r == nil {
		return 0
	}
	key := scope.RunID.String() + ":" + strconv.Itoa(scope.EpochNumber)
	r.mu.Lock()
	set := r.items[key]
	var fns []context.CancelFunc
	for id, fn := range set {
		fns = append(fns, fn)
		delete(set, id)
	}
	r.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
	_ = jobID
	return len(fns)
}
