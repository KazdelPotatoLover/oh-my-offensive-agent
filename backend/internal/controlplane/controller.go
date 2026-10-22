package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// RecoveryFunc is the existing epoch handoff. Agent 0 wires it to quiesce,
// StartNewEpoch, revoke, and the one startRunExecutor path. It must not create
// a second run or a second engine. executorStarted is true only when that
// single executor was started.
type RecoveryFunc func(ctx context.Context, runID uuid.UUID, reason, summary string, expectedEpoch int, idempotencyKey string) (newEpoch int, executorStarted bool, err error)

// Controller is the single writer for control commands. REST, planner tools,
// and Main Agent call Apply. It composes RunService with the live executor and
// the replan port. It does not call a model or wait for process exit while
// holding the run lock.
type Controller struct {
	Runs    *service.RunService
	Live    LiveControl
	Replan  ReplanPort
	Recover RecoveryFunc
}

var _ contract.ExecutionController = (*Controller)(nil)

func NewController(runs *service.RunService, live LiveControl, replan ReplanPort, recover RecoveryFunc) *Controller {
	return &Controller{Runs: runs, Live: live, Replan: replan, Recover: recover}
}

type storedCommand struct {
	Hash   string
	Result *contract.CommandResult
}

func (c *Controller) Apply(ctx context.Context, principal contract.Principal, scope contract.ExecutionScope, idempotencyKey string, cmd contract.Command) (*contract.CommandResult, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err := contract.RequireIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if principal.RunID != scope.RunID {
		return nil, scopeDenied("principal run does not match execution scope")
	}
	if err := cmd.Validate(); err != nil {
		return nil, err
	}
	if err := contract.CommandAllowed(principal, cmd.Type); err != nil {
		return nil, err
	}
	if err := roleMatches(principal, scope); err != nil {
		return nil, err
	}
	if scope.EpochNumber != cmd.ExpectedEpoch {
		return nil, staleEpoch("scope epoch does not match command expected_epoch")
	}
	if err := rejectExpansion(cmd); err != nil {
		return nil, err
	}
	hash, err := contract.CommandRequestHash(cmd)
	if err != nil {
		return nil, err
	}
	if c == nil || c.Runs == nil || c.Runs.Pool() == nil {
		return nil, persistence("execution controller is not configured")
	}

	unlock, err := lockAdvisory(ctx, c.Runs.Pool(), advisoryKey(controlLockNS, scope.RunID))
	if err != nil {
		return nil, err
	}
	defer unlock()

	existing, err := c.load(ctx, scope.RunID, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Hash != hash {
			return nil, idempotencyConflict("idempotency key was already used with different content")
		}
		return replayedResult(existing.Result), nil
	}

	run, err := c.Runs.Get(ctx, scope.RunID)
	if err != nil {
		return nil, mapServiceErr(err)
	}
	if run.CurrentEpoch != cmd.ExpectedEpoch {
		// Leave the key unused so the caller can retry with the current epoch.
		return nil, staleEpoch("expected_epoch does not match the current run epoch")
	}

	var attemptID *uuid.UUID
	if cmd.Type.TargetsWorkItem() {
		item, err := c.Runs.GetWorkItem(ctx, *cmd.WorkItemID)
		if err != nil {
			return nil, mapServiceErr(err)
		}
		if item.RunID != scope.RunID {
			return nil, scopeDenied("work item belongs to another run")
		}
		if cmd.ExpectedAttemptID != nil {
			if item.CurrentAttemptID == nil || *item.CurrentAttemptID != *cmd.ExpectedAttemptID {
				return nil, invalidState("expected_attempt_id does not match the current attempt")
			}
		}
		attemptID = item.CurrentAttemptID
	}

	// Structured human grants must be possible BEFORE restarting an exhausted
	// epoch. They update only the ledger; they do not revive execution. The
	// principal check above still rejects budget grants from every model role.
	canGrantBeforeRecovery := cmd.Type == contract.CmdGrantBudget &&
		(run.Status == model.RunStatusStopped || run.Status == model.RunStatusFailed || run.Status == model.RunStatusInterrupted)
	if run.Status.IsTerminal() && cmd.Type != contract.CmdStartEpoch && !canGrantBeforeRecovery {
		return nil, invalidState("terminal run is read-only until an explicit epoch recovery")
	}
	if cmd.Type == contract.CmdStartEpoch {
		if run.Status == model.RunStatusCompleted || run.Status == model.RunStatusStopping {
			return nil, invalidState("completed or stopping runs are not eligible for epoch recovery")
		}
		reason, summary, err := startEpochPayload(cmd.Payload)
		if err != nil {
			return nil, err
		}
		unlock()
		return c.applyStartEpoch(ctx, principal, scope, idempotencyKey, hash, cmd, reason, summary)
	}

	result, err := c.dispatch(ctx, principal, scope, idempotencyKey, cmd, attemptID)
	if err != nil {
		return nil, err
	}
	if err := c.insert(ctx, principal, scope, idempotencyKey, hash, cmd, result); err != nil {
		if existing, loadErr := c.load(ctx, scope.RunID, idempotencyKey); loadErr == nil && existing != nil {
			if existing.Hash != hash {
				return nil, idempotencyConflict("idempotency key was already used with different content")
			}
			return replayedResult(existing.Result), nil
		}
		return nil, err
	}
	return result, nil
}

