package handler

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"agent-workbench/backend/internal/agent"
	"agent-workbench/backend/internal/checker"
	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/controlplane"
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
	epochMu         sync.Mutex // serialize database epoch transitions and local executor registration
	runService      *service.RunService
	taskService     *service.TaskService
	manager         *worker.Manager
	resourceRevoker contract.ResourceRevoker
	controller      contract.ExecutionController
	supervision     *controlplane.Supervision
	sharedSteer     *orchestration.ToolBatchSupervisor
	modelRuntime    *provider.ProcessRuntime
}

func (h *RunHandler) ModelRuntime() *provider.ProcessRuntime { return h.modelRuntime }

// UseResourceRevoker attaches the capture revoker used by new engines. It does
// not change NewRunHandler and does not replace the job revoker owned by the manager.
func (h *RunHandler) UseResourceRevoker(revoker contract.ResourceRevoker) {
	if h == nil {
		return
	}
	h.resourceRevoker = revoker
}

// UseController attaches the one execution controller. It does not change NewRunHandler.
func (h *RunHandler) UseController(controller contract.ExecutionController) {
	if h == nil {
		return
	}
	h.controller = controller
}

// UseSupervision shares the planner tool factory with the controller.
func (h *RunHandler) UseSupervision(supervision *controlplane.Supervision) {
	if h == nil {
		return
	}
	h.supervision = supervision
}

// UseToolBatchSupervisor shares the pre-tool replan port with every new engine.
func (h *RunHandler) UseToolBatchSupervisor(supervisor *orchestration.ToolBatchSupervisor) {
	if h == nil {
		return
	}
	h.sharedSteer = supervisor
}

