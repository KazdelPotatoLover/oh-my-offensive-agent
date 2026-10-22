package contract

import (
	"context"

	"github.com/google/uuid"
)

// RevokeKind names a cancellable resource. Model-wait cancellation is a queue
// wake, not a failed or successful model call.
type RevokeKind string

const (
	RevokeModelWait    RevokeKind = "model_wait"
	RevokeHTTPExchange RevokeKind = "http_exchange"
	RevokeJob          RevokeKind = "job"
	RevokeAttempt      RevokeKind = "attempt"
)

// RevokeRequest asks one port to stop work for a scope. A single-direction kill
// sets the worker scope. Stop and epoch rollover revoke every old attempt.
type RevokeRequest struct {
	Scope  ExecutionScope
	Kind   RevokeKind
	JobID  *uuid.UUID
	Reason string
}

func (r RevokeRequest) Validate() error {
	if err := r.Scope.Validate(); err != nil {
		return err
	}
	switch r.Kind {
	case RevokeModelWait, RevokeHTTPExchange, RevokeJob, RevokeAttempt:
	default:
		return invalidInput("unknown revoke kind")
	}
	if r.Kind == RevokeJob && (r.JobID == nil || *r.JobID == uuid.Nil) {
		return invalidInput("job revoke requires job_id")
	}
	return nil
}

// RevokeReport counts what the port actually stopped. OutcomeUnknown is not
// success and not a forged exit code.
type RevokeReport struct {
	Kind           RevokeKind `json:"kind"`
	Cancelled      int        `json:"cancelled"`
	OutcomeUnknown int        `json:"outcome_unknown"`
}

// ResourceRevoker is the cancellation port shared by the controller and the
// worker manager. HTTP, job, and model-wait implementations are injected.
// There is no second manager.
type ResourceRevoker interface {
	Revoke(ctx context.Context, req RevokeRequest) (*RevokeReport, error)
}
