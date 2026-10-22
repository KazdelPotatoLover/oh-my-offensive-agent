package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"agent-workbench/backend/internal/provider"
)

var (
	ErrToolNotFound       = errors.New("tool not found")
	ErrInvalidArguments   = errors.New("invalid arguments")
	ErrExecutionCancelled = errors.New("tool execution cancelled")
)

type Tool interface {
	Name() string
	Description() string
	Parameters() json.RawMessage
	Validate(argumentsJSON []byte) error
	Execute(ctx context.Context, argumentsJSON []byte) (string, error)
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{
		tools: make(map[string]Tool),
	}
}

func (r *Registry) Register(tool Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[tool.Name()] = tool
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	tool, ok := r.tools[name]
	return tool, ok
}

func (r *Registry) Definitions() []provider.ToolDefinition {
	r.mu.RLock()
	defer r.mu.RUnlock()

	defs := make([]provider.ToolDefinition, 0, len(r.tools))
	for _, tool := range r.tools {
		defs = append(defs, provider.ToolDefinition{
			Name:        tool.Name(),
			Description: tool.Description(),
			Parameters:  tool.Parameters(),
		})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

// Validate validates that the tool exists and its arguments conform to expectations.
func (r *Registry) Validate(name string, argumentsJSON []byte) (Tool, error) {
	tool, ok := r.Get(name)
	if !ok {
		return nil, fmt.Errorf("%w: unknown tool '%s'", ErrToolNotFound, name)
	}

	if err := tool.Validate(argumentsJSON); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidArguments, err)
	}

	return tool, nil
}
