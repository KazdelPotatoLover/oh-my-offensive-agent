package contract

import "strings"

// Reserved migration numbers. Create a file only when the schema change is
// real. Do not add an empty file to occupy the number. Extra numbers are
// assigned by the integration owner. Migrations 001-025 stay unchanged.
const (
	MigrationRuntimeConstraints = 26
	MigrationModelDiagnostics   = 27
	MigrationHTTPCapture        = 28
	MigrationExecutionJobs      = 29
	MigrationMainControl        = 30
)

// ExecutionPorts is the production composition root. cmd/server is the only
// caller that fills it for a running server. A nil or Unimplemented port is
// not a production fallback.
type ExecutionPorts struct {
	Runtime     RuntimeResolver
	ModelPolicy ModelCallPolicy
	Controller  ExecutionController
	Tools       ToolsetFactory
	ToolPolicy  ToolExecutionPolicy
	Transport   TargetTransport
	Capture     CaptureGateway
	Jobs        JobExecutor
	Revoker     ResourceRevoker
	Workbench   WorkbenchQuery
}

// ProductionReady reports every missing or fail-closed port. It does not
// substitute a mock and does not start a run.
func (p ExecutionPorts) ProductionReady() error {
	missing := make([]string, 0, 10)
	check := func(name string, value any) {
		if value == nil || isUnimplemented(value) {
			missing = append(missing, name)
		}
	}
	check("runtime", p.Runtime)
	check("model_policy", p.ModelPolicy)
	check("controller", p.Controller)
	check("tools", p.Tools)
	check("tool_policy", p.ToolPolicy)
	check("transport", p.Transport)
	check("capture", p.Capture)
	check("jobs", p.Jobs)
	check("revoker", p.Revoker)
	check("workbench", p.Workbench)
	if len(missing) > 0 {
		return &Error{
			Code:            CodeNotImplemented,
			Scope:           ScopeRequest,
			Message:         "production ports missing: " + strings.Join(missing, ", "),
			SuggestedAction: ActionNone,
		}
	}
	return nil
}
