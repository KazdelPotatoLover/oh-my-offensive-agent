package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

func resourceRunID(w http.ResponseWriter, r *http.Request) (uuid.UUID, bool) {
	id, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, 400, "invalid run ID")
		return uuid.Nil, false
	}
	return id, true
}
func resourceError(w http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, service.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		status = 404
	}
	if errors.Is(err, service.ErrCrossRunBoundary) {
		status = 403
	}
	if errors.Is(err, service.ErrStaleEpoch) || errors.Is(err, service.ErrConflict) {
		status = 409
	}
	respondError(w, status, err.Error())
}

func (h *RunHandler) ListCapabilities(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	run, err := h.runService.Get(r.Context(), runID)
	if err != nil {
		resourceError(w, err)
		return
	}
	profiles, err := model.ProfilesFromInput(run.ExecutionInput)
	if err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, 200, map[string]any{"profiles": profiles, "tools": tools.CapabilityCatalog(tools.AdapterContext{RunID: runID, Service: h.runService}), "snapshot_version": "capabilities_v1"})
}
func (h *RunHandler) ListArtifacts(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	items, err := h.runService.ListArtifacts(r.Context(), runID, nil, offset, limit)
	if err != nil {
		resourceError(w, err)
		return
	}
	var total int
	if err := h.runService.Pool().QueryRow(r.Context(), `SELECT count(*) FROM artifacts WHERE run_id=$1`, runID).Scan(&total); err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, 200, map[string]any{"artifacts": items, "offset": offset, "total": total, "next_offset": offset + len(items), "has_more": offset+len(items) < total})
}
func (h *RunHandler) ReadArtifact(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "artifactID"))
	if err != nil {
		resourceError(w, err)
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	page, err := h.runService.ReadArtifact(r.Context(), runID, id, offset, limit)
	if err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, 200, page)
}
func (h *RunHandler) CreateArtifact(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	var in struct {
		WorkItemID    *uuid.UUID `json:"work_item_id"`
		Name, Content string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, service.MaxArtifactBytes+4096)).Decode(&in); err != nil {
		resourceError(w, err)
		return
	}
	a, err := h.runService.SaveArtifact(r.Context(), runID, in.WorkItemID, nil, nil, in.Name, "text/plain", in.Content)
	if err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, 201, a)
}
func (h *RunHandler) ListCheckpoints(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	items, err := h.runService.ListContextCheckpoints(r.Context(), runID)
	if err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, 200, map[string]any{"checkpoints": items})
}

func (h *RunHandler) ReviseExplorationNode(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	nodeID, err := uuid.Parse(chi.URLParam(r, "nodeID"))
	if err != nil {
		resourceError(w, err)
		return
	}
	var in struct {
		ExpectedVersion      int             `json:"expected_version"`
		Payload              json.RawMessage `json:"payload"`
		State, Reason, Actor string
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32768)).Decode(&in); err != nil {
		resourceError(w, err)
		return
	}
	if err := h.runService.ReviseExplorationNode(r.Context(), runID, nodeID, in.ExpectedVersion, in.Payload, in.State, in.Reason, in.Actor); err != nil {
		resourceError(w, err)
		return
	}
	if h.manager != nil {
		h.manager.Nudge(runID)
	}
	respondJSON(w, 200, map[string]any{"status": "revised"})
}

func (h *RunHandler) DeriveAsset(w http.ResponseWriter, r *http.Request) {
	runID, ok := resourceRunID(w, r)
	if !ok {
		return
	}
	var in struct {
		ParentID       uuid.UUID `json:"parent_asset_id"`
		Kind, Identity string
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		resourceError(w, err)
		return
	}
	out, err := h.runService.DeriveTypedAsset(r.Context(), runID, in.ParentID, in.Kind, in.Identity)
	if err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, 201, out)
}
