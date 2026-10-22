package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/provider"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MainAgent owns the persistent main conversation. It does not create a second
// planner or write worker model_calls. Control effects go through Controller.
type MainAgent struct {
	Runs        *service.RunService
	Controller  contract.ExecutionController
	Workbench   contract.WorkbenchQuery
	Provider    provider.Provider
	Resolver    contract.RuntimeResolver
	Runtime     *provider.ProcessRuntime
	Supervision *Supervision
	SyncDrain   bool
	locks       sync.Map
}

func NewMainAgent(runs *service.RunService, controller contract.ExecutionController, workbench contract.WorkbenchQuery, model provider.Provider, resolver contract.RuntimeResolver) *MainAgent {
	return &MainAgent{
		Runs:        runs,
		Controller:  controller,
		Workbench:   workbench,
		Provider:    model,
		Resolver:    resolver,
		Supervision: NewSupervision(runs, controller),
	}
}

type turnRow struct {
	ID             uuid.UUID
	ConversationID uuid.UUID
	SessionID      uuid.UUID
	RunID          uuid.UUID
	ExpectedEpoch  int
	ActorID        string
	Text           string
}

func (m *MainAgent) Submit(ctx context.Context, principal contract.Principal, idempotencyKey string, body contract.MainMessageSubmit) (*contract.MainMessageAccepted, error) {
	idempotencyKey = strings.TrimSpace(idempotencyKey)
	if err := contract.RequireIdempotencyKey(idempotencyKey); err != nil {
		return nil, err
	}
	if err := body.Validate(); err != nil {
		return nil, err
	}
	if principal.Kind != contract.PrincipalHuman {
		return nil, unauthorized("main messages are submitted by a human principal")
	}
	if err := principal.Validate(); err != nil {
		return nil, err
	}
	if m == nil || m.Runs == nil || m.Controller == nil {
		return nil, persistence("main agent is not configured")
	}
	hash, err := contract.MainMessageRequestHash(body)
	if err != nil {
		return nil, err
	}
	run, err := m.Runs.Get(ctx, principal.RunID)
	if err != nil {
		return nil, mapServiceErr(err)
	}
	if run.CurrentEpoch != body.ExpectedEpoch {
		return nil, staleEpoch("expected_epoch does not match the current run epoch")
	}
	conversationID, sessionID, err := m.ensureConversation(ctx, principal.RunID, run.CurrentEpoch)
	if err != nil {
		return nil, err
	}
	accepted, created, err := m.insertTurn(ctx, principal, conversationID, sessionID, idempotencyKey, hash, body)
	if err != nil {
		return nil, err
	}
	if !created {
		return accepted, nil
	}
	if m.SyncDrain {
		if err := m.Drain(ctx, principal.RunID); err != nil {
			return accepted, err
		}
		return accepted, nil
	}
	go func() { _ = m.Drain(context.Background(), principal.RunID) }()
	return accepted, nil
}

func (m *MainAgent) Drain(ctx context.Context, runID uuid.UUID) error {
	lock := m.runLock(runID)
	lock.Lock()
	defer lock.Unlock()
	for {
		turn, err := m.claimNext(ctx, runID)
		if err != nil || turn == nil {
			return err
		}
		if err := m.process(ctx, runID, turn); err != nil {
			_ = m.failTurn(ctx, turn.ID, err, 0)
			return err
		}
	}
}