func replayedResult(result *contract.CommandResult) *contract.CommandResult {
	if result == nil {
		return nil
	}
	copy := *result
	copy.Replayed = true
	return &copy
}

func (c *Controller) applyStartEpoch(ctx context.Context, principal contract.Principal, scope contract.ExecutionScope, key, hash string, cmd contract.Command, reason, summary string) (*contract.CommandResult, error) {
	var epoch int
	var started bool
	var callErr error
	if c.Recover != nil {
		epoch, started, callErr = c.Recover(ctx, scope.RunID, reason, summary, cmd.ExpectedEpoch, key)
	} else {
		epoch, callErr = c.Runs.StartNewEpoch(ctx, scope.RunID, reason, summary, service.EpochStartOptions{
			ExpectedEpoch:  cmd.ExpectedEpoch,
			IdempotencyKey: key,
		})
	}

	unlock, err := lockAdvisory(ctx, c.Runs.Pool(), advisoryKey(controlLockNS, scope.RunID))
	if err != nil {
		return nil, err
	}
	defer unlock()
	existing, err := c.load(ctx, scope.RunID, key)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		if existing.Hash != hash {
			return nil, idempotencyConflict("idempotency key was already used with different content")
		}
		return replayedResult(existing.Result), nil
	}
	if callErr != nil {
		return nil, mapServiceErr(callErr)
	}
	body, _ := json.Marshal(map[string]any{"epoch_number": epoch, "executor_started": started})
	result := &contract.CommandResult{
		CommandID:   uuid.New(),
		Status:      contract.CommandApplied,
		Type:        cmd.Type,
		EpochNumber: epoch,
		Delivered:   started,
		Result:      body,
	}
	if err := c.insert(ctx, principal, scope, key, hash, cmd, result); err != nil {
		if existing, loadErr := c.load(ctx, scope.RunID, key); loadErr == nil && existing != nil && existing.Hash == hash {
			return replayedResult(existing.Result), nil
		}
		return nil, err
	}
	return result, nil
}

