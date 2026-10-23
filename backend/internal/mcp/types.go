package mcp

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	ProtocolVersionStdio = "2024-11-05"
	ProtocolVersionHTTP  = "2025-06-18"
)

// ToolName formats a remote MCP tool name into the standardized three-segment
// name mcp__<server>__<tool> used throughout the workbench and agent framework.
func ToolName(server, name string) string {
	return "mcp__" + server + "__" + name
}

// RPCRequest is a JSON-RPC 2.0 request frame.
type RPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// RPCResponse is a JSON-RPC 2.0 response frame.
type RPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      int             `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *RPCError       `json:"error,omitempty"`
}

// RPCError is a standard JSON-RPC 2.0 error object.
type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	return fmt.Sprintf("mcp rpc error %d: %s", e.Code, e.Message)
}

// ClientInfo identifies the client in the MCP initialize handshake.
type ClientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

// InitializeParams is sent in the initialize request.
type InitializeParams struct {
	ProtocolVersion string         `json:"protocolVersion"`
	Capabilities    map[string]any `json:"capabilities"`
	ClientInfo      ClientInfo     `json:"clientInfo"`
}

// RemoteTool represents a single tool advertised by an MCP server via tools/list.
type RemoteTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"inputSchema,omitempty"`
}

// ToolListResult is the response payload for tools/list.
type ToolListResult struct {
	Tools []RemoteTool `json:"tools"`
}

// ContentItem represents a text or resource item in CallToolResult.
type ContentItem struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MimeType string `json:"mimeType,omitempty"`
}

// CallToolResult is the response payload for tools/call.
type CallToolResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError,omitempty"`
}

// MCPServerModel represents a configured MCP server row in PostgreSQL.
type MCPServerModel struct {
	ID        int64           `json:"id"`
	Name      string          `json:"name"`
	Transport string          `json:"transport"` // "stdio", "http", "sse"
	Command   string          `json:"command,omitempty"`
	Args      []string        `json:"args"`
	Env       map[string]string `json:"env"`
	URL       string          `json:"url,omitempty"`
	Enabled   bool            `json:"enabled"`
	Insecure  bool            `json:"insecure"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	Tools     []MCPToolModel  `json:"tools,omitempty"`
}

// MCPToolModel represents a cached tool for an MCP server in PostgreSQL.
type MCPToolModel struct {
	ID           int64           `json:"id"`
	ServerID     int64           `json:"server_id"`
	ToolName     string          `json:"tool_name"`
	Description  string          `json:"description"`
	Schema       json.RawMessage `json:"schema"`
	DiscoveredAt time.Time       `json:"discovered_at"`
}