func (m *MainAgent) process(ctx context.Context, runID uuid.UUID, turn *turnRow) error {
	if turn == nil {
		return invalidInput("main turn is missing")
	}
	turn.RunID = runID
	run, err := m.Runs.Get(ctx, runID)
	if err != nil {
		return mapServiceErr(err)
	}
	scope := contract.ExecutionScope{RunID: runID, EpochNumber: turn.ExpectedEpoch, Role: contract.RoleMainAgent, SessionID: &turn.SessionID}
	snapshot, err := m.resolveSnapshot(ctx, scope, run.ExecutionInput)
	if err != nil {
		return err
	}
	if _, err := m.Runs.Pool().Exec(ctx, `UPDATE main_turns SET runtime_snapshot=$2 WHERE id=$1`, turn.ID, snapshot); err != nil {
		return persistence("failed to store runtime snapshot")
	}

	explicit := ExplicitControlRequested(turn.Text)
	principal := contract.Principal{
		Kind:                contract.PrincipalMainAgent,
		ActorID:             "mainagent:" + turn.ActorID,
		RunID:               runID,
		ExplicitUserCommand: explicit,
	}
	if classified, ok := ClassifyCommand(turn.Text); ok && explicit && run.CurrentEpoch == turn.ExpectedEpoch {
		cmd := commandFromClassified(turn.ExpectedEpoch, classified)
		result, applyErr := m.Controller.Apply(ctx, principal, scope, mainCommandKey(turn.ID, cmd), cmd)
		if applyErr == nil && result != nil {
			if err := m.insertAction(ctx, turn, result); err != nil {
				return err
			}
		}
	}

	if m.Provider == nil && m.Runtime == nil {
		return executionUnavailable("main agent provider is not configured")
	}
	mutations := ExplicitControlRequested(turn.Text)
	reg, err := m.Supervision.Registry(scope, principal, func(cmd contract.Command) string {
		return mainCommandKey(turn.ID, cmd)
	}, mutations)
	if err != nil {
		return err
	}
	reg.Register(&situationTool{workbench: m.Workbench, runID: runID})

	reserve := mainReserve(run.ExecutionInput)
	used, err := m.countMainCalls(ctx, runID)
	if err != nil {
		return err
	}
	messages := []provider.ChatMessage{
		{Role: provider.RoleSystem, Content: mainSystemPrompt(BudgetChangeRequested(turn.Text))},
	}
	// Restore completed human/assistant exchanges as text. Tool protocol state
	// belongs to its original turn and is never replayed as a new command.
	history, err := m.history(ctx, runID, turn.ID)
	if err != nil {
		return err
	}
	messages = append(messages, history...)
	messages = append(messages, provider.ChatMessage{Role: provider.RoleUser, Content: turn.Text})
	calls := 0
	for used < reserve {
		resp, callID, err := m.callModel(ctx, turn, scope.EpochNumber, messages, reg)
		if err != nil {
			return err
		}
		used++
		calls++
		assistant := provider.ChatMessage{Role: provider.RoleAssistant, Content: resp.Content, ToolCalls: resp.ToolCalls, NativePayload: resp.NativePayload}
		messages = append(messages, assistant)
		if _, _, err := m.appendMessage(ctx, turn, "assistant", resp.Content, nil, &callID, nil); err != nil {
			return err
		}
		if len(resp.ToolCalls) == 0 {
			return m.completeTurn(ctx, turn.ID, calls)
		}
		for _, tc := range resp.ToolCalls {
			if strings.TrimSpace(tc.ID) == "" || strings.TrimSpace(tc.Function.Name) == "" {
				return &contract.Error{Code: contract.CodeInvalidModelOutput, Scope: contract.ScopeSession, Message: "tool call id or name is empty", SuggestedAction: contract.ActionNone}
			}
		}
		for _, tc := range resp.ToolCalls {
			output, execErr := executeTool(ctx, reg, tc)
			if execErr != nil {
				return execErr
			}
			messages = append(messages, provider.ChatMessage{Role: provider.RoleTool, ToolCallID: tc.ID, Content: output})
			if err := m.recordToolAction(ctx, turn, output); err != nil {
				return err
			}
		}
		if err := ProtocolPairingOK(messages); err != nil {
			return &contract.Error{Code: contract.CodeInvalidModelOutput, Scope: contract.ScopeSession, Message: err.Error(), SuggestedAction: contract.ActionNone}
		}
	}
	_, _, err = m.appendMessage(ctx, turn, "assistant", "main reserved model calls are exhausted; the run budget was not increased", nil, nil, nil)
	if err != nil {
		return err
	}
	return m.completeTurn(ctx, turn.ID, calls)
}

func (m *MainAgent) history(ctx context.Context, runID, currentTurn uuid.UUID) ([]provider.ChatMessage, error) {
	rows, err := m.Runs.Pool().Query(ctx, `SELECT role,text FROM (
		SELECT mm.seq,mm.role,mm.text FROM main_messages mm
		JOIN main_turns mt ON mt.id=mm.turn_id
		WHERE mm.run_id=$1 AND mm.turn_id<>$2 AND mt.status='completed'
		AND mm.role IN ('user','assistant') AND mm.text<>''
		ORDER BY mm.seq DESC LIMIT 40) history ORDER BY seq`, runID, currentTurn)
	if err != nil {
		return nil, persistence("failed to load main conversation")
	}
	defer rows.Close()
	var history []provider.ChatMessage
	for rows.Next() {
		var role, text string
		if err := rows.Scan(&role, &text); err != nil {
			return nil, err
		}
		history = append(history, provider.ChatMessage{Role: provider.ChatRole(role), Content: text})
	}
	return history, rows.Err()
}

