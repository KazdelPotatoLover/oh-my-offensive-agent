package service

import (
	"encoding/json"

	"agent-workbench/backend/internal/contract"
)

// explicitNetworkPolicy returns a user-submitted network_policy from task
// config or scope. A missing field stays missing. This function does not
// invent HTTPS or extra methods from a discovery policy.
func explicitNetworkPolicy(taskConfig, taskScope json.RawMessage) (json.RawMessage, error) {
	fromConfig, configErr := rawObjectField(taskConfig, "network_policy")
	if configErr != nil {
		return nil, configErr
	}
	fromScope, scopeErr := rawObjectField(taskScope, "network_policy")
	if scopeErr != nil {
		return nil, scopeErr
	}
	if fromConfig == nil && fromScope == nil {
		return nil, nil
	}
	if fromConfig != nil && fromScope != nil {
		left, err := canonicalJSON(fromConfig)
		if err != nil {
			return nil, invalidNetworkPolicy("network_policy in config is not JSON")
		}
		right, err := canonicalJSON(fromScope)
		if err != nil {
			return nil, invalidNetworkPolicy("network_policy in scope is not JSON")
		}
		if string(left) != string(right) {
			return nil, invalidNetworkPolicy("network_policy in config and scope disagree")
		}
	}
	if fromConfig != nil {
		return fromConfig, nil
	}
	return fromScope, nil
}

func rawObjectField(doc json.RawMessage, key string) (json.RawMessage, error) {
	if len(doc) == 0 || string(doc) == "null" {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(doc, &fields); err != nil {
		return nil, nil
	}
	raw, ok := fields[key]
	if !ok || len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	if !json.Valid(raw) {
		return nil, invalidNetworkPolicy("network_policy is not JSON")
	}
	return raw, nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func invalidNetworkPolicy(message string) error {
	return &contract.Error{
		Code:            contract.CodeInvalidInput,
		Scope:           contract.ScopeRequest,
		Message:         message,
		SuggestedAction: contract.ActionNone,
	}
}
