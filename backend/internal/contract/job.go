package contract

import (
	"context"

	"github.com/google/uuid"
)

// JobState is the execution-container lifecycle. Timeout and cancel do not
// become finished with exit code 0.
type JobState string

const (
	JobQueued         JobState = "queued"
	JobRunning        JobState = "running"
	JobFinished       JobState = "finished"
	JobKilled         JobState = "killed"
	JobInterrupted    JobState = "interrupted"
	JobOutcomeUnknown JobState = "outcome_unknown"
)

func (s JobState) Known() bool {
	switch s {
	case JobQueued, JobRunning, JobFinished, JobKilled, JobInterrupted, JobOutcomeUnknown:
		return true
	default:
		return false
	}
}

// JobSpec is the server-side job request. Image must be on the server allowlist.
// The model does not supply Docker socket, host path, privileged, or network mode.
type JobSpec struct {
	Image          string
	Argv           []string
	TimeoutSeconds int
	WorkItemID     uuid.UUID
}

func (s JobSpec) Validate() error {
	if s.WorkItemID == uuid.Nil {
		return invalidInput("job requires work_item_id")
	}
	if len(s.Argv) == 0 || s.Argv[0] == "" {
		return invalidInput("job requires argv")
	}
	if s.TimeoutSeconds <= 0 {
		return invalidInput("job requires timeout_seconds")
	}
	return nil
}

// JobView is the public job result. ExitCode is set only for a finished job.
type JobView struct {
	JobID              uuid.UUID   `json:"job_id"`
	State              JobState    `json:"state"`
	ExitCode           *int        `json:"exit_code,omitempty"`
	StdoutArtifactID   *uuid.UUID  `json:"stdout_artifact_id,omitempty"`
	StderrArtifactID   *uuid.UUID  `json:"stderr_artifact_id,omitempty"`
	TrafficRefs        []uuid.UUID `json:"traffic_refs,omitempty"`
	ExecutionReceiptID *uuid.UUID  `json:"execution_receipt_id,omitempty"`
}

func (v JobView) Validate() error {
	if v.JobID == uuid.Nil {
		return invalidInput("job_id is required")
	}
	if !v.State.Known() {
		return invalidInput("unknown job state")
	}
	if v.State == JobFinished {
		if v.ExitCode == nil {
			return invalidInput("finished job requires exit_code")
		}
		return nil
	}
	if v.ExitCode != nil {
		return invalidInput("exit_code is set only when the job finished")
	}
	return nil
}

// JobExecutor runs, cancels, and inspects an isolated job. Implementations
// receive CaptureGateway by constructor injection. They must not import the
// HTTP module's concrete types or mount a Docker socket in the API process.
type JobExecutor interface {
	Run(ctx context.Context, scope ExecutionScope, spec JobSpec) (*JobView, error)
	Cancel(ctx context.Context, scope ExecutionScope, jobID uuid.UUID) (*JobView, error)
	Inspect(ctx context.Context, scope ExecutionScope, jobID uuid.UUID) (*JobView, error)
}
