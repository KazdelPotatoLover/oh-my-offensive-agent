package contract

import (
	"bytes"
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	ProtocolChatCompletions = "chat_completions"
	ProtocolResponses       = "responses"
	HeaderIdempotencyKey    = "Idempotency-Key"
)

var runtimeNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:-]{0,127}$`)

// AgentRoleConfig points at a server-configured profile and model alias.
// It is not executable code and contains no credential.
type AgentRoleConfig struct {
	RuntimeProfile string `json:"runtime_profile"`
	Model          string `json:"model"`
}

func (c AgentRoleConfig) Validate() error {
	if !runtimeNamePattern.MatchString(c.RuntimeProfile) {
		return invalidInput("runtime_profile must be a server profile alias")
	}
	if !runtimeNamePattern.MatchString(c.Model) {
		return invalidInput("model must be a configured model alias")
	}
	return nil
}

// AgentsConfig is stored at execution_input.agents. An empty object means every
// role falls back to the historical single agent_config.
type AgentsConfig struct {
	MainAgent AgentRoleConfig `json:"mainagent"`
	Planner   AgentRoleConfig `json:"planner"`
	Worker    AgentRoleConfig `json:"worker"`
	Verifier  AgentRoleConfig `json:"verifier"`
}

func (c AgentsConfig) Validate() error {
	if err := c.MainAgent.Validate(); err != nil {
		return err
	}
	if err := c.Planner.Validate(); err != nil {
		return err
	}
	if err := c.Worker.Validate(); err != nil {
		return err
	}
	return c.Verifier.Validate()
}

// ParseAgentsConfig reads execution_input. A missing agents field is the
// legacy single-config fallback, not an error. A present but invalid object
// is rejected instead of being partially applied.
func ParseAgentsConfig(executionInput json.RawMessage) (AgentsConfig, bool, error) {
	trimmed := bytes.TrimSpace(executionInput)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return AgentsConfig{}, false, nil
	}
	var wrapped struct {
		Agents *AgentsConfig `json:"agents"`
	}
	if err := json.Unmarshal(trimmed, &wrapped); err != nil {
		return AgentsConfig{}, false, invalidInput("execution_input is not an object")
	}
	if wrapped.Agents == nil {
		return AgentsConfig{}, false, nil
	}
	if err := wrapped.Agents.Validate(); err != nil {
		return AgentsConfig{}, true, err
	}
	return *wrapped.Agents, true, nil
}

func (c AgentsConfig) For(role Role) (AgentRoleConfig, error) {
	switch role {
	case RoleMainAgent:
		return c.MainAgent, c.MainAgent.Validate()
	case RolePlanner:
		return c.Planner, c.Planner.Validate()
	case RoleWorker:
		return c.Worker, c.Worker.Validate()
	case RoleVerifier:
		return c.Verifier, c.Verifier.Validate()
	default:
		return AgentRoleConfig{}, invalidInput("unknown role")
	}
}

// RuntimeConfigRevision is the non-sensitive snapshot. A running epoch does not
// change it in place. The next revision names the epoch where it applies.
type RuntimeConfigRevision struct {
	Version       int          `json:"version"`
	EpochNumber   int          `json:"epoch_number"`
	Agents        AgentsConfig `json:"agents"`
	ProviderAlias string       `json:"provider_alias,omitempty"`
	Protocol      string       `json:"protocol,omitempty"`
}

func (r RuntimeConfigRevision) Validate() error {
	if r.Version < 1 || r.EpochNumber < 1 {
		return invalidInput("runtime config revision requires version and epoch")
	}
	if r.Protocol != "" && r.Protocol != ProtocolChatCompletions && r.Protocol != ProtocolResponses {
		return invalidInput("protocol must be chat_completions or responses")
	}
	if r.ProviderAlias != "" && !runtimeNamePattern.MatchString(r.ProviderAlias) {
		return invalidInput("provider_alias must be a server alias")
	}
	return r.Agents.Validate()
}

// NextConfigRevision records a change that becomes active at nextEpoch.
func NextConfigRevision(current RuntimeConfigRevision, next AgentsConfig, nextEpoch int) (RuntimeConfigRevision, error) {
	if nextEpoch <= current.EpochNumber {
		return RuntimeConfigRevision{}, &Error{
			Code:            CodeStaleEpoch,
			Scope:           ScopeEpoch,
			Message:         "runtime config changes apply on a newer epoch",
			SuggestedAction: ActionNone,
		}
	}
	if err := next.Validate(); err != nil {
		return RuntimeConfigRevision{}, err
	}
	version := current.Version + 1
	if version < 1 {
		version = 1
	}
	out := RuntimeConfigRevision{
		Version:       version,
		EpochNumber:   nextEpoch,
		Agents:        next,
		ProviderAlias: current.ProviderAlias,
		Protocol:      current.Protocol,
	}
	return out, nil
}

// ModelMessage is the portable conversation view. Native provider payloads stay
// inside the provider adapter and are not part of this struct.
type ModelMessage struct {
	Role       string          `json:"role"`
	Content    string          `json:"content"`
	ToolCallID string          `json:"tool_call_id,omitempty"`
	ToolCalls  []ModelToolCall `json:"tool_calls,omitempty"`
}

type ModelToolCall struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Arguments  string `json:"arguments"`
	Incomplete bool   `json:"incomplete,omitempty"`
	Status     string `json:"status,omitempty"`
}

type ModelToolDef struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type ModelCallOpts struct {
	ModelName string
	Timeout   time.Duration
	Protocol  string
}

type ModelUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ModelCallRequest is one real provider invocation. Adapters must not retry it.
type ModelCallRequest struct {
	Scope    ExecutionScope
	Messages []ModelMessage
	Tools    []ModelToolDef
	Opts     ModelCallOpts
}

// ModelCallResult.Executable is false when arguments or status are incomplete,
// truncated, or refused. Callers must not execute those tool calls or rewrite
// them into a successful call.
type ModelCallResult struct {
	Content           string          `json:"content"`
	ToolCalls         []ModelToolCall `json:"tool_calls,omitempty"`
	FinishReason      string          `json:"finish_reason"`
	Usage             *ModelUsage     `json:"usage,omitempty"`
	ProviderRequestID string          `json:"provider_request_id,omitempty"`
	Executable        bool            `json:"executable"`
	ProtocolError     *ErrorBody      `json:"protocol_error,omitempty"`
}

// RejectIncompleteToolCalls reports invalid_model_output when a result is not
// executable. It does not repair arguments.
func RejectIncompleteToolCalls(result *ModelCallResult) error {
	if result == nil {
		return &Error{
			Code:            CodeInvalidModelOutput,
			Scope:           ScopeSession,
			Message:         "provider returned no result",
			Retryable:       true,
			SuggestedAction: ActionRetryDirection,
		}
	}
	if result.ProtocolError != nil || !result.Executable {
		message := "model output is not executable"
		if result.ProtocolError != nil && result.ProtocolError.Message != "" {
			message = result.ProtocolError.Message
		}
		return &Error{
			Code:            CodeInvalidModelOutput,
			Scope:           ScopeSession,
			Message:         message,
			Retryable:       true,
			SuggestedAction: ActionRetryDirection,
			SourceID:        result.ProviderRequestID,
		}
	}
	for _, call := range result.ToolCalls {
		if call.Incomplete || strings.EqualFold(call.Status, "incomplete") {
			return &Error{
				Code:            CodeInvalidModelOutput,
				Scope:           ScopeSession,
				Message:         "incomplete function arguments or status",
				Retryable:       true,
				SuggestedAction: ActionRetryDirection,
				SourceID:        result.ProviderRequestID,
			}
		}
	}
	return nil
}

// ModelCaller performs one upstream request. Implementations must not loop,
// sleep and repeat, or complete missing tool arguments.
type ModelCaller interface {
	Call(ctx context.Context, req ModelCallRequest) (*ModelCallResult, error)
}

// ResolvedRuntime is the role binding returned by RuntimeResolver. Caller and
// Policy are nil until a real resolver is injected.
type ResolvedRuntime struct {
	Role                 Role
	Profile              string
	Model                string
	Protocol             string
	Revision             RuntimeConfigRevision
	Caller               ModelCaller
	Policy               ModelCallPolicy
	NonSensitiveSnapshot json.RawMessage
}

// RuntimeResolver maps a scope and profile alias to a role runtime. profile is
// a server alias, not caller-supplied code.
type RuntimeResolver interface {
	Resolve(ctx context.Context, scope ExecutionScope, profile string) (*ResolvedRuntime, error)
}

// ModelCallLease is one admitted upstream request. A retry needs a new lease.
type ModelCallLease struct {
	ID         uuid.UUID
	Scope      ExecutionScope
	AcquiredAt time.Time
}

// ModelCallRecord is written after the real Provider.Call returns, including
// failures. Attempted is false only when Call was not invoked.
type ModelCallRecord struct {
	LeaseID   uuid.UUID
	Attempted bool
	Err       error
	Result    *ModelCallResult
}

// ModelCallPolicy admits, counts, limits, and logs each real provider call.
// Waiting must honor context cancellation. A wait is not a failed worker and
// not a successful call.
type ModelCallPolicy interface {
	Acquire(ctx context.Context, scope ExecutionScope) (*ModelCallLease, error)
	Complete(ctx context.Context, lease ModelCallLease, record ModelCallRecord) error
}

// CallOnce is the frozen provider sequence: Acquire, one Call, Complete.
// It does not retry. A nil policy or caller fails closed before any call.
func CallOnce(ctx context.Context, policy ModelCallPolicy, caller ModelCaller, scope ExecutionScope, req ModelCallRequest) (*ModelCallResult, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if policy == nil || caller == nil || isUnimplemented(policy) || isUnimplemented(caller) {
		return nil, ErrNotImplemented
	}
	lease, err := policy.Acquire(ctx, scope)
	if err != nil {
		return nil, err
	}
	if lease == nil || lease.ID == uuid.Nil {
		return nil, &Error{Code: CodePersistenceError, Scope: ScopeSession, Message: "model call lease missing"}
	}
	result, callErr := caller.Call(ctx, req)
	completeErr := policy.Complete(ctx, *lease, ModelCallRecord{
		LeaseID:   lease.ID,
		Attempted: true,
		Err:       callErr,
		Result:    result,
	})
	if callErr != nil {
		return nil, callErr
	}
	if completeErr != nil {
		return nil, completeErr
	}
	if err := RejectIncompleteToolCalls(result); err != nil {
		return nil, err
	}
	return result, nil
}