func (m *MainAgent) callModel(ctx context.Context, turn *turnRow, epoch int, messages []provider.ChatMessage, reg *tools.Registry) (*provider.ModelResponse, uuid.UUID, error) {
	caller := m.Provider
	opts := provider.ModelOpts{Timeout: 30 * time.Second}
	callCtx, cancel := context.WithTimeout(ctx, opts.Timeout)
	defer cancel()
	if m.Runtime != nil {
		run, loadErr := m.Runs.Get(ctx, turn.RunID)
		if loadErr != nil {
			return nil, uuid.Nil, loadErr
		}
		var resolveErr error
		caller, resolveErr = provider.ProviderFromInput(run.ExecutionInput, provider.ScenarioNormal)
		if resolveErr != nil {
			return nil, uuid.Nil, resolveErr
		}
		limiter, tokens, _, admissionErr := m.Runtime.Admission()
		if admissionErr != nil {
			return nil, uuid.Nil, admissionErr
		}
		opts.MaxOutputTokens = tokens
		if waitErr := limiter.Acquire(callCtx); waitErr != nil {
			return nil, uuid.Nil, waitErr
		}
		defer limiter.Release()
	}
	callID := uuid.New()
	_, err := m.Runs.Pool().Exec(callCtx, `
		INSERT INTO main_model_calls (id, run_id, turn_id, session_id, epoch_number, status)
		VALUES ($1,$2,$3,$4,$5,'started')`, callID, turnIDRun(turn), turn.ID, turn.SessionID, epoch)
	if err != nil {
		return nil, uuid.Nil, persistence("failed to record main model call")
	}
	resp, callErr := caller.Call(callCtx, messages, reg.Definitions(), opts)
	status := "completed"
	finish := ""
	var prompt, completion, total int
	var errText *string
	if callErr != nil {
		status = "failed"
		msg := callErr.Error()
		errText = &msg
	} else if resp != nil {
		finish = resp.FinishReason
		if resp.Usage != nil {
			prompt = resp.Usage.PromptTokens
			completion = resp.Usage.CompletionTokens
			total = resp.Usage.TotalTokens
		}
	}
	finishCtx, finishCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer finishCancel()
	if _, updateErr := m.Runs.Pool().Exec(finishCtx, `
		UPDATE main_model_calls
		SET status=$2, finish_reason=$3, prompt_tokens=$4, completion_tokens=$5, total_tokens=$6, error_message=$7, finished_at=now()
		WHERE id=$1`, callID, status, finish, prompt, completion, total, errText); updateErr != nil {
		return nil, uuid.Nil, persistence("failed to finish main model call")
	}
	if callErr != nil {
		return nil, uuid.Nil, executionUnavailable("main model call failed")
	}
	if resp == nil {
		return nil, uuid.Nil, &contract.Error{Code: contract.CodeInvalidModelOutput, Scope: contract.ScopeSession, Message: "provider returned no result", SuggestedAction: contract.ActionNone}
	}
	return resp, callID, nil
}

func turnIDRun(turn *turnRow) uuid.UUID {
	return turnRunID(turn)
}

func (m *MainAgent) ensureConversation(ctx context.Context, runID uuid.UUID, epoch int) (uuid.UUID, uuid.UUID, error) {
	var conversationID, sessionID uuid.UUID
	err := m.Runs.Pool().QueryRow(ctx, `SELECT id, session_id FROM main_conversations WHERE run_id=$1`, runID).Scan(&conversationID, &sessionID)
	if err == nil {
		return conversationID, sessionID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, uuid.Nil, persistence("failed to read main conversation")
	}
	sessionID = uuid.New()
	_, err = m.Runs.Pool().Exec(ctx, `
		INSERT INTO agent_sessions (id, run_id, role, status, input_snapshot, epoch_number, finish_reason)
		VALUES ($1,$2,'mainagent','completed','{"role":"mainagent"}',$3,'main_conversation_anchor')`, sessionID, runID, epoch)
	if err != nil {
		return uuid.Nil, uuid.Nil, persistence("failed to create main session")
	}
	conversationID = uuid.New()
	_, err = m.Runs.Pool().Exec(ctx, `INSERT INTO main_conversations (id, run_id, session_id) VALUES ($1,$2,$3)`, conversationID, runID, sessionID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			if scanErr := m.Runs.Pool().QueryRow(ctx, `SELECT id, session_id FROM main_conversations WHERE run_id=$1`, runID).Scan(&conversationID, &sessionID); scanErr == nil {
				return conversationID, sessionID, nil
			}
		}
		return uuid.Nil, uuid.Nil, persistence("failed to create main conversation")
	}
	return conversationID, sessionID, nil
}

