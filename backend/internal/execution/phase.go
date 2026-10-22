package execution

import (
	"context"

	"github.com/google/uuid"
)

type phaseKey struct{}
type sourceToolCallKey struct{}
type epochKey struct{}

// WithPhase marks explore or settlement. Settlement cannot start commands.
func WithPhase(ctx context.Context, phase string) context.Context {
	return context.WithValue(ctx, phaseKey{}, phase)
}

// PhaseFrom returns the server-set phase. An empty phase is explore.
func PhaseFrom(ctx context.Context) string {
	phase, _ := ctx.Value(phaseKey{}).(string)
	if phase == "" {
		return PhaseExplore
	}
	return phase
}

// WithSourceToolCall attaches the tool-call row the session runner already inserted.
func WithSourceToolCall(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, sourceToolCallKey{}, id)
}

func sourceToolCallFrom(ctx context.Context) *uuid.UUID {
	id, ok := ctx.Value(sourceToolCallKey{}).(uuid.UUID)
	if !ok || id == uuid.Nil {
		return nil
	}
	return &id
}

// WithEpoch overrides the epoch tools read from the run row. Tests use it.
func WithEpoch(ctx context.Context, epoch int) context.Context {
	return context.WithValue(ctx, epochKey{}, epoch)
}

func epochFrom(ctx context.Context) (int, bool) {
	epoch, ok := ctx.Value(epochKey{}).(int)
	return epoch, ok && epoch >= 1
}
