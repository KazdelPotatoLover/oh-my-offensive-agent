package controlplane

import (
	"testing"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

func TestDefaultToolKeyIsIdempotentInsideSessionAndFreshAcrossSessions(t *testing.T) {
	runID := uuid.MustParse("11111111-1111-4111-8111-111111111111")
	itemID := uuid.MustParse("22222222-2222-4222-8222-222222222222")
	sessionA := uuid.MustParse("33333333-3333-4333-8333-333333333333")
	sessionB := uuid.MustParse("44444444-4444-4444-8444-444444444444")
	cmd := contract.Command{
		Type:          contract.CmdContinueWork,
		ExpectedEpoch: 1,
		WorkItemID:    &itemID,
		Payload:       []byte(`{"instruction":"granting second attempt"}`),
	}
	scopeA := contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RolePlanner, SessionID: &sessionA}
	scopeB := contract.ExecutionScope{RunID: runID, EpochNumber: 1, Role: contract.RolePlanner, SessionID: &sessionB}
	if defaultToolKey(scopeA, cmd) != defaultToolKey(scopeA, cmd) {
		t.Fatal("same session and payload must keep one idempotency key")
	}
	if defaultToolKey(scopeA, cmd) == defaultToolKey(scopeB, cmd) {
		t.Fatal("a later planner session must not replay the previous continue_work result")
	}
}
