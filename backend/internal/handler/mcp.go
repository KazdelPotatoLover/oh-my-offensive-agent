package handler

import (
	"encoding/json"
	"net/http"
	"strconv"

	"agent-workbench/backend/internal/mcp"

	"github.com/go-chi/chi/v5"
)

type MCPHandler struct {
	manager *mcp.Manager
}

func NewMCPHandler(manager *mcp.Manager) *MCPHandler {
	return &MCPHandler{manager: manager}
}

func (h *MCPHandler) List(w http.ResponseWriter, r *http.Request) {
	servers, err := h.manager.ListServers(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

func (h *MCPHandler) Get(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid server id")
		return
	}

	server, err := h.manager.GetServer(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusNotFound, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, server)
}

func (h *MCPHandler) Create(w http.ResponseWriter, r *http.Request) {
	var req mcp.CreateServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	server, err := h.manager.CreateServer(r.Context(), req)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, server)
}

func (h *MCPHandler) Update(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid server id")
		return
	}

	var req mcp.UpdateServerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid json payload")
		return
	}

	server, err := h.manager.UpdateServer(r.Context(), id, req)
	if err != nil {
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, server)
}

func (h *MCPHandler) Delete(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid server id")
		return
	}

	if err := h.manager.DeleteServer(r.Context(), id); err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

func (h *MCPHandler) Discover(w http.ResponseWriter, r *http.Request) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid server id")
		return
	}

	tools, err := h.manager.DiscoverTools(r.Context(), id)
	if err != nil {
		respondError(w, http.StatusBadGateway, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"tools": tools})
}
