package controlplane

import (
	"encoding/json"
	"testing"

	"agent-workbench/backend/internal/contract"
	"agent-workbench/backend/internal/model"
)

func TestMainInteractionLimitsDoNotTrapV1AtEight(t *testing.T) {
	limited, lifetime, turnCap := mainInteractionLimits(nil)
	if !limited || lifetime != contract.SuggestedExplorationTemplate().MainReservedModelCalls || turnCap != 0 {
		t.Fatalf("legacy limits limited=%v lifetime=%d turn=%d", limited, lifetime, turnCap)
	}
	if !mainInteractionStopped(true, 8, 8, 8, 0) {
		t.Fatal("legacy reserve must still stop at 8")
	}

	policy := model.BuiltinExplorationPolicy()
	raw, err := json.Marshal(map[string]any{"execution_policy": policy})
	if err != nil {
		t.Fatal(err)
	}
	limited, lifetime, turnCap = mainInteractionLimits(raw)
	if limited || lifetime != 0 || turnCap != 0 {
		t.Fatalf("v1 unlimited parsed as limited=%v lifetime=%d turn=%d", limited, lifetime, turnCap)
	}
	if mainInteractionStopped(limited, 9, lifetime, 9, turnCap) {
		t.Fatal("v1 main stopped after 9 interactions")
	}

	policy.LegacyReserves.MainModelCalls = 4
	raw, err = json.Marshal(map[string]any{"execution_policy": policy})
	if err != nil {
		t.Fatal(err)
	}
	limited, lifetime, turnCap = mainInteractionLimits(raw)
	if !limited || lifetime != 4 {
		t.Fatalf("explicit main reserve was dropped: limited=%v lifetime=%d", limited, lifetime)
	}
	if !mainInteractionStopped(limited, 4, lifetime, 4, turnCap) {
		t.Fatal("explicit reserve did not stop")
	}

	policy.LegacyReserves.MainModelCalls = 0
	policy.Agents.MainAgent.MaxTurns = 2
	raw, err = json.Marshal(map[string]any{"execution_policy": policy})
	if err != nil {
		t.Fatal(err)
	}
	limited, lifetime, turnCap = mainInteractionLimits(raw)
	if limited || turnCap != 2 {
		t.Fatalf("turn cap limited=%v turn=%d", limited, turnCap)
	}
	if mainInteractionStopped(limited, 20, lifetime, 1, turnCap) {
		t.Fatal("turn cap fired before two responses")
	}
	if !mainInteractionStopped(limited, 20, lifetime, 2, turnCap) {
		t.Fatal("turn cap did not stop")
	}
}