func (m *MainAgent) insertTurn(ctx context.Context, principal contract.Principal, conversationID, sessionID uuid.UUID, key, hash string, body contract.MainMessageSubmit) (*contract.MainMessageAccepted, bool, error) {
	turnID := uuid.New()
	tx, err := m.Runs.Pool().Begin(ctx)
	if err != nil {
		return nil, false, persistence("failed to begin main turn")
	}
	defer tx.Rollback(ctx)
	_, err = tx.Exec(ctx, `
		INSERT INTO main_turns (id, conversation_id, run_id, idempotency_key, request_hash, expected_epoch, actor_id, status)
		VALUES ($1,$2,$3,$4,$5,$6,$7,'queued')`,
		turnID, conversationID, principal.RunID, key, hash, body.ExpectedEpoch, truncateActor(principal.ActorID))
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return m.replayTurn(ctx, principal.RunID, sessionID, key, hash)
		}
		return nil, false, persistence("failed to store main turn")
	}
	messageID, _, err := insertMessageTx(ctx, tx, conversationID, principal.RunID, turnID, "user", body.Message, nil, nil, nil)
	if err != nil {
		return nil, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, false, persistence("failed to commit main turn")
	}
	return &contract.MainMessageAccepted{MessageID: messageID, TurnID: turnID, SessionID: sessionID}, true, nil
}

func (m *MainAgent) replayTurn(ctx context.Context, runID, sessionID uuid.UUID, key, hash string) (*contract.MainMessageAccepted, bool, error) {
	var turnID uuid.UUID
	var stored string
	err := m.Runs.Pool().QueryRow(ctx, `SELECT id, request_hash FROM main_turns WHERE run_id=$1 AND idempotency_key=$2`, runID, key).Scan(&turnID, &stored)
	if err != nil {
		return nil, false, persistence("failed to replay main turn")
	}
	if stored != hash {
		return nil, false, idempotencyConflict("idempotency key was already used with different content")
	}
	var messageID uuid.UUID
	if err := m.Runs.Pool().QueryRow(ctx, `SELECT id FROM main_messages WHERE turn_id=$1 AND role='user' ORDER BY seq LIMIT 1`, turnID).Scan(&messageID); err != nil {
		return nil, false, persistence("failed to replay main message")
	}
	return &contract.MainMessageAccepted{MessageID: messageID, TurnID: turnID, SessionID: sessionID}, false, nil
}

func (m *MainAgent) claimNext(ctx context.Context, runID uuid.UUID) (*turnRow, error) {
	tx, err := m.Runs.Pool().Begin(ctx)
	if err != nil {
		return nil, persistence("failed to claim main turn")
	}
	defer tx.Rollback(ctx)
	turn := &turnRow{}
	err = tx.QueryRow(ctx, `
		UPDATE main_turns SET status='running', started_at=COALESCE(started_at, now())
		WHERE id = (
			SELECT id FROM main_turns
			WHERE run_id=$1 AND status='queued'
			ORDER BY created_at, id
			LIMIT 1
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, conversation_id, run_id, expected_epoch, actor_id`, runID).Scan(&turn.ID, &turn.ConversationID, &turn.RunID, &turn.ExpectedEpoch, &turn.ActorID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, persistence("failed to claim main turn")
	}
	if err := tx.QueryRow(ctx, `SELECT session_id FROM main_conversations WHERE id=$1`, turn.ConversationID).Scan(&turn.SessionID); err != nil {
		return nil, persistence("failed to read main session")
	}
	if err := tx.QueryRow(ctx, `SELECT text FROM main_messages WHERE turn_id=$1 AND role='user' ORDER BY seq LIMIT 1`, turn.ID).Scan(&turn.Text); err != nil {
		return nil, persistence("failed to read main user message")
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, persistence("failed to commit claimed turn")
	}
	return turn, nil
}

func (m *MainAgent) resolveSnapshot(ctx context.Context, scope contract.ExecutionScope, executionInput json.RawMessage) (json.RawMessage, error) {
	if m.Resolver == nil {
		return json.RawMessage(`{"fallback":"historical"}`), nil
	}
	alias := mainProfileAlias(executionInput)
	resolved, err := m.Resolver.Resolve(ctx, scope, alias)
	if err != nil {
		return nil, err
	}
	if resolved == nil || len(resolved.NonSensitiveSnapshot) == 0 {
		return json.RawMessage(`{"alias":"` + alias + `"}`), nil
	}
	return redactPayload(resolved.NonSensitiveSnapshot), nil
}

