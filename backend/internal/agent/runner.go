package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

type AgentRunnerOption func(*AgentRunner)

func WithOnModelCallHook(hook func(round int)) AgentRunnerOption {
	return func(r *AgentRunner) {
		r.onModelCallHook = hook
	}
}

func WithOnToolCallHook(hook func(callID string, toolName string)) AgentRunnerOption {
	return func(r *AgentRunner) {
		r.onToolCallHook = hook
	}
}

type AgentRunner struct {
	runID           uuid.UUID
	runService      *service.RunService
	provider        provider.Provider
	registry        *tools.Registry
	budget          model.BudgetConfig
	onModelCallHook func(round int)
	onToolCallHook  func(callID string, toolName string)
}

func NewAgentRunner(
	runID uuid.UUID,
	runService *service.RunService,
	p provider.Provider,
	reg *tools.Registry,
	budget model.BudgetConfig,
	opts ...AgentRunnerOption,
) *AgentRunner {
	if reg == nil {
		reg = tools.DefaultRegistry()
	}
	runner := &AgentRunner{
		runID:      runID,
		runService: runService,
		provider:   p,
		registry:   reg,
		budget:     budget,
	}
	for _, opt := range opts {
		opt(runner)
	}
	return runner
}

func (r *AgentRunner) Run(ctx context.Context) {
	// 1. Ensure stopping state is properly finalized upon return
	defer r.ensureStoppingTerminal()

	// 2. Read Run input snapshot
	run, err := r.runService.Get(ctx, r.runID)
	if err != nil {
		slog.Error("agent runner: failed to get run", "run_id", r.runID, "error", err)
		return
	}

	// If stop was requested before worker started
	if run.Status == model.RunStatusStopping {
		reason := "user_requested_stop"
		r.runService.TransitionState(context.Background(), r.runID, model.RunStatusStopping, model.RunStatusStopped, &reason)
		return
	}

	// 3. Transition from queued to running
	if run.Status == model.RunStatusQueued {
		_, err := r.runService.TransitionState(ctx, r.runID, model.RunStatusQueued, model.RunStatusRunning, nil)
		if err != nil {
			slog.Error("agent runner: failed to transition to running", "run_id", r.runID, "error", err)
			return
		}
	}

	// Parse execution input snapshot
	var inputMap map[string]interface{}
	if len(run.ExecutionInput) > 0 {
		json.Unmarshal(run.ExecutionInput, &inputMap)
	}

	objective, _ := inputMap["objective"].(string)
	if objective == "" {
		objective = "Read all available HTTP response samples and generate a structured summary report."
	}
	scopeBytes, _ := json.Marshal(inputMap["scope"])

	// 4. Create Session in DB
	sess, err := r.runService.CreateSession(ctx, r.runID, "legacy_agent", nil, nil, nil, run.ExecutionInput)
	if err != nil {
		slog.Error("agent runner: failed to create session", "run_id", r.runID, "error", err)
		reason := "persistence_error"
		r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	// 5. Initialize conversation messages
	systemPrompt := "You are a cybersecurity research agent. Your task is to investigate and analyze HTTP response samples. " +
		"Read the samples using available tools, extract their HTTP status codes, titles, and headers, " +
		"and produce a concise summary report distinguishing between actual observations and speculations."

	userContent := fmt.Sprintf("Objective: %s\nScope: %s", objective, string(scopeBytes))

	initialMessages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: systemPrompt},
		{Role: provider.RoleUser, Content: userContent},
	}

	// Overall execution context bounded by run_timeout_seconds
	runTimeout := time.Duration(r.budget.RunTimeoutSeconds) * time.Second
	runCtx, cancelRun := context.WithTimeout(ctx, runTimeout)
	defer cancelRun()

	var srOpts []SessionRunnerOption
	if r.onModelCallHook != nil {
		srOpts = append(srOpts, WithSessionModelCallHook(r.onModelCallHook))
	}
	if r.onToolCallHook != nil {
		srOpts = append(srOpts, WithSessionToolCallHook(r.onToolCallHook))
	}

	sr := NewSessionRunner(sess.ID, r.runID, "legacy_agent", r.runService, r.provider, r.registry, r.budget, srOpts...)
	res, resErr := sr.Run(runCtx, initialMessages)

	if runCtx.Err() != nil {
		r.handleCancellation(runCtx)
		return
	}

	if resErr != nil {
		slog.Error("agent runner: session execution error", "run_id", r.runID, "error", resErr)
		reason := "provider_error"
		if errors.Is(resErr, provider.ErrProtocol) {
			reason = "protocol_error"
		}
		if errors.Is(resErr, ErrPersistenceFailure) {
			reason = "persistence_error"
		}
		r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	if res == nil {
		reason := "failed"
		r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
		return
	}

	if res.FinishReason == "completed" && res.FinalContent != "" {
		reason := "final_answer"
		_, err = r.runService.CompleteRunWithOutput(context.Background(), r.runID, reason, res.FinalContent)
		if err != nil {
			slog.Error("agent runner: failed to complete run with output", "run_id", r.runID, "error", err)
		}
		return
	}

	finishReason := res.FinishReason
	if finishReason == "completed" {
		finishReason = "final_answer"
	}
	r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &finishReason)
}

func (r *AgentRunner) handleCancellation(ctx context.Context) {
	cleanupCtx := context.Background()
	run, err := r.runService.Get(cleanupCtx, r.runID)
	if err != nil {
		slog.Error("agent runner: failed to get run during cancellation", "run_id", r.runID, "error", err)
		return
	}

	if run.Status.IsTerminal() {
		return
	}

	if run.Status == model.RunStatusStopping {
		reason := "user_requested_stop"
		r.runService.TransitionState(cleanupCtx, r.runID, model.RunStatusStopping, model.RunStatusStopped, &reason)
		return
	}

	// Deadline / timeout exceeded
	if ctx.Err() == context.DeadlineExceeded {
		reason := "deadline_exceeded"
		r.runService.TransitionState(cleanupCtx, r.runID, run.Status, model.RunStatusFailed, &reason)
		return
	}

	reason := "user requested stop"
	r.runService.TransitionState(cleanupCtx, r.runID, run.Status, model.RunStatusStopped, &reason)
}

func (r *AgentRunner) ensureStoppingTerminal() {
	cleanupCtx := context.Background()
	run, err := r.runService.Get(cleanupCtx, r.runID)
	if err != nil {
		return
	}
	if run.Status == model.RunStatusStopping {
		reason := "user_requested_stop"
		r.runService.TransitionState(cleanupCtx, r.runID, model.RunStatusStopping, model.RunStatusStopped, &reason)
		slog.Info("agent runner: ensureStoppingTerminal transitioned stopping to stopped", "run_id", r.runID)
	}
}