func (c *Controller) dispatch(ctx context.Context, principal contract.Principal, scope contract.ExecutionScope, key string, cmd contract.Command, attemptID *uuid.UUID) (*contract.CommandResult, error) {
	result := &contract.CommandResult{
		CommandID:   uuid.New(),
		Type:        cmd.Type,
		EpochNumber: cmd.ExpectedEpoch,
		WorkItemID:  cmd.WorkItemID,
		AttemptID:   attemptID,
		Status:      contract.CommandApplied,
	}
	var extra any
	switch cmd.Type {
	case contract.CmdSteerWork:
		instruction, err := requiredText(cmd.Payload, "instruction")
		if err != nil {
			return nil, err
		}
		if err := c.Runs.SteerWorkItem(ctx, scope.RunID, *cmd.WorkItemID, instruction, nil, key); err != nil {
			return nil, mapServiceErr(err)
		}
		armed := false
		if c.Replan != nil {
			c.Replan.NoteSteer(*cmd.WorkItemID, instruction)
			armed = true
		}
		if c.Live != nil {
			c.Live.Nudge(scope.RunID)
		}
		result.Status = contract.CommandAccepted
		result.Delivered = armed
		extra = map[string]any{"instruction": instruction}
	case contract.CmdKillWork:
		reason := optionalText(cmd.Payload, "reason")
		if reason == "" {
			reason = "killed_by_operator"
		}
		issued := false
		if c.Live != nil {
			if err := c.Live.Kill(scope.RunID, *cmd.WorkItemID, reason); err != nil {
				return nil, executionUnavailable("live worker cancel failed")
			}
			issued = true
		}
		if err := c.Runs.KillWorkItem(ctx, scope.RunID, *cmd.WorkItemID, reason); err != nil {
			return nil, mapServiceErr(err)
		}
		result.Delivered = issued
		extra = map[string]any{"reason": reason}
	case contract.CmdContinueWork, contract.CmdRetryWork:
		reason := optionalText(cmd.Payload, "instruction")
		if reason == "" {
			reason = string(cmd.Type)
		}
		expected := uuid.Nil
		if cmd.ExpectedAttemptID != nil {
			expected = *cmd.ExpectedAttemptID
		}
		var err error
		if cmd.Type == contract.CmdContinueWork {
			_, err = c.Runs.ContinueWorkItem(ctx, scope.RunID, *cmd.WorkItemID, expected, reason)
		} else {
			_, err = c.Runs.RetryWorkItem(ctx, scope.RunID, *cmd.WorkItemID, expected, reason)
		}
		if err != nil {
			return nil, mapServiceErr(err)
		}
		if c.Live != nil {
			c.Live.Nudge(scope.RunID)
			result.Delivered = true
		}
		extra = map[string]any{"reason": reason}
	case contract.CmdPauseRun:
		if err := c.Runs.PauseRun(ctx, scope.RunID); err != nil {
			return nil, mapServiceErr(err)
		}
		if c.Live != nil {
			if err := c.Live.Pause(scope.RunID); err != nil {
				return nil, executionUnavailable("live pause failed")
			}
			result.Delivered = true
		}
	case contract.CmdResumeRun:
		if err := c.Runs.ResumeRun(ctx, scope.RunID); err != nil {
			return nil, mapServiceErr(err)
		}
		if c.Live != nil {
			if err := c.Live.Resume(scope.RunID); err != nil {
				return nil, executionUnavailable("live resume failed")
			}
			result.Delivered = true
		}
		if c.Live != nil {
			c.Live.Nudge(scope.RunID)
		}
	case contract.CmdStopRun:
		if _, err := c.Runs.Stop(ctx, scope.RunID); err != nil {
			return nil, mapServiceErr(err)
		}
		if c.Live != nil {
			if err := c.Live.Stop(ctx, scope.RunID); err != nil {
				return nil, executionUnavailable("live stop failed")
			}
			result.Delivered = true
		}
	case contract.CmdHint:
		text, err := requiredText(cmd.Payload, "text")
		if err != nil {
			return nil, err
		}
		node, err := c.Runs.RecordHint(ctx, scope.RunID, text, nil)
		if err != nil {
			return nil, mapServiceErr(err)
		}
		if c.Live != nil {
			c.Live.Nudge(scope.RunID)
			result.Delivered = true
		}
		extra = map[string]any{"hint_id": node.ID}
	case contract.CmdAddIntent:
		if err := c.addIntent(ctx, scope, cmd, result); err != nil {
			return nil, err
		}
		return result, nil
	case contract.CmdGrantBudget:
		grant, err := c.grant(ctx, scope, key, cmd)
		if err != nil {
			return nil, err
		}
		extra = grant
		if c.Live != nil {
			if err := c.Live.RefreshBudget(ctx, scope.RunID); err != nil {
				return nil, executionUnavailable("live budget refresh failed")
			}
			result.Delivered = true
		}
	default:
		return nil, invalidInput("unsupported command")
	}
	if extra != nil {
		result.Result, _ = json.Marshal(extra)
	}
	return result, nil
}

func (c *Controller) addIntent(ctx context.Context, scope contract.ExecutionScope, cmd contract.Command, result *contract.CommandResult) error {
	run, err := c.Runs.Get(ctx, scope.RunID)
	if err != nil {
		return mapServiceErr(err)
	}
	if run.Status != model.RunStatusRunning {
		return invalidState("add_intent requires a running run")
	}
	var payload contract.AddIntentPayload
	if err := json.Unmarshal(cmd.Payload, &payload); err != nil || strings.TrimSpace(payload.Summary) == "" {
		return invalidInput("add_intent requires summary")
	}
	node, err := c.Runs.AddIntentNode(ctx, scope.RunID, payload.Summary, payload.SourceFactIDs, nil, payload.Priority, nil)
	if err != nil {
		return mapServiceErr(err)
	}
	key := "intent-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	items, err := c.Runs.SubmitPlan(ctx, scope.RunID, []service.PlanItemSpec{{
		Key:           key,
		Title:         payload.Summary,
		Objective:     payload.Summary,
		WorkType:      "intent",
		InputData:     map[string]any{},
		SourceFactIDs: payload.SourceFactIDs,
	}})
	if err != nil {
		return mapServiceErr(err)
	}
	if len(items) == 0 {
		return persistence("add_intent did not create a work item")
	}
	result.WorkItemID = &items[0].ID
	if c.Live != nil {
		c.Live.Nudge(scope.RunID)
		result.Delivered = true
	}
	result.Result, _ = json.Marshal(map[string]any{
		"intent_id":     node.ID,
		"work_item_id":  items[0].ID,
		"work_item_key": items[0].Key,
		"status":        items[0].Status,
	})
	return nil
}

