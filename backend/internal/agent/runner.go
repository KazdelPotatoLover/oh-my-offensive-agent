package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
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

	// 4. Initialize conversation messages
	systemPrompt := "You are a cybersecurity research agent. Your task is to investigate and analyze HTTP response samples. " +
		"Read the samples using available tools, extract their HTTP status codes, titles, and headers, " +
		"and produce a concise summary report distinguishing between actual observations and speculations."

	userContent := fmt.Sprintf("Objective: %s\nScope: %s", objective, string(scopeBytes))

	// Persist initial messages
	r.runService.InsertAgentMessage(context.Background(), r.runID, "system", systemPrompt, nil, nil)
	r.runService.InsertAgentMessage(context.Background(), r.runID, "user", userContent, nil, nil)

	messages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: systemPrompt},
		{Role: provider.RoleUser, Content: userContent},
	}

	// Overall execution context bounded by run_timeout_seconds
	runTimeout := time.Duration(r.budget.RunTimeoutSeconds) * time.Second
	runCtx, cancelRun := context.WithTimeout(ctx, runTimeout)
	defer cancelRun()

	modelCallCount := 0
	toolCallCount := 0

	// 5. Main Turn Loop
	for {
		// Pre-check cancellation / stop / timeout
		select {
		case <-runCtx.Done():
			r.handleCancellation(runCtx)
			return
		default:
		}

		// Check model call budget limit
		if modelCallCount >= r.budget.MaxModelCalls {
			reason := "max_model_calls_exceeded"
			r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		// Record model call start in DB
		round := modelCallCount + 1
		modelCallRec, err := r.runService.CreateModelCall(context.Background(), r.runID, round)
		if err != nil {
			slog.Error("agent runner: failed to create model call", "run_id", r.runID, "error", err)
			return
		}

		r.insertEvent("model_call_start", map[string]interface{}{
			"round":         round,
			"model_call_id": modelCallRec.ID,
		})

		if r.onModelCallHook != nil {
			r.onModelCallHook(round)
		}

		// Execute model call without holding DB transaction
		modelTimeout := time.Duration(r.budget.ModelTimeoutSeconds) * time.Second
		modelCtx, cancelModel := context.WithTimeout(runCtx, modelTimeout)
		resp, callErr := r.provider.Call(modelCtx, messages, r.registry.Definitions(), provider.ModelOpts{
			Timeout: modelTimeout,
		})
		cancelModel()

		if callErr != nil {
			errMsg := callErr.Error()
			r.runService.UpdateModelCall(context.Background(), modelCallRec.ID, "failed", nil, nil, 0, 0, 0, &errMsg)
			r.insertEvent("model_call_failed", map[string]interface{}{
				"round": round,
				"error": errMsg,
			})

			if runCtx.Err() != nil {
				r.handleCancellation(runCtx)
				return
			}

			reason := "provider_error"
			r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		// Model call completed
		modelCallCount++
		var promptTokens, completionTokens, totalTokens int
		if resp.Usage != nil {
			promptTokens = resp.Usage.PromptTokens
			completionTokens = resp.Usage.CompletionTokens
			totalTokens = resp.Usage.TotalTokens
		}
		finishReason := resp.FinishReason
		requestID := resp.RequestID
		r.runService.UpdateModelCall(context.Background(), modelCallRec.ID, "completed", &finishReason, &requestID,
			promptTokens, completionTokens, totalTokens, nil)

		r.insertEvent("model_call_finish", map[string]interface{}{
			"round":            round,
			"finish_reason":    finishReason,
			"tool_calls_count": len(resp.ToolCalls),
			"has_content":      resp.Content != "",
		})

		// Save assistant message to DB and conversation history
		var toolCallsJSON json.RawMessage
		if len(resp.ToolCalls) > 0 {
			toolCallsJSON, _ = json.Marshal(resp.ToolCalls)
		}
		r.runService.InsertAgentMessage(context.Background(), r.runID, "assistant", resp.Content, nil, toolCallsJSON)
		messages = append(messages, provider.ChatMessage{
			Role:      provider.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Handle explicit refusal or truncation
		if resp.FinishReason == "refusal" {
			reason := "model_refused"
			r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}
		if resp.FinishReason == "length" {
			reason := "model_output_truncated"
			r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		// Check if model returned final answer (no tool calls)
		if len(resp.ToolCalls) == 0 {
			if strings.TrimSpace(resp.Content) == "" {
				reason := "protocol_error"
				r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
				return
			}

			// Final answer achieved! Complete run safely
			reason := "final_answer"
			_, err = r.runService.CompleteRunWithOutput(context.Background(), r.runID, reason, resp.Content)
			if err != nil {
				slog.Error("agent runner: failed to complete run with output", "run_id", r.runID, "error", err)
			}
			return
		}

		// Model requested tool calls: validate protocol before executing batch
		seenIDs := make(map[string]bool)
		protocolErr := false
		for _, tc := range resp.ToolCalls {
			if strings.TrimSpace(tc.ID) == "" || seenIDs[tc.ID] {
				protocolErr = true
				break
			}
			seenIDs[tc.ID] = true
		}
		if protocolErr {
			reason := "protocol_error"
			r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		// Check tool call budget
		if toolCallCount+len(resp.ToolCalls) > r.budget.MaxToolCalls {
			reason := "max_tool_calls_exceeded"
			r.runService.TransitionState(context.Background(), r.runID, model.RunStatusRunning, model.RunStatusFailed, &reason)
			return
		}

		// Execute tool calls serially
		for _, tc := range resp.ToolCalls {
			select {
			case <-runCtx.Done():
				r.handleCancellation(runCtx)
				return
			default:
			}

			toolCallCount++
			argsBytes := []byte(tc.Function.Arguments)
			tcRec, err := r.runService.CreateAgentToolCall(context.Background(), r.runID, &modelCallRec.ID, tc.ID, tc.Function.Name, argsBytes)
			if err != nil {
				slog.Error("agent runner: failed to record tool call", "run_id", r.runID, "error", err)
				return
			}

			if r.onToolCallHook != nil {
				r.onToolCallHook(tc.ID, tc.Function.Name)
			}

			// Tool validation
			tool, valErr := r.registry.Validate(tc.Function.Name, argsBytes)
			if valErr != nil {
				errMsg := valErr.Error()
				now := time.Now()
				r.runService.UpdateAgentToolCall(context.Background(), tcRec.ID, "rejected", "", &errMsg, &now, &now)
				r.insertEvent("tool_call_rejected", map[string]interface{}{
					"call_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"error":     errMsg,
				})

				// Return structured error to model so it can correct in next turn
				toolErrOutput := fmt.Sprintf(`{"error": %q}`, errMsg)
				r.runService.InsertAgentMessage(context.Background(), r.runID, "tool", toolErrOutput, &tc.ID, nil)
				messages = append(messages, provider.ChatMessage{
					Role:       provider.RoleTool,
					Content:    toolErrOutput,
					ToolCallID: tc.ID,
				})
				continue
			}

			// Tool execution
			startTime := time.Now()
			r.runService.UpdateAgentToolCall(context.Background(), tcRec.ID, "running", "", nil, &startTime, nil)
			r.insertEvent("tool_call_start", map[string]interface{}{
				"call_id":   tc.ID,
				"tool_name": tc.Function.Name,
			})

			toolTimeout := time.Duration(r.budget.ToolTimeoutSeconds) * time.Second
			toolCtx, cancelTool := context.WithTimeout(runCtx, toolTimeout)
			output, execErr := tool.Execute(toolCtx, argsBytes)
			cancelTool()
			finishTime := time.Now()

			if execErr != nil {
				errMsg := execErr.Error()
				r.runService.UpdateAgentToolCall(context.Background(), tcRec.ID, "failed", "", &errMsg, &startTime, &finishTime)
				r.insertEvent("tool_call_failed", map[string]interface{}{
					"call_id":   tc.ID,
					"tool_name": tc.Function.Name,
					"error":     errMsg,
				})

				if runCtx.Err() != nil {
					r.handleCancellation(runCtx)
					return
				}

				toolErrOutput := fmt.Sprintf(`{"error": %q}`, errMsg)
				r.runService.InsertAgentMessage(context.Background(), r.runID, "tool", toolErrOutput, &tc.ID, nil)
				messages = append(messages, provider.ChatMessage{
					Role:       provider.RoleTool,
					Content:    toolErrOutput,
					ToolCallID: tc.ID,
				})
			} else {
				// Tool output size cap
				if len(output) > r.budget.MaxToolOutputBytes {
					output = output[:r.budget.MaxToolOutputBytes] + "... [truncated due to size limit]"
				}

				r.runService.UpdateAgentToolCall(context.Background(), tcRec.ID, "success", output, nil, &startTime, &finishTime)
				r.insertEvent("tool_call_success", map[string]interface{}{
					"call_id":    tc.ID,
					"tool_name":  tc.Function.Name,
					"output_len": len(output),
				})

				r.runService.InsertAgentMessage(context.Background(), r.runID, "tool", output, &tc.ID, nil)
				messages = append(messages, provider.ChatMessage{
					Role:       provider.RoleTool,
					Content:    output,
					ToolCallID: tc.ID,
				})
			}
		}
	}
}

func (r *AgentRunner) insertEvent(eventType string, payload map[string]interface{}) {
	payloadBytes, _ := json.Marshal(payload)
	ctx := context.Background()
	var nextSeq int
	err := r.runService.Pool().QueryRow(ctx,
		`SELECT COALESCE(MAX(seq), 0) + 1 FROM run_events WHERE run_id = $1`, r.runID).Scan(&nextSeq)
	if err != nil {
		slog.Error("failed to get next seq for event", "run_id", r.runID, "error", err)
		return
	}
	_, err = r.runService.Pool().Exec(ctx,
		`INSERT INTO run_events (id, run_id, seq, type, payload)
		 VALUES (gen_random_uuid(), $1, $2, $3, $4)`,
		r.runID, nextSeq, eventType, payloadBytes)
	if err != nil {
		slog.Error("failed to insert agent event", "run_id", r.runID, "error", err)
	}
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
