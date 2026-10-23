package handler

import (
	"net/http"

	"agent-workbench/backend/internal/tools"
)

// ToolHandler provides endpoints for querying built-in agent tools catalog.
type ToolHandler struct{}

func NewToolHandler() *ToolHandler {
	return &ToolHandler{}
}

// GetCatalog returns the catalog of all platform-native tools.
func (h *ToolHandler) GetCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := tools.GetNativeToolCatalog()
	respondJSON(w, http.StatusOK, map[string]any{"tools": catalog})
}
