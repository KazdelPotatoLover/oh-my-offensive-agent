package handler

import (
	"errors"
	"net/http"

	"agent-workbench/backend/internal/service"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type AssetHandler struct {
	runService *service.RunService
}

func NewAssetHandler(runService *service.RunService) *AssetHandler {
	return &AssetHandler{runService: runService}
}

func (h *AssetHandler) List(w http.ResponseWriter, r *http.Request) {
	assets, err := h.runService.ListAssets(r.Context())
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list assets")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"assets": assets})
}

func (h *AssetHandler) Get(w http.ResponseWriter, r *http.Request) {
	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid asset ID")
		return
	}

	asset, err := h.runService.GetAsset(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "asset not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get asset")
		return
	}
	respondJSON(w, http.StatusOK, asset)
}

func (h *AssetHandler) GetDetails(w http.ResponseWriter, r *http.Request) {
	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid asset ID")
		return
	}

	details, err := h.runService.GetAssetDetailsWithHistory(r.Context(), assetID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "asset not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get asset details")
		return
	}
	respondJSON(w, http.StatusOK, details)
}
