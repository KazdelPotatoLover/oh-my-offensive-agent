package handler_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-workbench/backend/internal/handler"
	"agent-workbench/backend/internal/tools"
)

func TestToolHandler_GetCatalog(t *testing.T) {
	h := handler.NewToolHandler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tools/catalog", nil)

	h.GetCatalog(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected status 200, got %d", rec.Code)
	}

	var res struct {
		Tools []tools.ToolCatalogItem `json:"tools"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if len(res.Tools) == 0 {
		t.Fatalf("expected non-empty tools in response")
	}
}
