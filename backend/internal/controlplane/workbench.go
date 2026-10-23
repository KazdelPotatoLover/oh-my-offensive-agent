package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
	"agent-workbench/backend/internal/service"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ModelCapacitySource supplies the shared upstream limit. A nil source reports
// zeros; it does not invent a limit.
type ModelCapacitySource interface {
	ModelCapacity(ctx context.Context, runID uuid.UUID) (contract.ModelCapacity, error)
}

// Workbench is the workbench.v1 read model. It does not start an engine or
// mutate a run.
type Workbench struct {
	Runs   *service.RunService
	Models ModelCapacitySource
	Now    func() time.Time
}

func NewWorkbench(runs *service.RunService, models ModelCapacitySource) *Workbench {
	return &Workbench{Runs: runs, Models: models}
}

var _ contract.WorkbenchQuery = (*Workbench)(nil)

func (w *Workbench) Snapshot(ctx context.Context, runID uuid.UUID, q contract.WorkbenchQueryParams) (*contract.WorkbenchSnapshot, error) {
	if w == nil || w.Runs == nil || w.Runs.Pool() == nil {
		return nil, persistence("workbench is not configured")
	}
	if runID == uuid.Nil {
		return nil, invalidInput("run_id is required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	if q.AfterEventSeq < 0 {
		return nil, invalidInput("after_event_seq must be non-negative")
	}

	tx, err := w.Runs.Pool().BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead})
	if err != nil {
		return nil, persistence("failed to open workbench snapshot")
	}
	defer tx.Rollback(ctx)

	var status string
	var epoch int
	var executionInput []byte
	err = tx.QueryRow(ctx, `SELECT status, current_epoch, execution_input FROM runs WHERE id=$1`, runID).Scan(&status, &epoch, &executionInput)
	if err != nil {
		if err == pgx.ErrNoRows {
			return nil, notFound("run not found")
		}
		return nil, persistence("failed to read run")
	}
	var epochStatus string
	err = tx.QueryRow(ctx, `SELECT status FROM execution_epochs WHERE run_id=$1 AND epoch_number=$2`, runID, epoch).Scan(&epochStatus)
	if err == pgx.ErrNoRows {
		epochStatus = ""
	} else if err != nil {
		return nil, persistence("failed to read execution epoch")
	}
	if model.RunStatus(status).IsTerminal() && epochStatus == "running" {
		return nil, invalidState("refusing mixed snapshot: run is terminal while the current epoch is running")
	}

	requested, effective, err := workerPoolSize(executionInput)
	if err != nil {
		return nil, err
	}
	var active, queued int
	if err = tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM attempts a
		JOIN work_items wi ON wi.id = a.work_item_id
		WHERE wi.run_id=$1 AND a.status='running'`, runID).Scan(&active); err != nil {
		return nil, persistence("failed to count active workers")
	}
	if err = tx.QueryRow(ctx, `SELECT COUNT(*) FROM work_items WHERE run_id=$1 AND status='pending'`, runID).Scan(&queued); err != nil {
		return nil, persistence("failed to count queued directions")
	}
	capacity := contract.WorkerCapacity{Requested: requested, Effective: effective, Active: active, Queued: queued}
	if err := capacity.Validate(); err != nil {
		return nil, err
	}

	modelCapacity := contract.ModelCapacity{}
	if w.Models != nil {
		modelCapacity, err = w.Models.ModelCapacity(ctx, runID)
		if err != nil {
			return nil, err
		}
	}

	planner, err := readPlanner(ctx, tx, runID, status, active, queued)
	if err != nil {
		return nil, err
	}
	directions, err := readDirections(ctx, tx, runID, status)
	if err != nil {
		return nil, err
	}
	goals, err := readGoals(ctx, tx, runID)
	if err != nil {
		return nil, err
	}
	events, lastSeq, hasMore, err := readEvents(ctx, tx, runID, q.AfterEventSeq, limit)
	if err != nil {
		return nil, err
	}
	cursor, err := readMainCursor(ctx, tx, runID)
	if err != nil {
		return nil, err
	}

	snap := &contract.WorkbenchSnapshot{
		SchemaVersion:    contract.WorkbenchSchemaVersion,
		SnapshotAt:       w.now(),
		Run:              contract.WorkbenchRun{ID: runID, Status: status, EpochNumber: epoch},
		WorkerCapacity:   capacity,
		ModelCapacity:    modelCapacity,
		Budget:           rawBudget(executionInput),
		Goals:            goals,
		Planner:          planner,
		Directions:       directions,
		MainConversation: cursor,
		Events:           events,
		LastEventSeq:     lastSeq,
		HasMoreEvents:    hasMore,
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	return snap, nil
}

func (w *Workbench) MainMessages(ctx context.Context, runID uuid.UUID, afterSeq int64, limit int) (*contract.MainMessagePage, error) {
	if w == nil || w.Runs == nil || w.Runs.Pool() == nil {
		return nil, persistence("workbench is not configured")
	}
	if afterSeq < 0 {
		return nil, invalidInput("after_seq must be non-negative")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 100 {
		limit = 100
	}
	var exists bool
	if err := w.Runs.Pool().QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM runs WHERE id=$1)`, runID).Scan(&exists); err != nil {
		return nil, persistence("failed to read run")
	}
	if !exists {
		return nil, notFound("run not found")
	}
	rows, err := w.Runs.Pool().Query(ctx, `
		SELECT m.seq, m.id, m.role, m.text, m.turn_id, COALESCE(t.status, ''), m.command_id, m.model_call_id, m.tool_call_id, m.work_item_id
		FROM main_messages m
		LEFT JOIN main_turns t ON t.id = m.turn_id
		WHERE m.run_id=$1 AND m.seq > $2
		ORDER BY m.seq ASC
		LIMIT $3`, runID, afterSeq, limit+1)
	if err != nil {
		return nil, persistence("failed to read main messages")
	}
	defer rows.Close()
	page := &contract.MainMessagePage{Messages: []contract.MainMessage{}, LastSeq: afterSeq}
	for rows.Next() {
		var msg contract.MainMessage
		var turnStatus string
		if err := rows.Scan(&msg.Seq, &msg.ID, &msg.Role, &msg.Text, &msg.TurnID, &turnStatus, &msg.CommandID, &msg.ModelCallID, &msg.ToolCallID, &msg.WorkItemID); err != nil {
			return nil, persistence("failed to scan main message")
		}
		msg.TurnStatus = turnStatus
		if err := msg.Validate(); err != nil {
			return nil, err
		}
		page.Messages = append(page.Messages, msg)
	}
	if err := rows.Err(); err != nil {
		return nil, persistence("failed to read main messages")
	}
	if len(page.Messages) > limit {
		page.HasMore = true
		page.Messages = page.Messages[:limit]
	}
	if len(page.Messages) > 0 {
		page.LastSeq = page.Messages[len(page.Messages)-1].Seq
	}
	return page, nil
}

