package service

import (
	"encoding/json"
	"errors"
	"testing"

	"agent-workbench/backend/internal/contract"
)

func TestExplicitNetworkPolicyStaysAbsentUnlessSubmitted(t *testing.T) {
	raw, err := explicitNetworkPolicy(json.RawMessage(`{"executor":"orchestrator"}`), json.RawMessage(`{"mode":"discovery","seeds":["http://127.0.0.1:9/"]}`))
	if err != nil {
		t.Fatal(err)
	}
	if raw != nil {
		t.Fatalf("missing policy was invented: %s", raw)
	}
}

func TestExplicitNetworkPolicyRejectsDisagreement(t *testing.T) {
	_, err := explicitNetworkPolicy(
		json.RawMessage(`{"network_policy":{"template":"readonly","origins":[{"scheme":"https","host":"127.0.0.1"}]}}`),
		json.RawMessage(`{"network_policy":{"template":"active","origins":[{"scheme":"http","host":"127.0.0.1"}]}}`),
	)
	var coded *contract.Error
	if !errors.As(err, &coded) || coded.Code != contract.CodeInvalidInput {
		t.Fatalf("got %v", err)
	}
}

func TestExplicitNetworkPolicyKeepsSubmittedObject(t *testing.T) {
	raw, err := explicitNetworkPolicy(json.RawMessage(`{"network_policy":{"template":"readonly","origins":[{"scheme":"https","host":"app.internal"}]}}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Template string `json:"template"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Template != "readonly" {
		t.Fatalf("template %q", doc.Template)
	}
}
