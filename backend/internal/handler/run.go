package handler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/checker"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/orchestration"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"
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
		if errors.Is(err, provider.ErrModelConfigMissing) || errors.Is(err, provider.ErrModelConfigInvalid) {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}

	if existed {
		respondJSON(w, http.StatusOK, run)
	} else {
		// Determine executor (AgentRunner vs FakeWorker)
		var startErr error
		var inputMap map[string]interface{}
		if len(run.ExecutionInput) > 0 {
			json.Unmarshal(run.ExecutionInput, &inputMap)
		}

		executorType := "fake"
		var agentCfg map[string]interface{}
		if ac, ok := inputMap["agent_config"].(map[string]interface{}); ok {
			agentCfg = ac
		} else if cfgMap, ok := inputMap["config"].(map[string]interface{}); ok {
			if ac, ok := cfgMap["agent_config"].(map[string]interface{}); ok {
				agentCfg = ac
			}
		}
		if agentCfg != nil {
			if ex, ok := agentCfg["executor"].(string); ok && ex != "" {
				executorType = ex
			}
		}

		scopeMap, _ := inputMap["scope"].(map[string]interface{})
		isDiscovery := (scopeMap != nil && scopeMap["mode"] == "discovery") || inputMap["discovery_policy"] != nil
		if isDiscovery && executorType == "fake" {
			executorType = "orchestrator"
		}

		var bMap map[string]interface{}
		if b, ok := inputMap["budget"].(map[string]interface{}); ok {
			bMap = b
		} else if cfgMap, ok := inputMap["config"].(map[string]interface{}); ok {
			if b, ok := cfgMap["budget"].(map[string]interface{}); ok {
				bMap = b
			}
		}

		if executorType == "agent" || executorType == "orchestrator" || executorType == "orchestration" {
			budget := model.DefaultBudgetConfig()
			if bMap != nil {
				bBytes, _ := json.Marshal(bMap)
				json.Unmarshal(bBytes, &budget)
			}
			scenario := provider.MockScenario("normal")
			if executorType != "agent" {
				scenario = provider.ScenarioOrchestration
				if isDiscovery {
					scenario = provider.ScenarioDiscovery
				}
			}
			p, providerErr := provider.FromSnapshot(agentCfg, scenario)
			if providerErr != nil {
				reason := "provider_config_error"
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if err := h.runService.FinalizeRunWrapup(cleanupCtx, run.ID, model.TerminationCause(reason), nil); err != nil {
					slog.Error("failed to finalize provider configuration error", "run_id", run.ID, "error", err)
				}
				cancel()
				respondError(w, http.StatusBadRequest, providerErr.Error())
				return
			}
			if executorType == "agent" {
				runner := agent.NewAgentRunner(run.ID, h.runService, p, tools.DefaultRegistry(), budget)
				startErr = h.manager.StartExecutor(run.ID, runner)
			} else {
				// Discovery is sequential. Every provider, including mock, must
				// honor the original recorded model/tool hard caps.
				if isDiscovery {
					budget.MaxParallelWorkers = 1
				}
				engine := orchestration.NewEngine(run.ID, h.runService, p, budget)
				startErr = h.manager.StartExecutor(run.ID, engine)
			}
		} else {
			startErr = h.manager.Start(run.ID, run.FailAtStep)
		}

		if startErr != nil {
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := h.runService.FinalizeRunWrapup(cleanupCtx, run.ID, model.CauseShutdown, nil); err != nil {
				slog.Error("failed to finalize rejected executor", "run_id", run.ID, "error", err)
			}
			cancel()
			respondError(w, http.StatusServiceUnavailable, "service is shutting down")
			return
		}
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
	if err := h.manager.StopAndFinalizeIfIdle(r.Context(), runID); err != nil {
		respondError(w, http.StatusInternalServerError, "failed to finalize run stop")
		return
	}
	if latest, getErr := h.runService.Get(r.Context(), runID); getErr == nil {
		run = latest
	}

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

func (h *RunHandler) GetMessages(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	messages, err := h.runService.ListAgentMessages(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get messages")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"messages": messages})
}

func (h *RunHandler) GetModelCalls(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	calls, err := h.runService.ListModelCalls(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get model calls")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"model_calls": calls})
}

func (h *RunHandler) GetToolCalls(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	toolCalls, err := h.runService.ListAgentToolCalls(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get tool calls")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"tool_calls": toolCalls})
}

func (h *RunHandler) GetWorkItems(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	items, err := h.runService.ListWorkItemsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get work items")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"work_items": items})
}

func (h *RunHandler) GetObservations(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	obs, err := h.runService.ListObservationsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get observations")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"observations": obs})
}

func (h *RunHandler) GetSessions(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	sessions, err := h.runService.ListSessionsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get sessions")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"sessions": sessions})
}

func (h *RunHandler) GetPlannerRounds(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	rounds, err := h.runService.ListPlannerRounds(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get planner rounds")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"planner_rounds": rounds})
}

func (h *RunHandler) GetRunAssets(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	assets, err := h.runService.ListAssetsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get run assets")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"assets": assets})
}

func (h *RunHandler) GetWorkItemDetails(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	items, err := h.runService.ListWorkItemDetailsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get work item details")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"work_items": items})
}

func (h *RunHandler) GetDiscoveries(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	discoveries, err := h.runService.ListDiscoveriesByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get discoveries")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"discoveries": discoveries})
}

func (h *RunHandler) GetCheckResults(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	results, err := h.runService.ListCheckResultsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get check results")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"check_results": results})
}

func (h *RunHandler) GetFindings(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	findings, err := h.runService.ListFindingsByRun(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, "failed to get findings")
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"findings": findings})
}

func (h *RunHandler) GetCheckCatalog(w http.ResponseWriter, r *http.Request) {
	catalog := checker.GetCatalog()
	respondJSON(w, http.StatusOK, map[string]interface{}{"checks": catalog})
}

type CreateFindingReviewRequest struct {
	Status          model.FindingStatus `json:"status"`
	Comment         string              `json:"comment"`
	Reviewer        string              `json:"reviewer"`
	ExpectedVersion *int                `json:"expected_version"`
	RequestID       *uuid.UUID          `json:"request_id"`
}

func (h *RunHandler) CreateFindingReview(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	findingID, err := uuid.Parse(chi.URLParam(r, "findingID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid finding ID")
		return
	}

	// Limit request body size to 32 KiB
	const maxBodyBytes = 32 * 1024
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req CreateFindingReviewRequest
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&req); err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			respondError(w, http.StatusRequestEntityTooLarge, "request body exceeds 32KB limit")
			return
		}
		respondError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}

	if req.ExpectedVersion == nil {
		respondError(w, http.StatusBadRequest, "expected_version is required")
		return
	}
	if *req.ExpectedVersion < 0 {
		respondError(w, http.StatusBadRequest, "expected_version cannot be negative")
		return
	}
	if req.RequestID == nil || *req.RequestID == uuid.Nil {
		respondError(w, http.StatusBadRequest, "request_id is required")
		return
	}
	reviewer := strings.TrimSpace(req.Reviewer)
	if reviewer == "" {
		respondError(w, http.StatusBadRequest, "reviewer is required")
		return
	}
	if utf8.RuneCountInString(reviewer) > 100 {
		respondError(w, http.StatusBadRequest, "reviewer name exceeds 100 characters")
		return
	}
	comment := strings.TrimSpace(req.Comment)
	if comment == "" {
		respondError(w, http.StatusBadRequest, "comment is required")
		return
	}
	if utf8.RuneCountInString(comment) > 2000 {
		respondError(w, http.StatusBadRequest, "review comment exceeds 2000 characters")
		return
	}
	if !req.Status.IsValid() {
		respondError(w, http.StatusBadRequest, "invalid status")
		return
	}

	finding, review, replayed, err := h.runService.ReviewFinding(r.Context(), runID, findingID, service.ReviewFindingInput{
		Status:          req.Status,
		Comment:         req.Comment,
		Reviewer:        req.Reviewer,
		ExpectedVersion: req.ExpectedVersion,
		RequestID:       *req.RequestID,
	})
	if err != nil {
		if errors.Is(err, service.ErrFindingNotFound) {
			respondError(w, http.StatusNotFound, "finding not found in run")
			return
		}
		if errors.Is(err, service.ErrFindingMissingVersion) ||
			errors.Is(err, service.ErrFindingNegativeVersion) ||
			errors.Is(err, service.ErrFindingMissingRequestID) ||
			errors.Is(err, service.ErrFindingEmptyReviewer) ||
			errors.Is(err, service.ErrReviewerTooLong) ||
			errors.Is(err, service.ErrFindingEmptyComment) ||
			errors.Is(err, service.ErrFindingCommentTooLong) ||
			errors.Is(err, service.ErrFindingInvalidStatus) {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, service.ErrFindingVersionMismatch) ||
			errors.Is(err, service.ErrFindingSameStatus) ||
			errors.Is(err, service.ErrFindingIdempotentConflict) {
			respondJSON(w, http.StatusConflict, map[string]interface{}{
				"error": err.Error(),
			})
			return
		}
		slog.Error("failed to review finding", "error", err, "run_id", runID, "finding_id", findingID)
		respondError(w, http.StatusInternalServerError, "failed to review finding")
		return
	}

	statusCode := http.StatusOK
	if !replayed {
		statusCode = http.StatusCreated
	}
	respondJSON(w, statusCode, map[string]interface{}{
		"finding":  finding,
		"review":   review,
		"replayed": replayed,
	})
}

func (h *RunHandler) ListFindingReviews(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	findingID, err := uuid.Parse(chi.URLParam(r, "findingID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid finding ID")
		return
	}

	reviews, err := h.runService.ListFindingReviews(r.Context(), runID, findingID)
	if err != nil {
		if errors.Is(err, service.ErrFindingNotFound) {
			respondError(w, http.StatusNotFound, "finding not found in run")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to list finding reviews")
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"reviews": reviews})
}

func (h *RunHandler) GetFindingReport(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	report, err := h.runService.GetFindingReport(r.Context(), runID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "run not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get finding report")
		return
	}

	respondJSON(w, http.StatusOK, report)
}