func (w *Workbench) now() time.Time {
	if w.Now != nil {
		return w.Now().UTC()
	}
	return time.Now().UTC()
}

func workerPoolSize(executionInput []byte) (int, int, error) {
	var wrapped struct {
		Budget map[string]json.RawMessage `json:"budget"`
	}
	if len(executionInput) > 0 {
		if err := json.Unmarshal(executionInput, &wrapped); err != nil {
			return 0, 0, invalidInput("execution_input is not an object")
		}
	}
	raw, present := wrapped.Budget["max_parallel_workers"]
	if !present || len(raw) == 0 || string(raw) == "null" {
		return contract.LegacyMissingWorkerDefault, contract.LegacyMissingWorkerDefault, nil
	}
	var requested int
	if err := json.Unmarshal(raw, &requested); err != nil {
		return 0, 0, invalidInput("max_parallel_workers must be an integer")
	}
	accepted, err := contract.AcceptWorkerCount(requested, contract.MaxParallelWorkers)
	if err != nil {
		return 0, 0, err
	}
	return accepted, accepted, nil
}

func rawBudget(executionInput []byte) json.RawMessage {
	var wrapped struct {
		Budget json.RawMessage `json:"budget"`
	}
	if json.Unmarshal(executionInput, &wrapped) != nil || len(wrapped.Budget) == 0 || string(wrapped.Budget) == "null" {
		return json.RawMessage(`{}`)
	}
	policy := model.PolicyFromExecutionInput(executionInput)
	if policy == nil {
		return wrapped.Budget
	}
	var obj map[string]any
	if json.Unmarshal(wrapped.Budget, &obj) != nil {
		return wrapped.Budget
	}
	obj["limits"] = policy.PublicLimits()
	out, err := json.Marshal(obj)
	if err != nil {
		return wrapped.Budget
	}
	return out
}

