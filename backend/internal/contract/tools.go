package contract

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

const (
	// LegacyImplicitToolTimeout is the historical default. Command jobs must not
	// treat it as their only timeout.
	LegacyImplicitToolTimeout = 5 * time.Second
	DefaultHTTPToolTimeout    = 30 * time.Second
	MaxHTTPToolTimeout        = 120 * time.Second
	DefaultCommandTimeout     = 60 * time.Second
	MaxCommandTimeout         = 300 * time.Second
	CommandCleanupWindow      = 15 * time.Second
)

// ToolProfile selects a role toolset. Planner initial and follow-up profiles
// must expose the same control tools.
type ToolProfile string

const (
	ProfilePlannerInitial  ToolProfile = "planner_initial"
	ProfilePlannerFollowup ToolProfile = "planner_followup"
	ProfileWorker          ToolProfile = "worker"
	ProfileMainAgent       ToolProfile = "mainagent"
	ProfileVerifier        ToolProfile = "verifier"
)

func (p ToolProfile) Known() bool {
	switch p {
	case ProfilePlannerInitial, ProfilePlannerFollowup, ProfileWorker, ProfileMainAgent, ProfileVerifier:
		return true
	default:
		return false
	}
}

// PlannerControlTools must be present and identical on both planner profiles.
var PlannerControlTools = []string{
	"add_intent",
	"steer_work",
	"kill_work",
	"continue_work",
	"retry_work",
}

// PlannerReadTools are the supervision reads both planner profiles must expose.
var PlannerReadTools = []string{
	"graph_overview",
	"get_worker_trace",
	"list_worker_traces",
}

// WorkerExecutionTools are the real action tools a worker profile must expose
// once the HTTP and job modules are wired. The contract does not register them.
var WorkerExecutionTools = []string{
	"http_request",
	"run_command",
	"write_file",
	"read_file",
	"inspect_command",
}

// AssembledToolset is a name list, not an executable registry.
type AssembledToolset struct {
	Role    Role        `json:"role"`
	Profile ToolProfile `json:"profile"`
	Names   []string    `json:"names"`
}

// ToolsetFactory assembles the tools a role may see. Implementations live in
// the tool modules and are injected by the composition root.
type ToolsetFactory interface {
	Assemble(ctx context.Context, scope ExecutionScope, profile ToolProfile) (*AssembledToolset, error)
}

// RequirePlannerSupervision fails when either planner profile is missing a
// frozen supervision tool or their control-tool sets differ.
func RequirePlannerSupervision(initial, followup []string) error {
	if err := requireNames("planner_initial", initial, PlannerControlTools); err != nil {
		return err
	}
	if err := requireNames("planner_initial", initial, PlannerReadTools); err != nil {
		return err
	}
	if err := requireNames("planner_followup", followup, PlannerControlTools); err != nil {
		return err
	}
	if err := requireNames("planner_followup", followup, PlannerReadTools); err != nil {
		return err
	}
	if !sameSubset(initial, followup, PlannerControlTools) {
		return invalidInput("planner initial and follow-up control tools differ")
	}
	return nil
}

func requireNames(profile string, have, need []string) error {
	set := map[string]struct{}{}
	for _, name := range have {
		set[name] = struct{}{}
	}
	for _, name := range need {
		if _, ok := set[name]; !ok {
			return invalidInput(profile + " missing tool " + name)
		}
	}
	return nil
}

func sameSubset(left, right, names []string) bool {
	ls := map[string]struct{}{}
	rs := map[string]struct{}{}
	for _, name := range left {
		ls[name] = struct{}{}
	}
	for _, name := range right {
		rs[name] = struct{}{}
	}
	for _, name := range names {
		_, lok := ls[name]
		_, rok := rs[name]
		if lok != rok {
			return false
		}
	}
	return true
}

type ToolAdmissionRequest struct {
	ToolName      string
	ArgumentsHash string
	Timeout       time.Duration
}

type ToolAdmission struct {
	ID       uuid.UUID
	Deadline time.Time
}

// ToolOutcome.Status is success, failed, cancelled, interrupted, or outcome_unknown.
// success means the tool mechanism returned. It does not mean a finding is real.
type ToolOutcome struct {
	Status string
	Err    error
}

// ToolExecutionPolicy admits a tool call against scope, epoch, pause, and budget.
// It does not execute the tool. Release is idempotent.
type ToolExecutionPolicy interface {
	Admit(ctx context.Context, scope ExecutionScope, call ToolAdmissionRequest) (*ToolAdmission, error)
	Release(ctx context.Context, scope ExecutionScope, admissionID uuid.UUID, outcome ToolOutcome) error
}

// ToolBinding is the descriptive shape a factory contributor registers.
type ToolBinding struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}
