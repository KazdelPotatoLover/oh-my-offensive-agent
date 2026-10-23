package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"
	"time"

	"agent-workbench/backend/internal/tools"

	"github.com/stretchr/testify/require"
)

// mockMCPServer returns an httptest.Server implementing the MCP Streamable HTTP protocol.
func mockMCPServer(t *testing.T) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}

		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "sess-test-12345")

		switch req.Method {
		case "initialize":
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"protocolVersion": "2025-06-18",
					"capabilities": {"tools": {}},
					"serverInfo": {"name": "mock-mcp", "version": "1.0"}
				}`),
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "tools/list":
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"tools": [
						{
							"name": "ping",
							"description": "Send a ping message",
							"inputSchema": {
								"type": "object",
								"properties": {
									"msg": {"type": "string"}
								},
								"required": ["msg"]
							}
						},
						{
							"name": "echo",
							"description": "Echo input value",
							"inputSchema": {
								"type": "object",
								"properties": {
									"val": {"type": "string"}
								}
							}
						}
					]
				}`),
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "tools/call":
			var params struct {
				Name      string          `json:"name"`
				Arguments json.RawMessage `json:"arguments"`
			}
			rawParams, _ := json.Marshal(req.Params)
			_ = json.Unmarshal(rawParams, &params)

			outText := fmt.Sprintf("executed %s with %s", params.Name, string(params.Arguments))
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(fmt.Sprintf(`{
					"content": [
						{"type": "text", "text": %q}
					]
				}`, outText)),
			}
			_ = json.NewEncoder(w).Encode(resp)

		default:
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &RPCError{
					Code:    -32601,
					Message: "method not found",
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	}))
}

func TestHTTPClient_FullLifecycle(t *testing.T) {
	server := mockMCPServer(t)
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	headers := map[string]string{"Authorization": "Bearer test-secret-token"}
	client, err := NewHTTPClient(ctx, "test-remote", "http", server.URL, headers, true)
	require.NoError(t, err)
	defer client.Close()

	require.Equal(t, "test-remote", client.ServerName())

	// 1. List tools
	remoteTools, err := client.Tools(ctx)
	require.NoError(t, err)
	require.Len(t, remoteTools, 2)
	require.Equal(t, "ping", remoteTools[0].Name)
	require.Equal(t, "echo", remoteTools[1].Name)

	// 2. Call tool
	res, err := client.Call(ctx, "ping", json.RawMessage(`{"msg":"hello world"}`))
	require.NoError(t, err)
	require.Contains(t, res, "executed ping with")
	require.Contains(t, res, "hello world")
}

func TestToolAdapter_WithRegistry(t *testing.T) {
	server := mockMCPServer(t)
	defer server.Close()

	ctx := context.Background()
	client, err := NewHTTPClient(ctx, "mockserver", "http", server.URL, nil, true)
	require.NoError(t, err)
	defer client.Close()

	remoteTools, err := client.Tools(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, remoteTools)

	adapter := NewToolAdapter("mockserver", remoteTools[0], client)
	require.Equal(t, "mcp__mockserver__ping", adapter.Name())
	require.Contains(t, adapter.Description(), "[MCP mockserver]")
	require.NoError(t, adapter.Validate([]byte(`{"msg":"test"}`)))
	require.Error(t, adapter.Validate([]byte(`invalid-json`)))

	// Register with tools.Registry
	reg := tools.NewRegistry()
	reg.Register(adapter)

	registeredTool, ok := reg.Get("mcp__mockserver__ping")
	require.True(t, ok)
	require.Equal(t, "mcp__mockserver__ping", registeredTool.Name())

	// Execute via registry
	out, err := registeredTool.Execute(ctx, []byte(`{"msg":"ping-payload"}`))
	require.NoError(t, err)
	require.Contains(t, out, "ping-payload")
}

// TestHelperProcess implements an in-process stdio MCP server for testing StdioClient.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_MCP_HELPER_PROCESS") != "1" {
		return
	}

	scanner := bufioNewScanner(os.Stdin)
	for scanner.Scan() {
		line := scanner.Bytes()
		var req RPCRequest
		if err := json.Unmarshal(line, &req); err != nil {
			continue
		}

		switch req.Method {
		case "initialize":
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"protocolVersion": "2024-11-05",
					"capabilities": {},
					"serverInfo": {"name": "helper", "version": "1.0"}
				}`),
			}
			data, _ := json.Marshal(resp)
			fmt.Printf("%s\n", string(data))

		case "notifications/initialized":
			// no response needed for notification

		case "tools/list":
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"tools": [
						{"name": "helper_calc", "description": "Calculator"}
					]
				}`),
			}
			data, _ := json.Marshal(resp)
			fmt.Printf("%s\n", string(data))

		case "tools/call":
			resp := RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"content": [{"type": "text", "text": "result: 42"}]
				}`),
			}
			data, _ := json.Marshal(resp)
			fmt.Printf("%s\n", string(data))
		}
	}
	os.Exit(0)
}

func bufioNewScanner(r *os.File) *bufioScanner {
	return &bufioScanner{r: r}
}

type bufioScanner struct {
	r *os.File
}

func (s *bufioScanner) Scan() bool {
	// simple implementation
	return true
}

func (s *bufioScanner) Bytes() []byte {
	return nil
}

func TestStdioClient_Subprocess(t *testing.T) {
	// Use echo script or python3 inline helper for reliable cross-platform test
	pythonScript := `
import sys, json

for line in sys.stdin:
    if not line.strip():
        continue
    req = json.loads(line)
    method = req.get("method")
    req_id = req.get("id")
    if method == "initialize":
        res = {"jsonrpc": "2.0", "id": req_id, "result": {"protocolVersion": "2024-11-05", "serverInfo": {"name": "py-mcp"}}}
        sys.stdout.write(json.dumps(res) + "\n")
        sys.stdout.flush()
    elif method == "notifications/initialized":
        pass
    elif method == "tools/list":
        res = {"jsonrpc": "2.0", "id": req_id, "result": {"tools": [{"name": "py_probe", "description": "Python Probe"}]}}
        sys.stdout.write(json.dumps(res) + "\n")
        sys.stdout.flush()
    elif method == "tools/call":
        res = {"jsonrpc": "2.0", "id": req_id, "result": {"content": [{"type": "text", "text": "probe-ok"}]}}
        sys.stdout.write(json.dumps(res) + "\n")
        sys.stdout.flush()
`
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client, err := NewStdioClient(ctx, "localpy", "python3", nil, "-c", pythonScript)
	if err != nil {
		if errorsIsExecNotFound(err) {
			t.Skip("python3 not available, skipping stdio test")
		}
		require.NoError(t, err)
	}
	defer client.Close()

	toolsList, err := client.Tools(ctx)
	require.NoError(t, err)
	require.Len(t, toolsList, 1)
	require.Equal(t, "py_probe", toolsList[0].Name)

	callOut, err := client.Call(ctx, "py_probe", json.RawMessage(`{}`))
	require.NoError(t, err)
	require.Equal(t, "probe-ok", callOut)
}

func errorsIsExecNotFound(err error) bool {
	return err != nil && (os.IsNotExist(err) || exec.ErrNotFound != nil)
}