func readPlanner(ctx context.Context, tx pgx.Tx, runID uuid.UUID, runStatus string, active, queued int) (contract.PlannerView, error) {
	view := contract.PlannerView{Status: "idle"}
	var status string
	var sessionID *uuid.UUID
	err := tx.QueryRow(ctx, `SELECT status, session_id FROM planner_rounds WHERE run_id=$1 ORDER BY round DESC LIMIT 1`, runID).Scan(&status, &sessionID)
	if err != nil && err != pgx.ErrNoRows {
		return view, persistence("failed to read planner round")
	}
	if sessionID != nil {
		view.SessionID = sessionID
	}
	switch {
	case runStatus == string(model.RunStatusPaused):
		view.Status = "waiting"
		view.Reason = "run admission is paused"
	case runStatus == string(model.RunStatusStopped) || runStatus == string(model.RunStatusStopping) || runStatus == string(model.RunStatusInterrupted):
		view.Status = "stopped"
	case runStatus == string(model.RunStatusFailed):
		view.Status = "failed"
	case active > 0 && queued == 0:
		view.Status = "waiting"
		view.Reason = "waiting for in-flight worker directions"
	case status == "running":
		view.Status = "planning"
	case status == "failed":
		view.Status = "failed"
	case status == "stopped":
		view.Status = "stopped"
	default:
		view.Status = "idle"
	}
	return view, nil
}

func readDirections(ctx context.Context, tx pgx.Tx, runID uuid.UUID, runStatus string) ([]contract.DirectionView, error) {
	rows, err := tx.Query(ctx, `
		SELECT wi.id, wi.objective, wi.status, wi.input_data, wi.updated_at, wi.current_attempt_id,
		       a.session_id, a.finish_reason, tc.tool_name, tc.created_at
		FROM work_items wi
		LEFT JOIN attempts a ON a.id = wi.current_attempt_id
		LEFT JOIN LATERAL (
			SELECT tool_name, created_at FROM agent_tool_calls
			WHERE session_id = a.session_id
			ORDER BY created_at DESC
			LIMIT 1
		) tc ON TRUE
		WHERE wi.run_id=$1
		ORDER BY wi.created_at ASC, wi.id ASC`, runID)
	if err != nil {
		return nil, persistence("failed to read directions")
	}
	defer rows.Close()
	out := []contract.DirectionView{}
	for rows.Next() {
		var d contract.DirectionView
		var input []byte
		var updated time.Time
		var finish *string
		var step *string
		var stepAt *time.Time
		if err := rows.Scan(&d.WorkItemID, &d.Objective, &d.Status, &input, &updated, &d.AttemptID, &d.SessionID, &finish, &step, &stepAt); err != nil {
			rows.Close()
			return nil, persistence("failed to scan direction")
		}
		if step != nil {
			d.LatestStep = *step
		}
		if stepAt != nil {
			t := stepAt.UTC()
			d.ProgressAt = &t
		} else {
			t := updated.UTC()
			d.ProgressAt = &t
		}
		if len(input) > 0 && string(input) != "{}" && string(input) != "null" {
			d.LocalBudget = input
		}
		if finish != nil && *finish != "" && (d.Status == "failed" || d.Status == "blocked" || d.Status == "exhausted") {
			code := contract.CodeInvalidState
			if d.Status == "exhausted" {
				code = contract.CodeWorkerExhausted
			}
			d.Error = &contract.ErrorBody{Code: code, Scope: contract.ScopeWorkItem, Message: *finish}
		}
		d.AllowedActions = allowedActions(runStatus, d.Status)
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, persistence("failed to read directions")
	}
	rows.Close()
	for i := range out {
		refs, err := sourceRefs(ctx, tx, out[i].WorkItemID)
		if err != nil {
			return nil, err
		}
		out[i].SourceRefs = refs
		if err := out[i].Validate(); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func sourceRefs(ctx context.Context, tx pgx.Tx, workItemID uuid.UUID) ([]string, error) {
	rows, err := tx.Query(ctx, `SELECT source_observation_id FROM work_item_sources WHERE work_item_id=$1 ORDER BY created_at ASC`, workItemID)
	if err != nil {
		return nil, persistence("failed to read direction sources")
	}
	defer rows.Close()
	var refs []string
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, persistence("failed to scan direction source")
		}
		refs = append(refs, "observation:"+id.String())
	}
	return refs, rows.Err()
}

