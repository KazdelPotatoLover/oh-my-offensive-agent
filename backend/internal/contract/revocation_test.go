package contract

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

type skipRevoker struct{}

func (skipRevoker) Revoke(context.Context, RevokeRequest) (*RevokeReport, error) {
	return nil, invalidInput("execution revoker does not handle model_wait")
}

type countRevoker struct{ n int }

func (c *countRevoker) Revoke(context.Context, RevokeRequest) (*RevokeReport, error) {
	c.n++
	return &RevokeReport{Cancelled: 2}, nil
}

func TestFanoutRevokerSkipsUnhandledKind(t *testing.T) {
	counter := &countRevoker{}
	fan := FanoutRevoker{Ports: []ResourceRevoker{nil, skipRevoker{}, counter}}
	report, err := fan.Revoke(context.Background(), RevokeRequest{
		Scope: ExecutionScope{RunID: uuid.New(), EpochNumber: 1, Role: RolePlanner},
		Kind:  RevokeModelWait,
	})
	if err != nil {
		t.Fatal(err)
	}
	if counter.n != 1 || report.Cancelled != 2 {
		t.Fatalf("counter %d report %+v", counter.n, report)
	}
}
