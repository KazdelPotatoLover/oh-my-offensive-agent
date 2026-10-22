package contract

import (
	"context"
	"errors"
	"strings"

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

// FanoutRevoker delivers one request to each injected port. A port that does
// not handle the kind is skipped. It does not turn a missing port into success.
type FanoutRevoker struct {
	Ports []ResourceRevoker
}

func (f FanoutRevoker) Revoke(ctx context.Context, req RevokeRequest) (*RevokeReport, error) {
	report := &RevokeReport{Kind: req.Kind}
	var hard error
	for _, port := range f.Ports {
		if port == nil {
			continue
		}
		got, err := port.Revoke(ctx, req)
		if err != nil {
			var coded *Error
			if errors.As(err, &coded) && coded.Code == CodeInvalidInput && strings.Contains(coded.Message, "does not handle") {
				continue
			}
			if hard == nil {
				hard = err
			}
		}
		if got != nil {
			report.Cancelled += got.Cancelled
			report.OutcomeUnknown += got.OutcomeUnknown
		}
	}
	return report, hard
}