func allowedActions(runStatus, itemStatus string) []contract.CommandType {
	if model.RunStatus(runStatus).IsTerminal() || runStatus == string(model.RunStatusStopping) {
		return nil
	}
	switch itemStatus {
	case string(model.WorkItemStatusRunning):
		return []contract.CommandType{contract.CmdSteerWork, contract.CmdKillWork}
	case string(model.WorkItemStatusExhausted):
		return []contract.CommandType{contract.CmdContinueWork}
	case string(model.WorkItemStatusFailed), string(model.WorkItemStatusBlocked):
		return []contract.CommandType{contract.CmdRetryWork}
	default:
		return nil
	}
}

func readGoals(ctx context.Context, tx pgx.Tx, runID uuid.UUID) ([]json.RawMessage, error) {
	rows, err := tx.Query(ctx, `
		SELECT id, state, payload FROM exploration_nodes
		WHERE run_id=$1 AND kind='goal' AND delete_reason IS NULL
		ORDER BY created_at ASC`, runID)
	if err != nil {
		return nil, persistence("failed to read goals")
	}
	defer rows.Close()
	goals := []json.RawMessage{}
	for rows.Next() {
		var id uuid.UUID
		var state string
		var payload []byte
		if err := rows.Scan(&id, &state, &payload); err != nil {
			return nil, persistence("failed to scan goal")
		}
		body, err := json.Marshal(map[string]any{"id": id, "state": state, "payload": json.RawMessage(payload)})
		if err != nil {
			return nil, persistence("failed to encode goal")
		}
		goals = append(goals, body)
	}
	return goals, rows.Err()
}

func readEvents(ctx context.Context, tx pgx.Tx, runID uuid.UUID, after int64, limit int) ([]contract.WorkbenchEvent, int64, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT seq, type, payload FROM run_events
		WHERE run_id=$1 AND seq > $2
		ORDER BY seq ASC
		LIMIT $3`, runID, after, limit+1)
	if err != nil {
		return nil, 0, false, persistence("failed to read events")
	}
	defer rows.Close()
	events := []contract.WorkbenchEvent{}
	for rows.Next() {
		var ev contract.WorkbenchEvent
		var payload []byte
		if err := rows.Scan(&ev.Seq, &ev.Type, &payload); err != nil {
			return nil, 0, false, persistence("failed to scan event")
		}
		ev.Payload = redactPayload(payload)
		events = append(events, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, false, persistence("failed to read events")
	}
	hasMore := false
	if len(events) > limit {
		hasMore = true
		events = events[:limit]
	}
	last := after
	if len(events) > 0 {
		last = events[len(events)-1].Seq
	}
	return events, last, hasMore, nil
}

func readMainCursor(ctx context.Context, tx pgx.Tx, runID uuid.UUID) (contract.MainConversationCursor, error) {
	cursor := contract.MainConversationCursor{}
	var conversationID uuid.UUID
	err := tx.QueryRow(ctx, `SELECT id FROM main_conversations WHERE run_id=$1`, runID).Scan(&conversationID)
	if err == pgx.ErrNoRows {
		return cursor, nil
	}
	if err != nil {
		return cursor, persistence("failed to read main conversation")
	}
	if err := tx.QueryRow(ctx, `SELECT COALESCE(MAX(seq), 0) FROM main_messages WHERE conversation_id=$1`, conversationID).Scan(&cursor.LastSeq); err != nil {
		return cursor, persistence("failed to read main cursor")
	}
	var active uuid.UUID
	err = tx.QueryRow(ctx, `
		SELECT id FROM main_turns
		WHERE run_id=$1 AND status IN ('queued', 'running')
		ORDER BY created_at ASC, id ASC
		LIMIT 1`, runID).Scan(&active)
	if err == nil {
		cursor.ActiveTurnID = &active
	} else if err != pgx.ErrNoRows {
		return cursor, persistence("failed to read active turn")
	}
	return cursor, nil
}

func redactPayload(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return json.RawMessage(`{}`)
	}
	redacted, err := json.Marshal(redactValue(value))
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return redacted
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, child := range typed {
			if secretKey(key) {
				continue
			}
			out[key] = redactValue(child)
		}
		return out
	case []any:
		for i := range typed {
			typed[i] = redactValue(typed[i])
		}
		return typed
	default:
		return value
	}
}

func secretKey(key string) bool {
	switch strings.ToLower(strings.TrimSpace(key)) {
	case "authorization", "api_key", "cookie", "token":
		return true
	default:
		return false
	}
}
