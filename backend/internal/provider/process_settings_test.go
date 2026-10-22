package provider

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"agent-workbench/backend/internal/contract"
)

func TestProcessSettingsRejectsOutOfRangeValues(t *testing.T) {
	t.Setenv("LLM_MAX_INFLIGHT", "")
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "")
	t.Setenv("LLM_MODEL_RECOVERY_ATTEMPTS", "")
	settings, err := LoadProcessModelSettings()
	if err != nil || settings != (ProcessModelSettings{}) {
		t.Fatalf("empty env = %+v, %v", settings, err)
	}

	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "8193")
	if _, err := LoadProcessModelSettings(); !errors.Is(err, ErrModelConfigInvalid) {
		t.Fatalf("output cap error = %v", err)
	}
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "-1")
	if _, err := LoadProcessModelSettings(); !errors.Is(err, ErrModelConfigInvalid) {
		t.Fatalf("negative output error = %v", err)
	}
	t.Setenv("LLM_MAX_OUTPUT_TOKENS", "")
	t.Setenv("LLM_MODEL_RECOVERY_ATTEMPTS", "9")
	if _, err := LoadProcessModelSettings(); !errors.Is(err, ErrModelConfigInvalid) {
		t.Fatalf("recovery cap error = %v", err)
	}
	t.Setenv("LLM_MODEL_RECOVERY_ATTEMPTS", "1")
	t.Setenv("LLM_MAX_INFLIGHT", "2")
	limiter, tokens, recovery, err := SessionAdmissionFromEnv()
	if err != nil || limiter.Limit() != 2 || tokens != 0 || recovery.Attempts() != 1 {
		t.Fatalf("admission = limit %d tokens %d attempts %d err %v", limiter.Limit(), tokens, recovery.Attempts(), err)
	}
}

func TestAttachAgentsSnapshotKeepsLegacyAndRejectsUnknownAlias(t *testing.T) {
	historical := ModelAlias{Alias: "historical", Model: "mock", Protocol: FormatChatCompletions, ProviderAlias: "historical"}
	input := map[string]any{"agent_config": map[string]any{"provider": "mock"}}
	if err := AttachAgentsSnapshot(input, json.RawMessage(`{"executor":"orchestrator"}`), historical); err != nil {
		t.Fatal(err)
	}
	if _, ok := input["agents"]; ok {
		t.Fatal("missing agents was copied into execution input")
	}

	input = map[string]any{"agent_config": map[string]any{"provider": "mock", "api_key": "should-not-be-copied"}}
	err := AttachAgentsSnapshot(input, json.RawMessage(`{"agents":{"mainagent":{"runtime_profile":"default","model":"other"},"planner":{"runtime_profile":"default","model":"other"},"worker":{"runtime_profile":"default","model":"other"},"verifier":{"runtime_profile":"default","model":"other"}}}`), historical)
	var coded *contract.Error
	if !errors.As(err, &coded) || coded.Code != contract.CodeInvalidInput {
		t.Fatalf("unknown alias error = %v", err)
	}

	input = map[string]any{"agent_config": map[string]any{"provider": "mock"}}
	task := json.RawMessage(`{"agents":{"mainagent":{"runtime_profile":"historical","model":"mock"},"planner":{"runtime_profile":"historical","model":"mock"},"worker":{"runtime_profile":"historical","model":"mock"},"verifier":{"runtime_profile":"historical","model":"mock"}}}`)
	if err := AttachAgentsSnapshot(input, task, historical); err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"agents"`) || strings.Contains(strings.ToLower(string(raw)), "api_key") || strings.Contains(string(raw), "sk-") {
		t.Fatalf("snapshot = %s", raw)
	}
}