func NewRunHandler(runService *service.RunService, taskService *service.TaskService, manager *worker.Manager) *RunHandler {
	runtime := &provider.ProcessRuntime{}
	if manager != nil {
		runtime = manager.ModelRuntime()
	}
	return &RunHandler{
		runService:   runService,
		taskService:  taskService,
		manager:      manager,
		modelRuntime: runtime,
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

	var raw []byte
	if r.Body != nil {
		raw, err = io.ReadAll(r.Body)
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	var req model.CreateRunRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			respondError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
	}
	task, taskErr := h.taskService.Get(r.Context(), taskID)
	if taskErr != nil {
		if errors.Is(taskErr, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "task not found")
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to get task")
		return
	}
	present, requested := orchestration.WorkerFieldInJSON(raw)
	_, resolveErr := contract.ResolveRequestedWorkers(present, requested, taskIsExploration(task), contract.MaxParallelWorkers)
	if resolveErr != nil {
		var coded *contract.Error
		if errors.As(resolveErr, &coded) {
			respondJSON(w, coded.Code.DefaultHTTPStatus(), map[string]string{"error": coded.Message, "code": string(coded.Code)})
			return
		}
		respondError(w, http.StatusBadRequest, resolveErr.Error())
		return
	}
	// Keep omitted fields omitted: RunService resolves server → task → request
	// defaults and freezes the result. Never replace a task/global default here.

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
		if errors.Is(err, provider.ErrModelConfigMissing) || errors.Is(err, provider.ErrModelConfigInvalid) || errors.Is(err, model.ErrInvalidProfile) {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		var coded *contract.Error
		if errors.As(err, &coded) {
			respondJSON(w, coded.Status(), map[string]string{"error": coded.Message, "code": string(coded.Code)})
			return
		}
		respondError(w, http.StatusInternalServerError, "failed to create run: "+err.Error())
		return
	}

	if existed {
		respondJSON(w, http.StatusOK, run)
	} else {
		startErr := h.startRunExecutor(run)
		if startErr != nil {
			if errors.Is(startErr, provider.ErrModelConfigMissing) || errors.Is(startErr, provider.ErrModelConfigInvalid) {
				cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_ = h.runService.FinalizeRunWrapup(cleanupCtx, run.ID, model.TerminationCause("provider_config_error"), nil)
				cancel()
				respondError(w, http.StatusBadRequest, startErr.Error())
				return
			}
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

// startRunExecutor applies the immutable provider/budget snapshot for both create and epoch resume.
func (h *RunHandler) startRunExecutor(run *model.Run) error {
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

	if run.CurrentEpoch > 1 && executorType != "orchestrator" && executorType != "orchestration" {
		return fmt.Errorf("new execution epochs require an orchestrator run")
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
			return providerErr
		}

		workers, workerErr := orchestration.WorkersFromSnapshot(run.ExecutionInput)
		if workerErr != nil {
			return workerErr
		}
		budget.MaxParallelWorkers = workers
		limiter, tokens, recovery, modelErr := h.modelRuntime.Admission()
		if modelErr != nil {
			return modelErr
		}
		policy := model.PolicyFromExecutionInput(run.ExecutionInput)
		if executorType == "agent" {
			runnerOpts := []agent.AgentRunnerOption{agent.WithProcessAdmission(limiter, tokens, recovery)}
			if policy != nil && !policy.HasDeadline() {
				runnerOpts = append(runnerOpts, agent.WithNoRunDeadline())
			}
			runner := agent.NewAgentRunner(run.ID, h.runService, p, tools.DefaultRegistry(), budget, runnerOpts...)
			startErr = h.manager.StartExecutor(run.ID, runner)
		} else {
			// Discovery uses the same pool size as the accepted request.
			// Origin pacing is not a reason to force a serial pool.
			opts := orchestration.EngineOptionsFromAdmission(limiter, tokens, recovery)
			if h.resourceRevoker != nil {
				opts = append(opts, orchestration.WithResourceRevoker(h.resourceRevoker))
			}
			if h.sharedSteer != nil {
				opts = append(opts, orchestration.WithToolBatchSupervisor(h.sharedSteer))
			}
			if h.supervision != nil {
				opts = append(opts, orchestration.WithPlannerControlInstaller(supervisionInstaller{supervision: h.supervision}))
			}
			engine := orchestration.NewEngineWithPolicy(run.ID, h.runService, p, budget, policy, opts...)
			startErr = h.manager.StartExecutor(run.ID, engine)
		}
	} else {
		startErr = h.manager.Start(run.ID, run.FailAtStep)
	}

	return startErr
}

func taskIsExploration(task *model.Task) bool {
	if task == nil {
		return false
	}
	var scope map[string]any
	if len(task.Scope) > 0 && json.Unmarshal(task.Scope, &scope) == nil {
		if mode, _ := scope["mode"].(string); mode == "discovery" {
			return true
		}
	}
	var cfg map[string]any
	if len(task.Config) > 0 && json.Unmarshal(task.Config, &cfg) == nil {
		if exploration, _ := cfg["exploration"].(bool); exploration {
			return true
		}
	}
	return false
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
	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if _, ok := h.applyLegacy(w, r, runID, nil, contract.CmdStopRun, body, nil); !ok {
		return
	}
	h.legacyRun(w, r, runID)
}

// RecoverEpoch is the single epoch handoff used by the execution controller.
// It quiesces, starts the epoch, revokes the old epoch, and starts one executor.
func (h *RunHandler) RecoverEpoch(ctx context.Context, runID uuid.UUID, reason, summary string, expected int, key string) (int, bool, error) {
	if h == nil || h.manager == nil {
		return 0, false, &contract.Error{Code: contract.CodeExecutionUnavailable, Scope: contract.ScopeRun, Message: "executor manager unavailable", SuggestedAction: contract.ActionWait}
	}
	h.epochMu.Lock()
	defer h.epochMu.Unlock()
	drainCtx, cancelDrain := context.WithTimeout(ctx, 15*time.Second)
	err := h.manager.QuiesceEpoch(drainCtx, runID)
	cancelDrain()
	if err != nil {
		return 0, false, &contract.Error{Code: contract.CodeInvalidState, Scope: contract.ScopeEpoch, Message: "previous executor has not drained: " + err.Error(), SuggestedAction: contract.ActionWait}
	}
	epoch, err := h.runService.StartNewEpoch(ctx, runID, reason, summary, service.EpochStartOptions{ExpectedEpoch: expected, IdempotencyKey: key})
	if err != nil {
		return 0, false, err
	}
	h.manager.RevokeEpoch(runID, epoch-1)
	run, err := h.runService.Get(ctx, runID)
	if err != nil {
		return epoch, false, err
	}
	if err := h.startRunExecutor(run); err != nil {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.runService.FinalizeRunWrapupWithEpoch(cleanupCtx, runID, model.CauseInternalError, nil, epoch)
		return epoch, false, &contract.Error{Code: contract.CodeExecutionUnavailable, Scope: contract.ScopeEpoch, Message: "new epoch executor could not start: " + err.Error(), SuggestedAction: contract.ActionWait}
	}
	return epoch, true, nil
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

func (h *RunHandler) ContinueWorkItem(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	workItemID, err := uuid.Parse(chi.URLParam(r, "workItemID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid work item ID")
		return
	}

	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if _, ok := h.applyLegacy(w, r, runID, &workItemID, contract.CmdContinueWork, body, textPayload("instruction", body.Reason)); !ok {
		return
	}
	h.legacyWorkItem(w, r, workItemID)
}

func (h *RunHandler) RetryWorkItem(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	workItemID, err := uuid.Parse(chi.URLParam(r, "workItemID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid work item ID")
		return
	}

	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if _, ok := h.applyLegacy(w, r, runID, &workItemID, contract.CmdRetryWork, body, textPayload("instruction", body.Reason)); !ok {
		return
	}
	h.legacyWorkItem(w, r, workItemID)
}

func (h *RunHandler) ListPlannerTodos(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	todos, err := h.runService.ListPlannerTodos(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"planner_todos": todos})
}

func (h *RunHandler) AddOrUpdatePlannerTodo(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	var req struct {
		ItemKey     string   `json:"item_key"`
		Description string   `json:"description"`
		Status      string   `json:"status"`
		DependsOn   []string `json:"depends_on"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	todo, err := h.runService.AddOrUpdatePlannerTodo(r.Context(), runID, req.ItemKey, req.Description, req.Status, req.DependsOn)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	respondJSON(w, http.StatusOK, map[string]interface{}{"planner_todo": todo})
}

func (h *RunHandler) ListGoals(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	goals, err := h.runService.ListGoals(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"goals": goals})
}

func (h *RunHandler) SetGoals(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	var req struct {
		Goals []model.GoalSpec `json:"goals"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	nodes, err := h.runService.SetGoals(r.Context(), runID, req.Goals)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"goals": nodes})
}

func (h *RunHandler) ProveGoal(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	goalID, err := uuid.Parse(chi.URLParam(r, "goalID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid goal ID")
		return
	}
	var req struct {
		EvidenceID string `json:"evidence_id"`
		Reason     string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	evidenceID, err := uuid.Parse(strings.TrimSpace(req.EvidenceID))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid evidence_id")
		return
	}
	allMet, err := h.runService.ProveGoal(r.Context(), runID, goalID, evidenceID, req.Reason)
	if err != nil {
		if errors.Is(err, service.ErrCrossRunBoundary) {
			respondError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, service.ErrNodeNotFound) || errors.Is(err, service.ErrInvalidEvidence) {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{
		"status":        "proved",
		"goal_id":       goalID,
		"evidence_id":   evidenceID,
		"all_goals_met": allMet,
	})
}

func (h *RunHandler) GraphOverview(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	overview, err := h.runService.GraphOverview(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	offset, _ := strconv.Atoi(r.URL.Query().Get("offset"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	nodes, total, pageErr := h.runService.GraphNodePage(r.Context(), runID, offset, limit)
	if pageErr != nil {
		resourceError(w, pageErr)
		return
	}
	overview["nodes"], overview["total_nodes"], overview["offset"], overview["next_offset"], overview["has_more"] = nodes, total, offset, offset+len(nodes), offset+len(nodes) < total
	respondJSON(w, http.StatusOK, overview)
}

func (h *RunHandler) GetExplorationNodeDetail(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	nodeID, err := uuid.Parse(chi.URLParam(r, "nodeID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid node ID")
		return
	}
	detail, err := h.runService.GetExplorationNodeDetail(r.Context(), runID, nodeID)
	if err != nil {
		if errors.Is(err, service.ErrNodeNotFound) {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		if errors.Is(err, service.ErrCrossRunBoundary) {
			respondError(w, http.StatusForbidden, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, detail)
}

func (h *RunHandler) ListFacts(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	facts, err := h.runService.ListFacts(r.Context(), runID, 100)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"facts": facts})
}

func (h *RunHandler) RecordFact(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	var req struct {
		Summary    string   `json:"summary"`
		Detail     string   `json:"detail"`
		Confidence string   `json:"confidence"`
		Evidence   string   `json:"evidence"`
		IntentID   string   `json:"intent_id"`
		AssetIDs   []string `json:"asset_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var intentUUID *uuid.UUID
	if strings.TrimSpace(req.IntentID) != "" {
		if u, err := uuid.Parse(strings.TrimSpace(req.IntentID)); err == nil {
			intentUUID = &u
		}
	}
	var assetUUIDs []uuid.UUID
	for _, s := range req.AssetIDs {
		u, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid asset_id")
			return
		}
		assetUUIDs = append(assetUUIDs, u)
	}
	node, err := h.runService.RecordFact(r.Context(), runID, req.Summary, req.Detail, req.Confidence, req.Evidence, intentUUID, assetUUIDs)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"fact": node})
}

func (h *RunHandler) RecordHint(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	var req struct {
		Text     string   `json:"text"`
		AssetIDs []string `json:"asset_ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	var assetUUIDs []uuid.UUID
	for _, s := range req.AssetIDs {
		u, err := uuid.Parse(strings.TrimSpace(s))
		if err != nil {
			respondError(w, http.StatusBadRequest, "invalid asset_id")
			return
		}
		assetUUIDs = append(assetUUIDs, u)
	}
	node, err := h.runService.RecordHint(r.Context(), runID, req.Text, assetUUIDs)
	if err != nil {
		resourceError(w, err)
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"hint": node})
}

func (h *RunHandler) AddAssetRelation(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceAssetID string          `json:"source_asset_id"`
		TargetAssetID string          `json:"target_asset_id"`
		RelationType  string          `json:"relation_type"`
		Metadata      json.RawMessage `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	srcID, err := uuid.Parse(req.SourceAssetID)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid source_asset_id")
		return
	}
	dstID, err := uuid.Parse(req.TargetAssetID)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid target_asset_id")
		return
	}
	rel, err := h.runService.AddAssetRelation(r.Context(), srcID, dstID, req.RelationType, req.Metadata)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, map[string]interface{}{"relation": rel})
}

func (h *RunHandler) ListAssetRelations(w http.ResponseWriter, r *http.Request) {
	assetID, err := uuid.Parse(chi.URLParam(r, "assetID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid asset ID")
		return
	}
	relations, err := h.runService.ListAssetRelations(r.Context(), assetID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"relations": relations})
}

func (h *RunHandler) ListWorkerTraces(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	q := r.URL.Query().Get("q")
	traces, err := h.runService.ListWorkerTraces(r.Context(), runID, q, 50)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"traces": traces})
}

func (h *RunHandler) SearchAllWorkerTraces(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		respondError(w, http.StatusBadRequest, "query parameter q is required")
		return
	}
	var excludeWorkItemID *uuid.UUID
	if exStr := r.URL.Query().Get("exclude_work_item_id"); exStr != "" {
		if u, err := uuid.Parse(exStr); err == nil {
			excludeWorkItemID = &u
		}
	}
	hits, err := h.runService.SearchAllWorkerTraces(r.Context(), runID, excludeWorkItemID, q, 50)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]interface{}{"query": q, "hits": hits})
}

func (h *RunHandler) GetWorkerTrace(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	targetID, err := uuid.Parse(chi.URLParam(r, "workItemID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid work item ID")
		return
	}
	q := r.URL.Query().Get("q")
	var stepIDs []uuid.UUID
	if stepIDsStr := r.URL.Query().Get("step_ids"); stepIDsStr != "" {
		for _, s := range strings.Split(stepIDsStr, ",") {
			if u, err := uuid.Parse(strings.TrimSpace(s)); err == nil {
				stepIDs = append(stepIDs, u)
			}
		}
	}
	res, err := h.runService.GetWorkerTrace(r.Context(), runID, targetID, q, stepIDs, 50)
	if err != nil {
		if errors.Is(err, service.ErrCrossRunBoundary) {
			respondError(w, http.StatusForbidden, err.Error())
			return
		}
		if errors.Is(err, service.ErrNodeNotFound) {
			respondError(w, http.StatusNotFound, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, res)
}

func (h *RunHandler) PauseRun(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if _, ok := h.applyLegacy(w, r, runID, nil, contract.CmdPauseRun, body, nil); !ok {
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "paused"})
}

func (h *RunHandler) ResumeRun(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if _, ok := h.applyLegacy(w, r, runID, nil, contract.CmdResumeRun, body, nil); !ok {
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "running"})
}

func (h *RunHandler) SteerWorkItem(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	workItemID, err := uuid.Parse(chi.URLParam(r, "workItemID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid work item ID")
		return
	}

	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	if rejectSteerOverride(w, body.InputDataOverride) {
		return
	}
	if _, ok := h.applyLegacy(w, r, runID, &workItemID, contract.CmdSteerWork, body, textPayload("instruction", body.Instructions)); !ok {
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "steered"})
}

func (h *RunHandler) KillWorkItem(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}
	workItemID, err := uuid.Parse(chi.URLParam(r, "workItemID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid work item ID")
		return
	}

	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	payload := textPayload("reason", body.Reason)
	if strings.TrimSpace(body.Reason) == "" {
		payload = nil
	}
	if _, ok := h.applyLegacy(w, r, runID, &workItemID, contract.CmdKillWork, body, payload); !ok {
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "cancelled"})
}

func (h *RunHandler) GrantBudget(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}
	result, ok := h.applyLegacy(w, r, runID, nil, contract.CmdGrantBudget, body, grantPayload(body))
	if !ok {
		return
	}
	h.legacyGrant(w, r, runID, result)
}

func (h *RunHandler) ListBudgetGrants(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	grants, err := h.runService.ListBudgetGrants(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

func (h *RunHandler) GetEffectiveBudget(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	budget, err := h.runService.GetEffectiveBudget(r.Context(), runID)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "run not found")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	payload := map[string]any{}
	if raw, marshalErr := json.Marshal(budget); marshalErr == nil {
		_ = json.Unmarshal(raw, &payload)
	}
	if run, getErr := h.runService.Get(r.Context(), runID); getErr == nil {
		if policy := model.PolicyFromExecutionInput(run.ExecutionInput); policy != nil {
			payload["limits"] = policy.PublicLimits()
		}
	}
	respondJSON(w, http.StatusOK, payload)
}

func (h *RunHandler) StartNewEpoch(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	body, err := h.readLegacyControl(r)
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON")
		return
	}
	result, ok := h.applyLegacy(w, r, runID, nil, contract.CmdStartEpoch, body, epochPayload(body))
	if !ok {
		return
	}
	epoch := body.ExpectedEpoch
	if result != nil && result.EpochNumber > 0 {
		epoch = result.EpochNumber
	}
	respondJSON(w, http.StatusCreated, map[string]int{"epoch_number": epoch})
}

func (h *RunHandler) ListExecutionEpochs(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	epochs, err := h.runService.ListExecutionEpochs(r.Context(), runID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"epochs": epochs})
}

func (h *RunHandler) ReportFinding(w http.ResponseWriter, r *http.Request) {
	runID, err := uuid.Parse(chi.URLParam(r, "runID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid run ID")
		return
	}

	var req service.ReportFindingInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	f, err := h.runService.ReportFinding(r.Context(), runID, req)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "run or asset not found")
			return
		}
		respondError(w, http.StatusBadRequest, err.Error())
		return
	}
	respondJSON(w, http.StatusCreated, f)
}

func (h *RunHandler) CreateFindingRetest(w http.ResponseWriter, r *http.Request) {
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

	var req struct {
		Notes string `json:"notes"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	retest, sess, err := h.runService.CreateFindingRetest(r.Context(), runID, findingID, req.Notes)
	if err != nil {
		if errors.Is(err, service.ErrNotFound) {
			respondError(w, http.StatusNotFound, "finding not found")
			return
		}
		if errors.Is(err, service.ErrRetestActiveConflict) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if h.manager != nil {
		run, runErr := h.runService.Get(r.Context(), runID)
		if runErr == nil {
			var inputMap map[string]interface{}
			if len(run.ExecutionInput) > 0 {
				_ = json.Unmarshal(run.ExecutionInput, &inputMap)
			}
			var agentCfg map[string]interface{}
			if ac, ok := inputMap["agent_config"].(map[string]interface{}); ok {
				agentCfg = ac
			} else if cfgMap, ok := inputMap["config"].(map[string]interface{}); ok {
				if ac, ok := cfgMap["agent_config"].(map[string]interface{}); ok {
					agentCfg = ac
				}
			}
			p, _ := provider.FromSnapshot(agentCfg, provider.ScenarioOrchestration)
			reg := tools.NewRegistry()
			reg.Register(tools.NewGetEndpointTool(runID, uuid.Nil, h.runService))
			if err := h.manager.StartRetest(runID, findingID, retest.ID, p, reg); err != nil {
				slog.Error("failed to start retest executor", "run_id", runID, "finding_id", findingID, "error", err)
			}
		}
	}

	respondJSON(w, http.StatusCreated, map[string]any{
		"retest":  retest,
		"session": sess,
	})
}

func (h *RunHandler) FinishFindingRetest(w http.ResponseWriter, r *http.Request) {
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
	retestID, err := uuid.Parse(chi.URLParam(r, "retestID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid retest ID")
		return
	}

	var req struct {
		Verdict  string `json:"verdict"`
		Evidence string `json:"evidence"`
		Summary  string `json:"summary"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, "invalid JSON payload")
		return
	}

	retest, err := h.runService.FinishFindingRetest(r.Context(), runID, findingID, retestID, req.Verdict, req.Evidence, req.Summary)
	if err != nil {
		if errors.Is(err, service.ErrRetestNotFound) {
			respondError(w, http.StatusNotFound, "retest not found")
			return
		}
		if errors.Is(err, service.ErrInvalidVerdict) || errors.Is(err, service.ErrInvalidEvidence) {
			respondError(w, http.StatusBadRequest, err.Error())
			return
		}
		if errors.Is(err, service.ErrInvalidState) {
			respondError(w, http.StatusConflict, err.Error())
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, retest)
}

func (h *RunHandler) StopFindingRetest(w http.ResponseWriter, r *http.Request) {
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
	retestID, err := uuid.Parse(chi.URLParam(r, "retestID"))
	if err != nil {
		respondError(w, http.StatusBadRequest, "invalid retest ID")
		return
	}

	var req struct {
		Reason string `json:"reason"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}

	if err := h.runService.StopFindingRetest(r.Context(), runID, findingID, retestID, req.Reason); err != nil {
		if errors.Is(err, service.ErrRetestNotFound) {
			respondError(w, http.StatusNotFound, "retest not found")
			return
		}
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]string{"status": "stopped"})
}

func (h *RunHandler) ListFindingRetests(w http.ResponseWriter, r *http.Request) {
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

	retests, err := h.runService.ListFindingRetests(r.Context(), runID, findingID)
	if err != nil {
		respondError(w, http.StatusInternalServerError, err.Error())
		return
	}
	respondJSON(w, http.StatusOK, map[string]any{"retests": retests})
}
