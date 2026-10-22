package tools

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

type panicFixture struct{ phase string }

func (p panicFixture) Name() string                { return "panic_fixture" }
func (p panicFixture) Description() string         { return "fixture" }
func (p panicFixture) Parameters() json.RawMessage { return json.RawMessage(`{}`) }
func (p panicFixture) Validate([]byte) error {
	if p.phase == "validate" {
		panic("secret-panic-payload")
	}
	return nil
}
func (p panicFixture) Execute(ctx context.Context, _ []byte) (string, error) {
	defer func() { _ = ctx.Err() }()
	panic("secret-panic-payload")
}

func TestRegistryGuardsValidationAndExecution(t *testing.T) {
	for _, phase := range []string{"validate", "execute"} {
		t.Run(phase, func(t *testing.T) {
			r := NewRegistry()
			r.Register(panicFixture{phase})
			tool, err := r.Validate("panic_fixture", []byte(`{}`))
			if phase == "execute" {
				if err != nil {
					t.Fatal(err)
				}
				_, err = tool.Execute(context.Background(), []byte(`{}`))
			}
			if !errors.Is(err, ErrToolPanic) {
				t.Fatalf("got %v", err)
			}
		})
	}
}
