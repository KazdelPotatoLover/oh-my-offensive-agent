package tools

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
)

// ErrToolPanic deliberately contains no panic payload, arguments or stack.
// A recovered panic is a failed call whose external outcome may be unknown.
var ErrToolPanic = errors.New("tool internal error; call failed, inspect saved results before retrying")

type guardedTool struct{ Tool }

func (t guardedTool) recoverCall(phase string, err *error) {
	if value := recover(); value != nil {
		slog.Error("tool panic", "tool", t.Name(), "phase", phase, "panic", value, "stack", string(debug.Stack()))
		*err = ErrToolPanic
	}
}

func (t guardedTool) Validate(args []byte) (err error) {
	defer t.recoverCall("validate", &err)
	return t.Tool.Validate(args)
}

func (t guardedTool) Execute(ctx context.Context, args []byte) (out string, err error) {
	defer t.recoverCall("execute", &err)
	return t.Tool.Execute(ctx, args)
}
