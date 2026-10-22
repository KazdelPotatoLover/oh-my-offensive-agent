package netcap

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

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
