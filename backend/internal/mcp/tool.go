package mcp

import (
	"context"
	"encoding/json"
	"fmt"
)

// ToolAdapter adapts an MCP remote tool definition and active Client connection
// into the Workbench tools.Tool interface so it can be registered with tools.Registry
// and invoked seamlessly by Planner or Worker sessions.
type ToolAdapter struct {
	serverName string
	remoteName string
	desc       string
	parameters json.RawMessage
	client     Client
}

// NewToolAdapter creates a ToolAdapter for a discovered MCP tool.
func NewToolAdapter(serverName string, tool RemoteTool, client Client) *ToolAdapter {
	params := tool.InputSchema
	if len(params) == 0 || string(params) == "null" {
		params = json.RawMessage(`{"type":"object","properties":{}}`)
	}

	return &ToolAdapter{
		serverName: serverName,
		remoteName: tool.Name,
		desc:       tool.Description,
		parameters: params,
		client:     client,
	}
}

func (t *ToolAdapter) Name() string {
	return ToolName(t.serverName, t.remoteName)
}

func (t *ToolAdapter) Description() string {
	if t.desc != "" {
		return fmt.Sprintf("[MCP %s] %s", t.serverName, t.desc)
	}
	return fmt.Sprintf("[MCP %s] Remote tool %s", t.serverName, t.remoteName)
}

func (t *ToolAdapter) Parameters() json.RawMessage {
	return t.parameters
}

func (t *ToolAdapter) Validate(args []byte) error {
	if len(args) == 0 {
		return nil
	}
	var js any
	if err := json.Unmarshal(args, &js); err != nil {
		return fmt.Errorf("invalid arguments json: %w", err)
	}
	return nil
}

func (t *ToolAdapter) Execute(ctx context.Context, args []byte) (string, error) {
	return t.client.Call(ctx, t.remoteName, args)
}