func (m *MainAgent) insertAction(ctx context.Context, turn *turnRow, result *contract.CommandResult) error {
	if result == nil || result.CommandID == uuid.Nil {
		return nil
	}
	var exists bool
	if err := m.Runs.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM main_messages WHERE turn_id=$1 AND command_id=$2)`, turn.ID, result.CommandID).Scan(&exists); err != nil {
		return persistence("failed to check action message")
	}
	if exists {
		return nil
	}
	text := string(result.Type) + " " + string(result.Status)
	_, _, err := m.appendMessage(ctx, turn, "action", text, &result.CommandID, nil, result.WorkItemID)
	return err
}

func (m *MainAgent) recordToolAction(ctx context.Context, turn *turnRow, output string) error {
	var result contract.CommandResult
	if json.Unmarshal([]byte(output), &result) != nil || result.CommandID == uuid.Nil {
		return nil
	}
	return m.insertAction(ctx, turn, &result)
}

func (m *MainAgent) appendMessage(ctx context.Context, turn *turnRow, role, text string, commandID, modelCallID, workItemID *uuid.UUID) (uuid.UUID, int64, error) {
	if len(text) > 4000 {
		text = text[:4000]
	}
	var lastErr error
	for i := 0; i < 3; i++ {
		id, seq, err := insertMessage(ctx, m.Runs.Pool(), turn.ConversationID, turnRunID(turn), turn.ID, role, text, commandID, modelCallID, workItemID)
		if err == nil {
			return id, seq, nil
		}
		lastErr = err
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
			return uuid.Nil, 0, err
		}
	}
	if lastErr == nil {
		lastErr = persistence("failed to store main message")
	}
	return uuid.Nil, 0, lastErr
}

func (m *MainAgent) completeTurn(ctx context.Context, turnID uuid.UUID, calls int) error {
	_, err := m.Runs.Pool().Exec(ctx, `UPDATE main_turns SET status='completed', model_calls=$2, finished_at=now() WHERE id=$1`, turnID, calls)
	if err != nil {
		return persistence("failed to complete main turn")
	}
	return nil
}

func (m *MainAgent) failTurn(ctx context.Context, turnID uuid.UUID, cause error, calls int) error {
	var ce *contract.Error
	if !errors.As(cause, &ce) {
		ce = persistence(cause.Error())
	}
	body, _ := json.Marshal(ce.Envelope())
	_, err := m.Runs.Pool().Exec(ctx, `UPDATE main_turns SET status='failed', model_calls=$2, error=$3, finished_at=now() WHERE id=$1 AND status='running'`, turnID, calls, body)
	return err
}

func (m *MainAgent) countMainCalls(ctx context.Context, runID uuid.UUID) (int, error) {
	var n int
	if err := m.Runs.Pool().QueryRow(ctx, `SELECT COUNT(*) FROM main_model_calls WHERE run_id=$1`, runID).Scan(&n); err != nil {
		return 0, persistence("failed to count main model calls")
	}
	return n, nil
}

func (m *MainAgent) runLock(id uuid.UUID) *sync.Mutex {
	value, _ := m.locks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func insertMessage(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, conversationID, runID, turnID uuid.UUID, role, text string, commandID, modelCallID, workItemID *uuid.UUID) (uuid.UUID, int64, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, persistence("failed to store main message")
	}
	defer tx.Rollback(ctx)
	id, seq, err := insertMessageTx(ctx, tx, conversationID, runID, turnID, role, text, commandID, modelCallID, workItemID)
	if err != nil {
		return uuid.Nil, 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, persistence("failed to commit main message")
	}
	return id, seq, nil
}

func insertMessageTx(ctx context.Context, tx pgx.Tx, conversationID, runID, turnID uuid.UUID, role, text string, commandID, modelCallID, workItemID *uuid.UUID) (uuid.UUID, int64, error) {
	id := uuid.New()
	var seq int64
	err := tx.QueryRow(ctx, `
		INSERT INTO main_messages (id, conversation_id, run_id, seq, role, text, turn_id, command_id, model_call_id, work_item_id)
		VALUES ($1,$2,$3,(SELECT COALESCE(MAX(seq),0)+1 FROM main_messages WHERE conversation_id=$2),$4,$5,$6,$7,$8,$9)
		RETURNING seq`, id, conversationID, runID, role, text, turnID, commandID, modelCallID, workItemID).Scan(&seq)
	if err != nil {
		return uuid.Nil, 0, err
	}
	return id, seq, nil
}

func commandFromClassified(epoch int, classified ClassifiedCommand) contract.Command {
	cmd := contract.Command{Type: classified.Type, ExpectedEpoch: epoch, WorkItemID: classified.WorkItemID}
	switch classified.Type {
	case contract.CmdSteerWork:
		cmd.Payload, _ = json.Marshal(contract.SteerPayload{Instruction: classified.Instruction})
	case contract.CmdKillWork:
		cmd.Payload, _ = json.Marshal(contract.KillPayload{Reason: classified.Reason})
	case contract.CmdHint:
		cmd.Payload, _ = json.Marshal(contract.HintPayload{Text: classified.Text})
	case contract.CmdAddIntent:
		cmd.Payload, _ = json.Marshal(contract.AddIntentPayload{Summary: classified.Text})
	case contract.CmdContinueWork, contract.CmdRetryWork:
		cmd.Payload, _ = json.Marshal(contract.ContinuePayload{Instruction: classified.Instruction})
	case contract.CmdStartEpoch:
		cmd.Payload, _ = json.Marshal(contract.StartEpochPayload{Reason: classified.Reason})
	}
	return cmd
}

func mainCommandKey(turnID uuid.UUID, cmd contract.Command) string {
	target := ""
	if cmd.WorkItemID != nil {
		target = cmd.WorkItemID.String()
	}
	return "main-turn:" + turnID.String() + ":" + string(cmd.Type) + ":" + target
}

func mainReserve(executionInput json.RawMessage) int {
	var wrapped struct {
		Budget struct {
			MainReservedModelCalls int `json:"main_reserved_model_calls"`
		} `json:"budget"`
	}
	if json.Unmarshal(executionInput, &wrapped) == nil && wrapped.Budget.MainReservedModelCalls > 0 {
		return wrapped.Budget.MainReservedModelCalls
	}
	return contract.SuggestedExplorationTemplate().MainReservedModelCalls
}

func mainProfileAlias(executionInput json.RawMessage) string {
	cfg, present, err := contract.ParseAgentsConfig(executionInput)
	if err == nil && present {
		return cfg.MainAgent.Model
	}
	return "historical"
}

func mainSystemPrompt(budgetAsked bool) string {
	prompt := "You are the main agent for this run. Explain progress only from tool results and stored records. A sentence you write does not change run state. Control changes happen only through tools, and those tools use the shared execution controller. You cannot grant budget, expand scope, start an extra engine, or run a host shell."
	if budgetAsked {
		prompt += " The user asked to change budget or scope. Tell them to use the structured authorization path. Do not claim the budget changed."
	}
	return prompt
}

func executeTool(ctx context.Context, reg *tools.Registry, call provider.ToolCall) (string, error) {
	tool, err := reg.Validate(call.Function.Name, []byte(call.Function.Arguments))
	if err != nil {
		body, _ := json.Marshal(map[string]any{"error": map[string]any{"code": contract.CodeInvalidModelOutput, "message": err.Error()}})
		return string(body), nil
	}
	return tool.Execute(ctx, []byte(call.Function.Arguments))
}

type situationTool struct {
	workbench contract.WorkbenchQuery
	runID     uuid.UUID
}

func (t *situationTool) Name() string { return "get_situation" }
func (t *situationTool) Description() string {
	return "Read the current workbench snapshot. This does not change run state."
}
func (t *situationTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t *situationTool) Validate(_ []byte) error { return nil }
func (t *situationTool) Execute(ctx context.Context, _ []byte) (string, error) {
	if t.workbench == nil {
		return "", persistence("workbench is not configured")
	}
	snap, err := t.workbench.Snapshot(ctx, t.runID, contract.WorkbenchQueryParams{})
	if err != nil {
		var ce *contract.Error
		if errors.As(err, &ce) && ce.Code != contract.CodePersistenceError {
			return envelopeJSON(ce), nil
		}
		return "", err
	}
	body, err := json.Marshal(snap)
	if err != nil {
		return "", persistence("failed to encode situation")
	}
	return string(body), nil
}

// turnRunID is the run stored on the claimed turn. Model-call and message
// inserts must use it so they cannot land on a nil run id.
func turnRunID(turn *turnRow) uuid.UUID {
	if turn == nil {
		return uuid.Nil
	}
	return turn.RunID
}
