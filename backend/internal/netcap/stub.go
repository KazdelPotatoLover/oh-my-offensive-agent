package netcap

import (
	"context"

	"agent-workbench/backend/internal/contract"

	"github.com/google/uuid"
)

// MemGateway is the explicit local stand-in. It implements the frozen ports
// and refuses to mint an empirical receipt. Production must not inject it.
type MemGateway struct{}

func (m *MemGateway) Do(context.Context, contract.ExecutionScope, contract.HTTPRequestInput) (*contract.HTTPReceipt, error) {
	return nil, contract.ErrNotImplemented
}

func (m *MemGateway) Exchange(context.Context, contract.ExecutionScope, contract.HTTPRequestInput) (*contract.HTTPReceipt, error) {
	return nil, contract.ErrNotImplemented
}

func (m *MemGateway) Lookup(context.Context, contract.ExecutionScope, uuid.UUID) (*contract.HTTPReceipt, error) {
	return nil, contract.ErrNotImplemented
}

// ProductionForbidden marks a stand-in. A real Transport does not implement this.
func (m *MemGateway) ProductionForbidden() error { return contract.ErrNotImplemented }

// RejectNonProduction fails closed for nil ports and explicit stand-ins.
// contract.ProductionReady does not see a stub that merely implements the interface.
func RejectNonProduction(port any) error {
	if port == nil {
		return contract.ErrNotImplemented
	}
	type forbidden interface {
		ProductionForbidden() error
	}
	if f, ok := port.(forbidden); ok {
		return f.ProductionForbidden()
	}
	return nil
}
