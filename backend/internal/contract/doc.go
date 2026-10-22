// Package contract is the frozen multi-agent boundary for this round.
//
// It defines identities, errors, commands, and ports. It does not execute a
// run, call a model, or mint evidence. Production code must inject real
// implementations. The Unimplemented* types return ErrNotImplemented and must
// not be wired as a silent fallback.
//
// Dependency direction:
//
//	contract imports only the standard library and github.com/google/uuid.
//	provider, service, orchestration, tools, handler, and the frontend adapt
//	to these types. They do not import one another's handlers.
//	cmd/server is the only production composition root.
//
// ToolExecutionPolicy, RuntimeResolver, and ModelCallPolicy are consumed by
// the session runner. ExecutionController is the only control writer. Capture
// receipts and jobs are produced by their gateways, then referenced by ID.
// Workbench DTOs are produced by WorkbenchQuery and consumed by the UI.
package contract