func (c *Controller) grant(ctx context.Context, scope contract.ExecutionScope, key string, cmd contract.Command) (any, error) {
	var payload struct {
		AdditionalModelCalls int    `json:"additional_model_calls"`
		AdditionalToolCalls  int    `json:"additional_tool_calls"`
		AdditionalSeconds    int    `json:"additional_seconds"`
		Reason               string `json:"reason"`
	}
	if len(cmd.Payload) > 0 {
		if err := json.Unmarshal(cmd.Payload, &payload); err != nil {
			return nil, invalidInput("grant_budget payload must be an object")
		}
	}
	if payload.Reason == "" {
		payload.Reason = "structured human authorization"
	}
	keyCopy := key
	grant, err := c.Runs.GrantBudget(ctx, scope.RunID, payload.AdditionalModelCalls, payload.AdditionalToolCalls, payload.AdditionalSeconds, payload.Reason, &keyCopy)
	if err != nil {
		return nil, mapServiceErr(err)
	}
	return map[string]any{
		"grant_id":               grant.ID,
		"additional_model_calls": grant.AdditionalModelCalls,
		"additional_tool_calls":  grant.AdditionalToolCalls,
		"additional_seconds":     grant.AdditionalSeconds,
	}, nil
}

func (c *Controller) load(ctx context.Context, runID uuid.UUID, key string) (*storedCommand, error) {
	var hash string
	var raw []byte
	err := c.Runs.Pool().QueryRow(ctx, `SELECT request_hash, result FROM execution_commands WHERE run_id=$1 AND idempotency_key=$2`, runID, key).Scan(&hash, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, persistence("failed to read execution command")
	}
	var result contract.CommandResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, persistence("stored command result is unreadable")
	}
	return &storedCommand{Hash: hash, Result: &result}, nil
}

func (c *Controller) insert(ctx context.Context, principal contract.Principal, scope contract.ExecutionScope, key, hash string, cmd contract.Command, result *contract.CommandResult) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return persistence("failed to encode command result")
	}
	_, err = c.Runs.Pool().Exec(ctx, `
		INSERT INTO execution_commands (
			id, run_id, idempotency_key, request_hash, command_type, status,
			principal_kind, actor_id, explicit_user_command, epoch_number,
			work_item_id, attempt_id, source_tool_call_id, delivered, result
		) VALUES (
			$1,$2,$3,$4,$5,$6,
			$7,$8,$9,$10,
			$11,$12,$13,$14,$15
		)`,
		result.CommandID, scope.RunID, key, hash, string(cmd.Type), string(result.Status),
		string(principal.Kind), truncateActor(principal.ActorID), principal.ExplicitUserCommand, result.EpochNumber,
		result.WorkItemID, result.AttemptID, scope.SourceToolCallID, result.Delivered, raw,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return idempotencyConflict("idempotency key was already used")
		}
		return persistence("failed to store execution command")
	}
	return nil
}

func roleMatches(principal contract.Principal, scope contract.ExecutionScope) error {
	switch principal.Kind {
	case contract.PrincipalHuman, contract.PrincipalSystem:
		if scope.Role != contract.RoleMainAgent && scope.Role != contract.RolePlanner {
			return unauthorized("control scope role is not allowed for this principal")
		}
	case contract.PrincipalPlanner:
		if scope.Role != contract.RolePlanner {
			return unauthorized("planner principal requires planner scope")
		}
	case contract.PrincipalMainAgent:
		if scope.Role != contract.RoleMainAgent {
			return unauthorized("main agent principal requires mainagent scope")
		}
	}
	return nil
}

func startEpochPayload(raw json.RawMessage) (string, string, error) {
	var payload contract.StartEpochPayload
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &payload); err != nil {
			return "", "", invalidInput("start_epoch payload must be an object")
		}
	}
	if strings.TrimSpace(payload.Reason) == "" {
		return "", "", invalidInput("start_epoch requires a reason")
	}
	return payload.Reason, payload.Summary, nil
}

func requiredText(raw json.RawMessage, field string) (string, error) {
	text := optionalText(raw, field)
	if strings.TrimSpace(text) == "" {
		return "", invalidInput(field + " is required")
	}
	return text, nil
}

func optionalText(raw json.RawMessage, field string) string {
	if len(raw) == 0 {
		return ""
	}
	var payload map[string]any
	if err := json.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	text, _ := payload[field].(string)
	return strings.TrimSpace(text)
}

func truncateActor(actor string) string {
	actor = strings.TrimSpace(actor)
	if len(actor) > 200 {
		return actor[:200]
	}
	return actor
}
