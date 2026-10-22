package contract

import (
	"context"

	"github.com/google/uuid"
)

func isUnimplemented(value any) bool {
	switch value.(type) {
	case UnimplementedRuntime, *UnimplementedRuntime,
		UnimplementedModelPolicy, *UnimplementedModelPolicy,
		UnimplementedModelCaller, *UnimplementedModelCaller,
		UnimplementedController, *UnimplementedController,
		UnimplementedToolset, *UnimplementedToolset,
		UnimplementedToolPolicy, *UnimplementedToolPolicy,
		UnimplementedTransport, *UnimplementedTransport,
		UnimplementedCapture, *UnimplementedCapture,
		UnimplementedJobs, *UnimplementedJobs,
		UnimplementedRevoker, *UnimplementedRevoker,
		UnimplementedWorkbench, *UnimplementedWorkbench:
		return true
	default:
		return false
	}
}

type UnimplementedRuntime struct{}

func (UnimplementedRuntime) Resolve(context.Context, ExecutionScope, string) (*ResolvedRuntime, error) {
	return nil, ErrNotImplemented
}

type UnimplementedModelPolicy struct{}

func (UnimplementedModelPolicy) Acquire(context.Context, ExecutionScope) (*ModelCallLease, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedModelPolicy) Complete(context.Context, ModelCallLease, ModelCallRecord) error {
	return ErrNotImplemented
}

type UnimplementedModelCaller struct{}

func (UnimplementedModelCaller) Call(context.Context, ModelCallRequest) (*ModelCallResult, error) {
	return nil, ErrNotImplemented
}

type UnimplementedController struct{}

func (UnimplementedController) Apply(context.Context, Principal, ExecutionScope, string, Command) (*CommandResult, error) {
	return nil, ErrNotImplemented
}

type UnimplementedToolset struct{}

func (UnimplementedToolset) Assemble(context.Context, ExecutionScope, ToolProfile) (*AssembledToolset, error) {
	return nil, ErrNotImplemented
}

type UnimplementedToolPolicy struct{}

func (UnimplementedToolPolicy) Admit(context.Context, ExecutionScope, ToolAdmissionRequest) (*ToolAdmission, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedToolPolicy) Release(context.Context, ExecutionScope, uuid.UUID, ToolOutcome) error {
	return ErrNotImplemented
}

type UnimplementedTransport struct{}

func (UnimplementedTransport) Do(context.Context, ExecutionScope, HTTPRequestInput) (*HTTPReceipt, error) {
	return nil, ErrNotImplemented
}

type UnimplementedCapture struct{}

func (UnimplementedCapture) Exchange(context.Context, ExecutionScope, HTTPRequestInput) (*HTTPReceipt, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedCapture) Lookup(context.Context, ExecutionScope, uuid.UUID) (*HTTPReceipt, error) {
	return nil, ErrNotImplemented
}

type UnimplementedJobs struct{}

func (UnimplementedJobs) Run(context.Context, ExecutionScope, JobSpec) (*JobView, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedJobs) Cancel(context.Context, ExecutionScope, uuid.UUID) (*JobView, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedJobs) Inspect(context.Context, ExecutionScope, uuid.UUID) (*JobView, error) {
	return nil, ErrNotImplemented
}

type UnimplementedRevoker struct{}

func (UnimplementedRevoker) Revoke(context.Context, RevokeRequest) (*RevokeReport, error) {
	return nil, ErrNotImplemented
}

type UnimplementedWorkbench struct{}

func (UnimplementedWorkbench) Snapshot(context.Context, uuid.UUID, WorkbenchQueryParams) (*WorkbenchSnapshot, error) {
	return nil, ErrNotImplemented
}

func (UnimplementedWorkbench) MainMessages(context.Context, uuid.UUID, int64, int) (*MainMessagePage, error) {
	return nil, ErrNotImplemented
}

var (
	_ RuntimeResolver     = UnimplementedRuntime{}
	_ ModelCallPolicy     = UnimplementedModelPolicy{}
	_ ModelCaller         = UnimplementedModelCaller{}
	_ ExecutionController = UnimplementedController{}
	_ ToolsetFactory      = UnimplementedToolset{}
	_ ToolExecutionPolicy = UnimplementedToolPolicy{}
	_ TargetTransport     = UnimplementedTransport{}
	_ CaptureGateway      = UnimplementedCapture{}
	_ JobExecutor         = UnimplementedJobs{}
	_ ResourceRevoker     = UnimplementedRevoker{}
	_ WorkbenchQuery      = UnimplementedWorkbench{}
)
