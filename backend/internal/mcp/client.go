package mcp

import (
	"context"
	"encoding/json"
	"io"
)

// Client is a unified interface for communicating with an MCP server regardless
// of underlying transport (stdio subprocess, Streamable HTTP, or legacy SSE).
type Client interface {
	io.Closer
	ServerName() string
	Tools(ctx context.Context) ([]RemoteTool, error)
	Call(ctx context.Context, toolName string, args json.RawMessage) (string, error)
}
