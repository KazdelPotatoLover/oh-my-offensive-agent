package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// StandInGateway is an explicit test double for Agent 3's CaptureGateway.
// It fetches the target itself and mints a receipt. Production must inject the
// real gateway; this type is not constructed by the broker or the executor.
type StandInGateway struct {
	Allow  []string
	Client *http.Client

	mu       sync.Mutex
	receipts map[uuid.UUID]*contract.HTTPReceipt
	bodies   map[uuid.UUID][]byte
}

func NewStandInGateway(allow []string, client *http.Client) *StandInGateway {
	if client == nil {
		client = &http.Client{}
	}
	return &StandInGateway{
		Allow:    allow,
		Client:   client,
		receipts: map[uuid.UUID]*contract.HTTPReceipt{},
		bodies:   map[uuid.UUID][]byte{},
	}
}

func (g *StandInGateway) Exchange(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*contract.HTTPReceipt, error) {
	ex, err := g.ExchangeBody(ctx, scope, in)
	if err != nil {
		return nil, err
	}
	return ex.Receipt, nil
}

func (g *StandInGateway) ExchangeBody(ctx context.Context, scope contract.ExecutionScope, in contract.HTTPRequestInput) (*BodyExchange, error) {
	if err := in.Validate(); err != nil {
		return nil, err
	}
	parsed, err := url.Parse(in.URL)
	if err != nil || !originAllowed(g.Allow, parsed) {
		return nil, scopeDenied("command origin is not in the work item scope")
	}
	req, err := http.NewRequestWithContext(ctx, strings.ToUpper(in.Method), in.URL, strings.NewReader(in.BodyText))
	if err != nil {
		return nil, unavailable("capture stand-in could not fetch the fixture")
	}
	for k, v := range in.Headers {
		req.Header.Set(k, v)
	}
	res, err := g.Client.Do(req)
	if err != nil {
		return nil, unavailable("capture stand-in could not fetch the fixture")
	}
	defer res.Body.Close()
	body, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return nil, unavailable("capture stand-in could not fetch the fixture")
	}
	mode := contract.CaptureDirect
	if parsed.Scheme == "https" {
		mode = contract.CaptureTLSIntercept
	}
	preview := string(body)
	truncated := false
	if len(preview) > 512 {
		preview = preview[:512]
		truncated = true
	}
	sum := sha256.Sum256(body)
	receipt := &contract.HTTPReceipt{
		RequestID:       uuid.New(),
		ReceiptID:       uuid.New(),
		Method:          strings.ToUpper(in.Method),
		URL:             in.URL,
		StatusCode:      res.StatusCode,
		ResponsePreview: preview,
		Truncated:       truncated,
		ResponseHash:    hex.EncodeToString(sum[:]),
		CaptureMode:     mode,
		Scope:           scope,
	}
	if err := receipt.Validate(); err != nil {
		return nil, err
	}
	g.mu.Lock()
	g.receipts[receipt.ReceiptID] = receipt
	g.bodies[receipt.ReceiptID] = append([]byte(nil), body...)
	g.mu.Unlock()
	return &BodyExchange{Receipt: receipt, Body: body, Headers: map[string]string{"Content-Type": res.Header.Get("Content-Type")}}, nil
}

func (g *StandInGateway) Lookup(ctx context.Context, scope contract.ExecutionScope, receiptID uuid.UUID) (*contract.HTTPReceipt, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	receipt := g.receipts[receiptID]
	if receipt == nil || receipt.Scope.RunID != scope.RunID {
		return nil, notFound("capture receipt was not found")
	}
	return receipt, nil
}
