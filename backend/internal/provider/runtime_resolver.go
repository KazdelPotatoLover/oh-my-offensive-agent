package provider

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

const (
	BudgetSourceMainReserved       = "main_reserved"
	BudgetSourcePlannerReserved    = "planner_reserved"
	BudgetSourceWorkerLocalThenRun = "worker_local_then_run"
	BudgetSourceRunGlobal          = "run_global"
)

// RoleCallPolicy is the non-sensitive admission rule for one role.
// Pause, stop, and deadline can cancel a queued or backing-off model call.
type RoleCallPolicy struct {
	Role             contract.Role `json:"role"`
	BudgetSource     string        `json:"budget_source"`
	PauseCancels     bool          `json:"pause_cancels"`
	StopCancels      bool          `json:"stop_cancels"`
	DeadlineCancels  bool          `json:"deadline_cancels"`
	ModelInFlightCap string        `json:"model_inflight_cap"`
}

func RoleCallPolicyFor(role contract.Role) (RoleCallPolicy, error) {
	policy := RoleCallPolicy{
		Role:             role,
		PauseCancels:     true,
		StopCancels:      true,
		DeadlineCancels:  true,
		ModelInFlightCap: "shared_upstream_limiter",
	}
	switch role {
	case contract.RoleMainAgent:
		policy.BudgetSource = BudgetSourceMainReserved
	case contract.RolePlanner:
		policy.BudgetSource = BudgetSourcePlannerReserved
	case contract.RoleWorker:
		policy.BudgetSource = BudgetSourceWorkerLocalThenRun
	case contract.RoleVerifier:
		policy.BudgetSource = BudgetSourceRunGlobal
	default:
		return RoleCallPolicy{}, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: "unknown role"}
	}
	return policy, nil
}

// ModelAlias is a server-side model binding. It contains no credential.
type ModelAlias struct {
	Alias         string
	Model         string
	Protocol      string
	ProviderAlias string
}

// StaticRuntimeResolver maps roles to aliases. A missing agents config falls
// back to one historical binding. An unknown alias does not fall back.
type StaticRuntimeResolver struct {
	Historical ModelAlias
	Aliases    map[string]ModelAlias
	Agents     *contract.AgentsConfig
	Version    int
	Epoch      int
	Caller     contract.ModelCaller
	Policy     contract.ModelCallPolicy
}

func (r *StaticRuntimeResolver) Resolve(_ context.Context, scope contract.ExecutionScope, profile string) (*contract.ResolvedRuntime, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	rolePolicy, err := RoleCallPolicyFor(scope.Role)
	if err != nil {
		return nil, err
	}
	binding, err := r.binding(scope.Role, profile)
	if err != nil {
		return nil, err
	}
	revision := contract.RuntimeConfigRevision{
		Version:       maxInt(r.Version, 1),
		EpochNumber:   maxInt(r.Epoch, scope.EpochNumber),
		Agents:        r.snapshotAgents(binding),
		ProviderAlias: binding.ProviderAlias,
		Protocol:      binding.Protocol,
	}
	if err := revision.Validate(); err != nil {
		return nil, err
	}
	raw, err := nonSensitiveSnapshot(revision, rolePolicy)
	if err != nil {
		return nil, err
	}
	return &contract.ResolvedRuntime{
		Role:                 scope.Role,
		Profile:              binding.Alias,
		Model:                binding.Model,
		Protocol:             binding.Protocol,
		Revision:             revision,
		Caller:               r.Caller,
		Policy:               r.Policy,
		NonSensitiveSnapshot: raw,
	}, nil
}

func (r *StaticRuntimeResolver) binding(role contract.Role, profile string) (ModelAlias, error) {
	requested := strings.TrimSpace(profile)
	if requested != "" {
		found, ok := r.lookup(requested)
		if !ok {
			return ModelAlias{}, unknownAlias(requested)
		}
		return found, nil
	}
	if r.Agents == nil {
		if r.Historical.Alias == "" || r.Historical.Model == "" {
			return ModelAlias{}, unknownAlias("historical")
		}
		return r.Historical, nil
	}
	roleConfig, err := r.Agents.For(role)
	if err != nil {
		return ModelAlias{}, err
	}
	found, ok := r.lookup(roleConfig.RuntimeProfile)
	if !ok {
		found, ok = r.lookup(roleConfig.Model)
	}
	if !ok || (found.Model != roleConfig.Model && found.Alias != roleConfig.Model) {
		return ModelAlias{}, unknownAlias(roleConfig.Model)
	}
	return found, nil
}

func (r *StaticRuntimeResolver) lookup(alias string) (ModelAlias, bool) {
	if alias == "" {
		return ModelAlias{}, false
	}
	if r.Aliases != nil {
		if found, ok := r.Aliases[alias]; ok {
			return found, true
		}
	}
	if alias == r.Historical.Alias || alias == r.Historical.Model {
		return r.Historical, r.Historical.Alias != "" && r.Historical.Model != ""
	}
	return ModelAlias{}, false
}

func (r *StaticRuntimeResolver) snapshotAgents(binding ModelAlias) contract.AgentsConfig {
	if r.Agents != nil {
		return *r.Agents
	}
	role := contract.AgentRoleConfig{RuntimeProfile: binding.Alias, Model: binding.Model}
	return contract.AgentsConfig{MainAgent: role, Planner: role, Worker: role, Verifier: role}
}

func nonSensitiveSnapshot(revision contract.RuntimeConfigRevision, policy RoleCallPolicy) (json.RawMessage, error) {
	payload := struct {
		contract.RuntimeConfigRevision
		RolePolicy RoleCallPolicy `json:"role_policy"`
	}{RuntimeConfigRevision: revision, RolePolicy: policy}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	lower := strings.ToLower(string(raw))
	for _, banned := range []string{"api_key", "authorization", "bearer ", "sk-", "llm_api_key"} {
		if strings.Contains(lower, banned) {
			return nil, &contract.Error{Code: contract.CodeInvalidInput, Scope: contract.ScopeRequest, Message: "runtime snapshot contained sensitive material"}
		}
	}
	return raw, nil
}

func unknownAlias(alias string) error {
	return &contract.Error{
		Code:            contract.CodeInvalidInput,
		Scope:           contract.ScopeRequest,
		Message:         "unknown runtime profile alias",
		SuggestedAction: contract.ActionNone,
		SourceID:        alias,
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// SharedCallPolicy admits one upstream request at a time through the shared limiter.
// Complete releases the slot. It does not retry.
type SharedCallPolicy struct {
	Limiter *RequestLimiter
}

func (p *SharedCallPolicy) Acquire(ctx context.Context, scope contract.ExecutionScope) (*contract.ModelCallLease, error) {
	if err := scope.Validate(); err != nil {
		return nil, err
	}
	if p == nil || p.Limiter == nil {
		return nil, contract.ErrNotImplemented
	}
	if err := p.Limiter.Acquire(ctx); err != nil {
		return nil, err
	}
	return &contract.ModelCallLease{ID: uuid.New(), Scope: scope, AcquiredAt: time.Now()}, nil
}

func (p *SharedCallPolicy) Complete(_ context.Context, lease contract.ModelCallLease, _ contract.ModelCallRecord) error {
	if p != nil && p.Limiter != nil {
		p.Limiter.Release()
	}
	if lease.ID == uuid.Nil {
		return &contract.Error{Code: contract.CodePersistenceError, Scope: contract.ScopeSession, Message: "model call lease missing"}
	}
	return nil
}
