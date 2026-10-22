package execution

import (
	"bytes"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// RunRequest is the fixed broker protocol. HostConfig, mounts, and image
// arguments from a model payload are not fields of this struct.
type RunRequest struct {
	Protocol       string    `json:"protocol"`
	JobID          uuid.UUID `json:"job_id"`
	RunID          uuid.UUID `json:"run_id"`
	AttemptID      uuid.UUID `json:"attempt_id"`
	Image          string    `json:"image"`
	Argv           []string  `json:"argv"`
	TimeoutSeconds int       `json:"timeout_seconds"`
	WorkspaceHost  string    `json:"workspace_host"`
	CaptureURL     string    `json:"capture_url,omitempty"`
	CaptureToken   string    `json:"capture_token,omitempty"`
	AllowedOrigins []string  `json:"allowed_origins,omitempty"`
	Offline        bool      `json:"offline"`
}

// RunResult is the supervision result. ExitCode is set only for a natural finish.
type RunResult struct {
	JobID       uuid.UUID       `json:"job_id"`
	State       string          `json:"state"`
	ExitCode    *int            `json:"exit_code,omitempty"`
	Stdout      string          `json:"stdout,omitempty"`
	Stderr      string          `json:"stderr,omitempty"`
	Truncated   bool            `json:"truncated"`
	ContainerID string          `json:"container_id,omitempty"`
	Security    *SecurityReport `json:"security,omitempty"`
	ErrorCode   string          `json:"error_code,omitempty"`
}

type captureExchange struct {
	Protocol string            `json:"protocol"`
	JobID    uuid.UUID         `json:"job_id"`
	Method   string            `json:"method"`
	URL      string            `json:"url"`
	Headers  map[string]string `json:"headers,omitempty"`
	BodyB64  string            `json:"body_b64,omitempty"`
}

type captureExchangeResult struct {
	StatusCode  int               `json:"status_code"`
	Headers     map[string]string `json:"headers,omitempty"`
	BodyB64     string            `json:"body_b64,omitempty"`
	ReceiptID   uuid.UUID         `json:"receipt_id"`
	Truncated   bool              `json:"truncated"`
	CaptureMode string            `json:"capture_mode"`
}

type reconcileRequest struct {
	Protocol       string          `json:"protocol"`
	KillContainers []reconcileKill `json:"kill_containers,omitempty"`
}

type reconcileKill struct {
	ID    string    `json:"id"`
	JobID uuid.UUID `json:"job_id"`
}

func decodeStrict(raw []byte, dest any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dest); err != nil {
		return invalidInput("execution protocol body is not valid")
	}
	if dec.More() {
		return invalidInput("execution protocol body is not valid")
	}
	return nil
}

func (r RunRequest) validate(allow []string) error {
	if r.Protocol != ProtocolVersion {
		return invalidInput("execution protocol version is not supported")
	}
	if r.JobID == uuid.Nil || r.RunID == uuid.Nil || r.AttemptID == uuid.Nil {
		return invalidInput("execution job requires job, run, and attempt ids")
	}
	if !imageAllowed(r.Image, allow) {
		return scopeDenied("execution image is not allowlisted")
	}
	if _, err := NormalizeArgv(r.Argv); err != nil {
		return err
	}
	if r.TimeoutSeconds <= 0 || r.TimeoutSeconds > 300 {
		return invalidInput("execution timeout is outside 1..300 seconds")
	}
	if r.Offline && len(r.AllowedOrigins) > 0 {
		return invalidInput("offline execution cannot declare network origins")
	}
	if !r.Offline && r.CaptureURL == "" {
		return unavailable("networked execution requires a capture callback")
	}
	return nil
}

func protocolErrorStatus(err error) int {
	var ce interface{ Status() int }
	if errors.As(err, &ce) {
		return ce.Status()
	}
	return 500
}
