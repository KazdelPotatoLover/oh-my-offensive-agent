package controlplane

import (
	"encoding/json"
	"strings"

	"agent-workbench/backend/internal/contract"
)

var expansionKeys = []string{
	"scope",
	"allow",
	"budget",
	"max_parallel_workers",
	"grant_budget",
	"additional_model_calls",
	"additional_tool_calls",
	"additional_seconds",
}

// rejectExpansion refuses payload fields that would widen scope or budget.
// grant_budget is the only command allowed to carry grant amounts, and only
// a human principal reaches that command.
func rejectExpansion(cmd contract.Command) error {
	if len(cmd.Payload) == 0 || string(cmd.Payload) == "null" {
		return nil
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(cmd.Payload, &raw); err != nil {
		return invalidInput("command payload must be an object")
	}
	if cmd.Type == contract.CmdGrantBudget {
		return nil
	}
	for key := range raw {
		normalized := strings.ToLower(strings.TrimSpace(key))
		for _, banned := range expansionKeys {
			if normalized == banned {
				return unauthorized("command cannot expand scope or budget")
			}
		}
	}
	return nil
}
