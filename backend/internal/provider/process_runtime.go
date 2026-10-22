package provider

import (
	"context"
	"encoding/json"
	"sync"

	"agent-workbench/backend/internal/contract"
	"github.com/google/uuid"
)

// ProcessRuntime owns one upstream admission pool shared by runs and Main chat.
// Routing stays in each run's immutable snapshot; only credentials come from env.
type ProcessRuntime struct {
	once     sync.Once
	limiter  *RequestLimiter
	tokens   int
	recovery RecoveryPolicy
	err      error
}

func (r *ProcessRuntime) Admission() (*RequestLimiter, int, RecoveryPolicy, error) {
	r.once.Do(func() { r.limiter, r.tokens, r.recovery, r.err = SessionAdmissionFromEnv() })
	return r.limiter, r.tokens, r.recovery, r.err
}

func (r *ProcessRuntime) ModelCapacity(_ context.Context, _ uuid.UUID) (contract.ModelCapacity, error) {
	l, _, _, err := r.Admission()
	if err != nil {
		return contract.ModelCapacity{}, err
	}
	return l.Snapshot(), nil
}

func ProviderFromInput(raw json.RawMessage, scenario MockScenario) (Provider, error) {
	var input struct {
		AgentConfig map[string]any `json:"agent_config"`
	}
	if err := json.Unmarshal(raw, &input); err != nil {
		return nil, err
	}
	return FromSnapshot(input.AgentConfig, scenario)
}
