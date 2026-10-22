package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/service"
	"agent-workbench/backend/internal/tools"

	"github.com/google/uuid"
)

// Supervision builds planner and main control toolsets from one factory.
// Both planner profiles expose the same control and read tools. Worker and
// verifier profiles are refused so the composition root can delegate them.
type Supervision struct {
	Runs       *service.RunService
	Controller contract.ExecutionController
}

func NewSupervision(runs *service.RunService, controller contract.ExecutionController) *Supervision {
	return &Supervision{Runs: runs, Controller: controller}
}

var _ contract.ToolsetFactory = (*Supervision)(nil)

func (s *Supervision) Assemble(_ context.Context, scope contract.ExecutionScope, profile contract.ToolProfile) (*contract.AssembledToolset, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	names, role, err := profileNames(profile)
	if err != nil {
		return nil, err
	}
	return &contract.AssembledToolset{Role: role, Profile: profile, Names: names}, nil
}

// Registry returns executable tools for a scope. mutations is false for a
// status question so the model cannot call a control tool. grant_budget,
// shell, and worker start are never registered.
func (s *Supervision) Registry(scope contract.ExecutionScope, principal contract.Principal, keyFn func(contract.Command) string, mutations bool) (*tools.Registry, error) {
	if s == nil || s.Controller == nil || s.Runs == nil {
		return nil, persistence("supervision tools are not configured")
	}
	reg := tools.NewRegistry()
	readerScope := tools.AdapterContext{RunID: scope.RunID, Service: s.Runs}
	if scope.SessionID != nil {
		readerScope.SessionID = *scope.SessionID
	}
	tools.RegisterArtifactReaders(reg, readerScope)
	reg.Register(tools.NewGraphOverviewTool(scope.RunID, s.Runs))
	reg.Register(tools.NewNodeDetailTool(scope.RunID, s.Runs))
	reg.Register(tools.NewListFactsTool(scope.RunID, s.Runs))
	reg.Register(tools.NewListWorkerTracesTool(scope.RunID, s.Runs))
	reg.Register(tools.NewGetWorkerTraceTool(scope.RunID, s.Runs))
	if !mutations {
		return reg, nil
	}
	names := contract.PlannerControlTools
	if principal.Kind == contract.PrincipalMainAgent {
		names = mainMutationTools()
	}
	for _, name := range names {
		reg.Register(newControlTool(name, s.Controller, scope, principal, keyFn))
	}
	return reg, nil
}

func profileNames(profile contract.ToolProfile) ([]string, contract.Role, error) {
	read := []string{"graph_overview", "node_detail", "list_facts", "get_worker_trace", "list_worker_traces", "search_http_traffic", "get_http_exchange", "list_artifacts", "read_artifact", "get_worker_output", "list_capability_profiles"}
	switch profile {
	case contract.ProfilePlannerInitial, contract.ProfilePlannerFollowup:
		names := append(append([]string{}, contract.PlannerControlTools...), read...)
		return names, contract.RolePlanner, nil
	case contract.ProfileMainAgent:
		names := append([]string{"get_situation"}, read...)
		names = append(names, mainMutationTools()...)
		return names, contract.RoleMainAgent, nil
	case contract.ProfileWorker, contract.ProfileVerifier:
		return nil, "", &contract.Error{
			Code:            contract.CodeNotImplemented,
			Scope:           contract.ScopeRequest,
			Message:         "worker and verifier toolsets are owned by their execution modules",
			SuggestedAction: contract.ActionNone,
		}
	default:
		return nil, "", invalidInput("unknown tool profile")
	}
}

func mainMutationTools() []string {
	return []string{
		"hint",
		"add_intent",
		"steer_work",
		"kill_work",
		"continue_work",
		"retry_work",
		"pause_run",
		"resume_run",
		"stop_run",
		"start_epoch",
	}
}

type controlTool struct {
	name        string
	description string
	parameters  json.RawMessage
	controller  contract.ExecutionController
	scope       contract.ExecutionScope
	principal   contract.Principal
	keyFn       func(contract.Command) string
}

func newControlTool(name string, controller contract.ExecutionController, scope contract.ExecutionScope, principal contract.Principal, keyFn func(contract.Command) string) *controlTool {
	return &controlTool{
		name:        name,
		description: "Apply " + name + " through the shared execution controller. A text reply does not change run state.",
		parameters:  controlSchema(name),
		controller:  controller,
		scope:       scope,
		principal:   principal,
		keyFn:       keyFn,
	}
}

func (t *controlTool) Name() string                { return t.name }
func (t *controlTool) Description() string         { return t.description }
func (t *controlTool) Parameters() json.RawMessage { return t.parameters }

func (t *controlTool) Validate(argumentsJSON []byte) error {
	if _, err := t.command(argumentsJSON); err != nil {
		return err
	}
	return nil
}

func (t *controlTool) Execute(ctx context.Context, argumentsJSON []byte) (string, error) {
	cmd, err := t.command(argumentsJSON)
	if err != nil {
		return envelopeJSON(err), nil
	}
	key := ""
	if t.keyFn != nil {
		key = t.keyFn(cmd)
	}
	if key == "" {
		key = defaultToolKey(t.scope, cmd)
	}
	result, err := t.controller.Apply(ctx, t.principal, t.scope, key, cmd)
	if err != nil {
		var ce *contract.Error
		if errors.As(err, &ce) && ce.Code != contract.CodePersistenceError && ce.Code != contract.CodeExecutionUnavailable {
			return envelopeJSON(ce), nil
		}
		return "", err
	}
	body, err := json.Marshal(result)
	if err != nil {
		return "", persistence("failed to encode command result")
	}
	return string(body), nil
}

