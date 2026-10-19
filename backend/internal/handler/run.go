package handler

import (
	"encoding/json"
	"errors"
	"net/http"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/worker"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type RunHandler struct {
	runService  *service.RunService
	taskService *service.TaskService
	manager     *worker.Manager
}

func NewRunHandler(runService *service.RunService, taskService *service.TaskService, manager *worker.Manager) *RunHandler {
	return &RunHandler{
		runService:  runService,
		taskService: taskService,
		manager:     manager,
	}
}

func (h *RunHandler) ListByTask(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "taskID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid task ID")
		return
	}

	// Verify task exists
	if _, err := h.taskService.Get(r.Context(), taskID); err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "task not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get task")
		return
	}

	runs, err := h.runService.ListByTask(r.Context(), taskID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to list runs")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"runs": runs})
}

func (h *RunHandler) Create(w http.ResponseWriter, r *http.Request) {
	taskID, err := uuid.Parse(chi.URLParam(r, "taskID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid task ID")
		return
	}

	var req model.CreateRunRequest
	// Body may be empty
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}

	// Get idempotency key from header
	var idempotencyKey *string
	if key := r.Header.Get("Idempotency-Key"); key != "" {
		idempotencyKey = &key
	}

	run, existed, err := h.runService.Create(r.Context(), taskID, req, idempotencyKey)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "task not found")
			return
		}
		if errors.Is(err, service.ErrAlreadyActive) {
			respondError(w, http.StatusConflict, "task already has an active run")
			return
		}
		if errors.Is(err, service.ErrConflict) {
			respondError(w, http.StatusConflict, "idempotency key conflict: same key but different request parameters")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create run")
		return
	}

	if existed {
		respondJSON(w, http.StatusOK, run)
	} else {
		// Start the worker
		h.manager.Start(run.ID, run.FailAtStep)
		respondJSON(w, http.StatusCreated, run)
	}
}

func (h *RunHandler) Get(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	run, err := h.runService.Get(r.Context(), runID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "run not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get run")
		return
	}
	respondJSON(w, http.StatusOK, run)
}

func (h *RunHandler) Stop(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	run, err := h.runService.Stop(r.Context(), runID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "run not found")
			return
		}
		if errors.Is(err, service.ErrTerminalState) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		if errors.Is(err, service.ErrInvalidTransition) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to stop run")
		return
	}

	// Signal worker to stop
	h.manager.Stop(runID)

	respondJSON(w, http.StatusOK, run)
}

func (h *RunHandler) GetEvents(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	events, err := h.runService.GetEvents(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get events")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"events": events})
}
