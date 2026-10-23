package handler

import (
	"net/http"

	"agent-workbench/backend/internal/config"
	"agent-workbench/backend/internal/provider"
)

// RuntimeDefaults exposes effective creation defaults, never model credentials.
func RuntimeDefaults(w http.ResponseWriter, r *http.Request) {
	budget, err := config.ExplorationBudget()
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, modelErr := provider.ConfigFromEnv().Normalized()
	respondJSON(w, http.StatusOK, map[string]any{
		"budget":           config.ManagedReserves(budget),
		"model_configured": modelErr == nil,
	})
}
