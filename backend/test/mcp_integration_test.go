package test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"agent-workbench/backend/internal/mcp"

	"github.com/stretchr/testify/require"
)

func mockMCPHTTPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "DELETE" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if r.Method != "POST" {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var req mcp.RPCRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Mcp-Session-Id", "sess-integ-test-456")

		switch req.Method {
		case "initialize":
			resp := mcp.RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"protocolVersion": "2025-06-18",
					"capabilities": {"tools": {}},
					"serverInfo": {"name": "integ-mcp", "version": "1.0"}
				}`),
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "tools/list":
			resp := mcp.RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"tools": [
						{
							"name": "lookup_cve",
							"description": "Look up vulnerability details by CVE ID",
							"inputSchema": {
								"type": "object",
								"properties": {
									"cve_id": {"type": "string"}
								},
								"required": ["cve_id"]
							}
						}
					]
				}`),
			}
			_ = json.NewEncoder(w).Encode(resp)

		case "tools/call":
			resp := mcp.RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Result: json.RawMessage(`{
					"content": [
						{"type": "text", "text": "CVE-2023-38606: Remote Code Execution flaw"}
					]
				}`),
			}
			_ = json.NewEncoder(w).Encode(resp)

		default:
			resp := mcp.RPCResponse{
				JSONRPC: "2.0",
				ID:      req.ID,
				Error: &mcp.RPCError{
					Code:    -32601,
					Message: "method not found",
				},
			}
			_ = json.NewEncoder(w).Encode(resp)
		}
	})
}

func TestMCP_SubsystemIntegrationAndHTTPAPI(t *testing.T) {
	env := setupTestEnv(t)
	defer env.teardown()
	ctx := context.Background()

	// Spin up mock MCP service
	mockServer := httptest.NewServer(mockMCPHTTPHandler())
	defer mockServer.Close()

	mgr := mcp.NewManager(env.pool)

	// 1. Verify default seed servers in DB
	servers, err := mgr.ListServers(ctx)
	require.NoError(t, err)
	var foundBrowser, foundScopeSentry bool
	for _, s := range servers {
		if s.Name == "browser" {
			foundBrowser = true
			require.Equal(t, "stdio", s.Transport)
			require.Equal(t, "npx", s.Command)
		}
		if s.Name == "ScopeSentry" {
			foundScopeSentry = true
			require.Equal(t, "http", s.Transport)
		}
	}
	require.True(t, foundBrowser, "browser MCP should be seeded")
	require.True(t, foundScopeSentry, "ScopeSentry MCP should be seeded")

	// 2. Register mock server via Manager
	newServer, err := mgr.CreateServer(ctx, mcp.CreateServerRequest{
		Name:      "vdb_service",
		Transport: "http",
		URL:       mockServer.URL,
	})
	require.NoError(t, err)
	require.Equal(t, "vdb_service", newServer.Name)
	require.True(t, newServer.Enabled)

	// 3. Discover tools
	discovered, err := mgr.DiscoverTools(ctx, newServer.ID)
	require.NoError(t, err)
	require.Len(t, discovered, 1)
	require.Equal(t, "lookup_cve", discovered[0].Name)

	// 4. Verify tool cached in DB
	cached, err := mgr.GetCachedTools(ctx, newServer.ID)
	require.NoError(t, err)
	require.Len(t, cached, 1)
	require.Equal(t, "lookup_cve", cached[0].ToolName)

	// 5. Test BuildAllTools runtime assembly
	assembledTools, cleanup := mgr.BuildAllTools(ctx)
	defer cleanup()

	var cveToolFound bool
	for _, tool := range assembledTools {
		if tool.Name() == "mcp__vdb_service__lookup_cve" {
			cveToolFound = true
			out, callErr := tool.Execute(ctx, []byte(`{"cve_id":"CVE-2023-38606"}`))
			require.NoError(t, callErr)
			require.Contains(t, out, "CVE-2023-38606: Remote Code Execution flaw")
		}
	}
	require.True(t, cveToolFound, "mcp__vdb_service__lookup_cve should be assembled")

	// 6. Test HTTP API routes via env.router
	// GET /api/mcp/servers
	req := httptest.NewRequest("GET", "/api/mcp/servers", nil)
	rec := httptest.NewRecorder()
	env.router.ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)

	var listResp struct {
		Servers []mcp.MCPServerModel `json:"servers"`
	}
	err = json.Unmarshal(rec.Body.Bytes(), &listResp)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(listResp.Servers), 3)

	// POST /api/mcp/servers/{id}/discover
	discoverReq := httptest.NewRequest("POST", fmt.Sprintf("/api/mcp/servers/%d/discover", newServer.ID), nil)
	discoverRec := httptest.NewRecorder()
	env.router.ServeHTTP(discoverRec, discoverReq)
	require.Equal(t, http.StatusOK, discoverRec.Code)

	// DELETE /api/mcp/servers/{id}
	delReq := httptest.NewRequest("DELETE", fmt.Sprintf("/api/mcp/servers/%d", newServer.ID), nil)
	delRec := httptest.NewRecorder()
	env.router.ServeHTTP(delRec, delReq)
	require.Equal(t, http.StatusOK, delRec.Code)

	// Verify deleted
	_, err = mgr.GetServer(ctx, newServer.ID)
	require.Error(t, err)
}
