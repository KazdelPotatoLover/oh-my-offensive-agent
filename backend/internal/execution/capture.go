package execution

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
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

// BodyExchange is the optional full-body result. The frozen HTTPReceipt only
// carries a preview, so a gateway that can return the fetched body implements this.
type BodyExchange = contract.HTTPExchangeBody

// ResponseBodyGateway is implemented by a capture gateway that can return the
// response body it fetched. The execution proxy never fetches the target itself.
type ResponseBodyGateway interface {
	ExchangeBody(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*BodyExchange, error)
}

type captureJob struct {
	id       uuid.UUID
	scope    contract.ExecutionScope
	origins  []string
	token    string
	gateway  contract.CaptureGateway
	receipts []uuid.UUID
}

type captureHub struct {
	mu   sync.Mutex
	jobs map[uuid.UUID]*captureJob
	ln   net.Listener
	srv  *http.Server
	base string
}

func newCaptureHub() *captureHub {
	return &captureHub{jobs: map[uuid.UUID]*captureJob{}}
}

func (h *captureHub) ensure() (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.base != "" {
		return h.base, nil
	}
	ln, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		return "", unavailable("capture callback could not listen")
	}
	h.ln = ln
	h.srv = &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second}
	go h.srv.Serve(ln)
	_, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		return "", unavailable("capture callback could not listen")
	}
	// The broker rewrites this host to the docker network gateway. The port stays.
	h.base = "http://127.0.0.1:" + port + "/v1/capture"
	return h.base, nil
}

func (h *captureHub) register(job *captureJob) {
	h.mu.Lock()
	h.jobs[job.id] = job
	h.mu.Unlock()
}

func (h *captureHub) forget(id uuid.UUID) []uuid.UUID {
	h.mu.Lock()
	defer h.mu.Unlock()
	job := h.jobs[id]
	delete(h.jobs, id)
	if job == nil {
		return nil
	}
	out := append([]uuid.UUID(nil), job.receipts...)
	return out
}

func (h *captureHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/capture" || r.Method != http.MethodPost {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	if err != nil {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	var in captureExchange
	if err := decodeStrict(raw, &in); err != nil || in.Protocol != ProtocolVersion {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	h.mu.Lock()
	job := h.jobs[in.JobID]
	h.mu.Unlock()
	if job == nil || !tokenMatch(r.Header.Get("Authorization"), job.token) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if job.gateway == nil {
		http.Error(w, "execution_unavailable", http.StatusServiceUnavailable)
		return
	}
	target, err := url.Parse(in.URL)
	if err != nil || !originAllowed(job.origins, target) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	body, err := base64.StdEncoding.DecodeString(in.BodyB64)
	if in.BodyB64 == "" {
		body, err = nil, nil
	}
	if err != nil || len(body) > 1<<20 {
		http.Error(w, "invalid", http.StatusBadRequest)
		return
	}
	req := contract.HTTPRequestInput{
		URL:     target.String(),
		Method:  strings.ToUpper(in.Method),
		Headers: dropSensitive(in.Headers),
	}
	if len(body) > 0 {
		req.BodyText = string(body)
	}
	ex, err := exchangeBody(r.Context(), job.gateway, job.scope, req)
	if err != nil || ex == nil || ex.Receipt == nil {
		http.Error(w, "execution_unavailable", http.StatusServiceUnavailable)
		return
	}
	if err := ex.Receipt.Validate(); err != nil || !ex.Receipt.EmpiricalSource() {
		http.Error(w, "execution_unavailable", http.StatusServiceUnavailable)
		return
	}
	h.mu.Lock()
	if current := h.jobs[in.JobID]; current != nil {
		current.receipts = append(current.receipts, ex.Receipt.ReceiptID)
	}
	h.mu.Unlock()
	out := captureExchangeResult{
		StatusCode:  ex.Receipt.StatusCode,
		Headers:     ex.Headers,
		BodyB64:     base64.StdEncoding.EncodeToString(ex.Body),
		ReceiptID:   ex.Receipt.ReceiptID,
		Truncated:   ex.Receipt.Truncated,
		CaptureMode: ex.Receipt.CaptureMode,
	}
	if out.StatusCode == 0 {
		out.StatusCode = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}

func exchangeBody(ctx context.Context, gw contract.CaptureGateway, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*BodyExchange, error) {
	if bodyGW, ok := gw.(ResponseBodyGateway); ok {
		return bodyGW.ExchangeBody(ctx, scope, in)
	}
	return nil, unavailable("capture gateway cannot return the full response body")
}

func dropSensitive(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		switch strings.ToLower(k) {
		case "authorization", "cookie", "set-cookie", "x-api-key", "proxy-authorization":
			if !contract.IsCredentialReference(v) {
				continue
			}
		}
		out[k] = v
	}
	return out
}

func tokenMatch(header, token string) bool {
	got := strings.TrimPrefix(header, "Bearer ")
	if len(got) != len(token) || token == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
