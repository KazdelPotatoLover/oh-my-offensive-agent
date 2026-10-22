package netcap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"unicode/utf8"

	"agent-workbench/backend/internal/contract"
)

func TestGatewayExecutionGetsCapturedBodyBeyondPreview(t *testing.T) {
	pool := ma3Pool(t)
	body := strings.Repeat("payload-", 512)
	server := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, body)
	}))
	scope := seedRun(t, pool, fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}]}}`, portOf(server)))
	gw := NewGateway(NewTransport(NewStore(pool)), nil)
	ex, err := gw.ExchangeBody(context.Background(), scope, contract.HTTPRequestInput{URL: server + "/", Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if string(ex.Body) != body {
		t.Fatalf("execution body was shortened: %d/%d", len(ex.Body), len(body))
	}
	if len(ex.Receipt.ResponsePreview) >= len(body) {
		t.Fatal("test did not exercise shortened preview")
	}
	if ex.Headers["Content-Type"] != "text/plain" {
		t.Fatalf("headers: %v", ex.Headers)
	}
	if ex.Receipt.ResponseArtifactID == nil {
		t.Fatal("execution body has no captured source")
	}
}

func TestGatewayChineseResponsePersistsPreviewAndFullBody(t *testing.T) {
	pool := ma3Pool(t)
	// Place a Chinese rune across byte 512, with a discovery link beyond the
	// preview. Neither persistence nor later body readers may lose the page.
	body := "<html><body>" + strings.Repeat("a", previewLimit-len("<html><body>")-1) +
		strings.Repeat("中文新闻", 40) + `<a href="/news/details">详情</a></body></html>`
	server := httptestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	scope := seedRun(t, pool, fmt.Sprintf(`{"network_policy":{"template":"readonly","allowed_methods":["GET"],"origins":[{"scheme":"http","host":"127.0.0.1","port":"%s","path_mode":"same_origin","allow_private":true}]}}`, portOf(server)))
	store := NewStore(pool)
	gw := NewGateway(NewTransport(store), nil)
	ex, err := gw.ExchangeBody(context.Background(), scope, contract.HTTPRequestInput{URL: server + "/news", Method: "GET"})
	if err != nil {
		t.Fatal(err)
	}
	if ex.Receipt.StatusCode != http.StatusOK || ex.Receipt.Truncated || string(ex.Body) != body {
		t.Fatal("Chinese page must remain a successful, complete captured response")
	}
	saved, err := store.LookupScoped(context.Background(), scope, ex.Receipt.ReceiptID)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(saved.ResponsePreview) || len(saved.ResponsePreview) != previewLimit-1 || saved.ResponsePreview != body[:previewLimit-1] {
		t.Fatal("database preview must contain a complete UTF8 prefix within 512 bytes")
	}
	if saved.ResponseArtifactID == nil || !strings.Contains(string(ex.Body), `<a href="/news/details">详情</a>`) {
		t.Fatal("full response artifact and links beyond the preview must be preserved")
	}
}
