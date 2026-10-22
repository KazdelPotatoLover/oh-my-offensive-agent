package labapp

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestLabAppIdentitiesTLSAndPatch(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	if strings.Contains(app.EntryPath(), "/login") && app.Marker() == "" {
		t.Fatal("marker missing")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(app.CAPEM()) {
		t.Fatal("ca pem")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	untrusted := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12}}}

	entry, err := http.Get(app.HTTPBase() + app.EntryPath())
	if err != nil {
		t.Fatal(err)
	}
	entryBody, _ := io.ReadAll(entry.Body)
	entry.Body.Close()
	if entry.StatusCode != 200 || !strings.Contains(string(entryBody), app.Marker()) {
		t.Fatalf("entry %d %s", entry.StatusCode, entryBody)
	}

	if _, err := untrusted.Get(app.HTTPSBase() + app.EntryPath()); err == nil {
		t.Fatal("default trust accepted the fixture certificate")
	}
	secure, err := client.Get(app.HTTPSBase() + app.EntryPath())
	if err != nil {
		t.Fatal(err)
	}
	secure.Body.Close()
	if secure.TLS == nil || len(secure.TLS.PeerCertificates) == 0 {
		t.Fatal("https response was not a verified TLS session")
	}

	dana := login(t, client, app, "dana", "clerk-pass", false)
	ravi := login(t, client, app, "ravi", "auditor-pass", true)
	danaItems := materials(t, client, app, dana.Cookie, "")
	raviItems := materials(t, client, app, "", ravi.Token)
	if len(danaItems) == 0 || len(raviItems) == 0 {
		t.Fatalf("missing materials dana=%d ravi=%d", len(danaItems), len(raviItems))
	}
	if danaItems[0].ID == raviItems[0].ID {
		t.Fatal("identities share a material id")
	}
	if raviItems[0].ListedCount == raviItems[0].BinCount {
		t.Fatal("expected a synthetic mismatch before patch")
	}
	other := clientGet(t, client, app.HTTPSBase()+"/"+app.Marker()+"/materials/"+raviItems[0].ID, dana.Cookie, "")
	if other.StatusCode != http.StatusNotFound {
		t.Fatalf("clerk read auditor material: %d", other.StatusCode)
	}
	other.Body.Close()

	patch, err := http.Post(app.AdminBase()+"/patch", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	patch.Body.Close()
	after := materials(t, client, app, "", ravi.Token)
	if after[0].ListedCount != after[0].BinCount {
		t.Fatalf("patch not visible on a new read: %+v", after[0])
	}
	logs := app.Requests()
	if len(logs) < 4 {
		t.Fatalf("expected real request logs, got %d", len(logs))
	}
	sawTLS := false
	for _, row := range logs {
		if row.TLS && row.Identity == "ravi" {
			sawTLS = true
		}
		if strings.Contains(row.Path, "auditor-pass") || strings.Contains(row.Identity, "tok-") {
			t.Fatalf("log leaked credential material: %+v", row)
		}
	}
	if !sawTLS {
		t.Fatal("no TLS request was recorded for the auditor")
	}
	adminLogs := app.Requests()
	for _, row := range adminLogs {
		if row.Path == "/patch" {
			t.Fatal("admin patch was recorded as a catalog request")
		}
	}
}

type loginResult struct {
	Cookie string
	Token  string
}

func TestLabAppHoldOverlapAndIndependentVerify(t *testing.T) {
	app, err := New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = app.Close() })
	app.HoldRequests()
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(app.HTTPBase() + app.EntryPath())
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
		}()
	}
	if !app.WaitInFlight(2, time.Second) {
		t.Fatalf("peak=%d in_flight did not overlap", app.PeakInFlight())
	}
	app.ReleaseRequests()
	wg.Wait()
	if app.PeakInFlight() < 2 {
		t.Fatalf("peak=%d", app.PeakInFlight())
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(app.CAPEM()) {
		t.Fatal("ca pem")
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}}}
	auditor, _ := app.Identity("ravi")
	session := login(t, client, app, auditor.Username, auditor.Password, false)
	before := clientGet(t, client, app.HTTPSBase()+app.VerifyPath(), session.Cookie, "")
	defer before.Body.Close()
	var prior struct {
		Mismatch bool `json:"mismatch"`
	}
	if err := json.NewDecoder(before.Body).Decode(&prior); err != nil || !prior.Mismatch {
		t.Fatalf("expected mismatch before patch: %+v %v", prior, err)
	}
	req, _ := http.NewRequest(http.MethodPost, app.AdminBase()+"/patch", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	after := clientGet(t, client, app.HTTPSBase()+app.VerifyPath(), session.Cookie, "")
	defer after.Body.Close()
	var fixed struct {
		Mismatch bool   `json:"mismatch"`
		Marker   string `json:"marker"`
	}
	if err := json.NewDecoder(after.Body).Decode(&fixed); err != nil || fixed.Mismatch || fixed.Marker != app.Marker() {
		t.Fatalf("independent verify after patch: %+v %v", fixed, err)
	}
	script := clientGet(t, client, app.HTTPSBase()+app.ScriptPath(), "", session.Token)
	defer script.Body.Close()
	raw, _ := io.ReadAll(script.Body)
	if script.StatusCode != 200 || !strings.Contains(string(raw), app.Marker()) {
		t.Fatalf("script %d %s", script.StatusCode, raw)
	}
}

func login(t *testing.T, client *http.Client, app *Server, user, pass string, asJSON bool) loginResult {
	t.Helper()
	var body io.Reader
	contentType := "application/x-www-form-urlencoded"
	if asJSON {
		contentType = "application/json"
		raw, _ := json.Marshal(map[string]string{"username": user, "password": pass})
		body = strings.NewReader(string(raw))
	} else {
		form := url.Values{"username": {user}, "password": {pass}}
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(http.MethodPost, app.HTTPSBase()+"/"+app.Marker()+"/login", body)
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("login %s: %d %s", user, resp.StatusCode, raw)
	}
	var parsed struct {
		Token string `json:"token"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&parsed)
	cookie := ""
	for _, c := range resp.Cookies() {
		if c.Name == "lab_session" {
			cookie = c.Value
		}
	}
	if cookie == "" || parsed.Token == "" {
		t.Fatalf("login %s missing session", user)
	}
	return loginResult{Cookie: cookie, Token: parsed.Token}
}

func materials(t *testing.T, client *http.Client, app *Server, cookie, token string) []Material {
	t.Helper()
	resp := clientGet(t, client, app.HTTPSBase()+"/"+app.Marker()+"/materials", cookie, token)
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("materials %d %s", resp.StatusCode, raw)
	}
	var parsed struct {
		Materials []Material `json:"materials"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		t.Fatal(err)
	}
	return parsed.Materials
}

func clientGet(t *testing.T, client *http.Client, rawURL, cookie, token string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodGet, rawURL, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "lab_session", Value: cookie})
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}