func (t *controlTool) command(argumentsJSON []byte) (contract.Command, error) {
	var args map[string]any
	if len(argumentsJSON) > 0 {
		if err := json.Unmarshal(argumentsJSON, &args); err != nil {
			return contract.Command{}, invalidInput("tool arguments must be an object")
		}
	}
	if args == nil {
		args = map[string]any{}
	}
	for key := range args {
		normalized := strings.ToLower(key)
		for _, banned := range expansionKeys {
			if normalized == banned {
				return contract.Command{}, unauthorized("tool arguments cannot expand scope or budget")
			}
		}
		if normalized == "principal" || normalized == "role" || normalized == "explicit_user_command" {
			return contract.Command{}, unauthorized("principal is established by the server")
		}
	}
	cmd := contract.Command{Type: contract.CommandType(t.name), ExpectedEpoch: t.scope.EpochNumber}
	if contract.CommandType(t.name).TargetsWorkItem() {
		id, err := parseArgUUID(args, "work_item_id", true)
		if err != nil {
			return contract.Command{}, err
		}
		cmd.WorkItemID = id
		attempt, err := parseArgUUID(args, "expected_attempt_id", false)
		if err != nil {
			return contract.Command{}, err
		}
		cmd.ExpectedAttemptID = attempt
	}
	payload := map[string]any{}
	switch contract.CommandType(t.name) {
	case contract.CmdSteerWork:
		instruction, _ := args["instruction"].(string)
		if strings.TrimSpace(instruction) == "" {
			return contract.Command{}, invalidInput("instruction is required")
		}
		payload["instruction"] = strings.TrimSpace(instruction)
	case contract.CmdKillWork:
		if reason, _ := args["reason"].(string); strings.TrimSpace(reason) != "" {
			payload["reason"] = strings.TrimSpace(reason)
		}
	case contract.CmdContinueWork, contract.CmdRetryWork:
		if instruction, _ := args["instruction"].(string); strings.TrimSpace(instruction) != "" {
			payload["instruction"] = strings.TrimSpace(instruction)
		}
	case contract.CmdHint:
		text, _ := args["text"].(string)
		if strings.TrimSpace(text) == "" {
			return contract.Command{}, invalidInput("text is required")
		}
		payload["text"] = strings.TrimSpace(text)
	case contract.CmdAddIntent:
		summary, _ := args["summary"].(string)
		if strings.TrimSpace(summary) == "" {
			return contract.Command{}, invalidInput("summary is required")
		}
		payload["summary"] = strings.TrimSpace(summary)
	case contract.CmdStartEpoch:
		reason, _ := args["reason"].(string)
		if strings.TrimSpace(reason) == "" {
			return contract.Command{}, invalidInput("reason is required")
		}
		payload["reason"] = strings.TrimSpace(reason)
		if summary, _ := args["summary"].(string); summary != "" {
			payload["summary"] = summary
		}
	}
	if len(payload) > 0 {
		raw, err := json.Marshal(payload)
		if err != nil {
			return contract.Command{}, invalidInput("failed to encode tool payload")
		}
		cmd.Payload = raw
	}
	if err := cmd.Validate(); err != nil {
		return contract.Command{}, err
	}
	return cmd, nil
}

func controlSchema(name string) json.RawMessage {
	switch contract.CommandType(name) {
	case contract.CmdSteerWork, contract.CmdKillWork, contract.CmdContinueWork, contract.CmdRetryWork:
		return json.RawMessage(`{"type":"object","properties":{"work_item_id":{"type":"string"},"expected_attempt_id":{"type":"string"},"instruction":{"type":"string"},"reason":{"type":"string"}},"required":["work_item_id"]}`)
	case contract.CmdHint:
		return json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)
	case contract.CmdAddIntent:
		return json.RawMessage(`{"type":"object","properties":{"summary":{"type":"string"}},"required":["summary"]}`)
	case contract.CmdStartEpoch:
		return json.RawMessage(`{"type":"object","properties":{"reason":{"type":"string"},"summary":{"type":"string"}},"required":["reason"]}`)
	default:
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
}

func parseArgUUID(args map[string]any, field string, required bool) (*uuid.UUID, error) {
	raw, _ := args[field].(string)
	raw = strings.TrimSpace(raw)
	if raw == "" {
		if required {
			return nil, invalidInput(field + " is required")
		}
		return nil, nil
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return nil, invalidInput(field + " must be a UUID")
	}
	return &id, nil
}

func defaultToolKey(scope contract.ExecutionScope, cmd contract.Command) string {
	target := ""
	if cmd.WorkItemID != nil {
		target = cmd.WorkItemID.String()
	}
	sum := sha256.Sum256(cmd.Payload)
	// A planner round is a new session. Retrying the same call inside that
	// session stays idempotent. A later round must hit the state machine
	// again; replaying an old applied result is not a new plan.
	base := scope.RunID.String()
	if scope.SessionID != nil {
		base = scope.SessionID.String()
	}
	if scope.SourceToolCallID != nil {
		base = scope.SourceToolCallID.String()
	}
	return "tool:" + base + ":" + string(cmd.Type) + ":" + target + ":" + hex.EncodeToString(sum[:8])
}

func envelopeJSON(err error) string {
	var ce *contract.Error
	if !errors.As(err, &ce) {
		ce = persistence("control request failed")
	}
	body, _ := json.Marshal(ce.Envelope())
	return string(body)
}
