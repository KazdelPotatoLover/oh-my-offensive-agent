package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"agent-workbench/backend/internal/tools"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Manager manages MCP server definitions, tool caches, and active runtime clients.
type Manager struct {
	pool *pgxpool.Pool
	mu   sync.RWMutex
}

// NewManager creates an MCP manager backed by PostgreSQL.
func NewManager(pool *pgxpool.Pool) *Manager {
	return &Manager{pool: pool}
}

// CreateServerRequest is the payload for adding an MCP server.
type CreateServerRequest struct {
	Name      string            `json:"name"`
	Transport string            `json:"transport"` // "stdio", "http", "sse"
	Command   string            `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       string            `json:"url,omitempty"`
	Enabled   *bool             `json:"enabled,omitempty"`
	Insecure  *bool             `json:"insecure,omitempty"`
}

// UpdateServerRequest is the payload for updating an MCP server.
type UpdateServerRequest struct {
	Transport *string           `json:"transport,omitempty"`
	Command   *string           `json:"command,omitempty"`
	Args      []string          `json:"args,omitempty"`
	Env       map[string]string `json:"env,omitempty"`
	URL       *string           `json:"url,omitempty"`
	Enabled   *bool             `json:"enabled,omitempty"`
	Insecure  *bool             `json:"insecure,omitempty"`
}

// ListServers returns all configured MCP servers along with their cached tools.
func (m *Manager) ListServers(ctx context.Context) ([]MCPServerModel, error) {
	query := `
		SELECT id, name, transport, COALESCE(command, ''), args, env, COALESCE(url, ''), enabled, insecure, created_at, updated_at
		FROM mcp_servers
		ORDER BY id ASC
	`
	rows, err := m.pool.Query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list mcp servers: %w", err)
	}
	defer rows.Close()

	var servers []MCPServerModel
	for rows.Next() {
		var s MCPServerModel
		var argsBytes, envBytes []byte
		if err := rows.Scan(
			&s.ID, &s.Name, &s.Transport, &s.Command, &argsBytes, &envBytes, &s.URL, &s.Enabled, &s.Insecure, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan mcp server: %w", err)
		}

		if len(argsBytes) > 0 {
			_ = json.Unmarshal(argsBytes, &s.Args)
		}
		if len(envBytes) > 0 {
			_ = json.Unmarshal(envBytes, &s.Env)
		}
		if s.Args == nil {
			s.Args = []string{}
		}
		if s.Env == nil {
			s.Env = map[string]string{}
		}

		servers = append(servers, s)
	}

	// Populate cached tools for each server
	for i := range servers {
		toolsList, err := m.GetCachedTools(ctx, servers[i].ID)
		if err == nil {
			servers[i].Tools = toolsList
		}
	}

	return servers, nil
}

// GetServer retrieves an MCP server by ID.
func (m *Manager) GetServer(ctx context.Context, id int64) (*MCPServerModel, error) {
	query := `
		SELECT id, name, transport, COALESCE(command, ''), args, env, COALESCE(url, ''), enabled, insecure, created_at, updated_at
		FROM mcp_servers
		WHERE id = $1
	`
	var s MCPServerModel
	var argsBytes, envBytes []byte
	err := m.pool.QueryRow(ctx, query, id).Scan(
		&s.ID, &s.Name, &s.Transport, &s.Command, &argsBytes, &envBytes, &s.URL, &s.Enabled, &s.Insecure, &s.CreatedAt, &s.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("mcp server %d not found", id)
		}
		return nil, err
	}

	if len(argsBytes) > 0 {
		_ = json.Unmarshal(argsBytes, &s.Args)
	}
	if len(envBytes) > 0 {
		_ = json.Unmarshal(envBytes, &s.Env)
	}
	if s.Args == nil {
		s.Args = []string{}
	}
	if s.Env == nil {
		s.Env = map[string]string{}
	}

	s.Tools, _ = m.GetCachedTools(ctx, s.ID)
	return &s, nil
}

// CreateServer registers a new MCP server.
func (m *Manager) CreateServer(ctx context.Context, req CreateServerRequest) (*MCPServerModel, error) {
	if req.Name == "" {
		return nil, errors.New("name cannot be empty")
	}
	if req.Transport != "stdio" && req.Transport != "http" && req.Transport != "sse" {
		return nil, fmt.Errorf("invalid transport %q", req.Transport)
	}

	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	insecure := false
	if req.Insecure != nil {
		insecure = *req.Insecure
	}

	argsJSON, _ := json.Marshal(req.Args)
	if len(req.Args) == 0 {
		argsJSON = []byte("[]")
	}
	envJSON, _ := json.Marshal(req.Env)
	if len(req.Env) == 0 {
		envJSON = []byte("{}")
	}

	var id int64
	var createdAt, updatedAt time.Time
	err := m.pool.QueryRow(ctx, `
		INSERT INTO mcp_servers (name, transport, command, args, env, url, enabled, insecure)
		VALUES ($1, $2, NULLIF($3, ''), $4, $5, NULLIF($6, ''), $7, $8)
		RETURNING id, created_at, updated_at
	`, req.Name, req.Transport, req.Command, argsJSON, envJSON, req.URL, enabled, insecure).Scan(&id, &createdAt, &updatedAt)
	if err != nil {
		return nil, fmt.Errorf("insert mcp server: %w", err)
	}

	return m.GetServer(ctx, id)
}

// UpdateServer updates an existing MCP server.
func (m *Manager) UpdateServer(ctx context.Context, id int64, req UpdateServerRequest) (*MCPServerModel, error) {
	current, err := m.GetServer(ctx, id)
	if err != nil {
		return nil, err
	}

	transport := current.Transport
	if req.Transport != nil {
		transport = *req.Transport
	}
	command := current.Command
	if req.Command != nil {
		command = *req.Command
	}
	urlVal := current.URL
	if req.URL != nil {
		urlVal = *req.URL
	}
	enabled := current.Enabled
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	insecure := current.Insecure
	if req.Insecure != nil {
		insecure = *req.Insecure
	}

	argsJSON, _ := json.Marshal(current.Args)
	if req.Args != nil {
		argsJSON, _ = json.Marshal(req.Args)
	}
	envJSON, _ := json.Marshal(current.Env)
	if req.Env != nil {
		envJSON, _ = json.Marshal(req.Env)
	}

	_, err = m.pool.Exec(ctx, `
		UPDATE mcp_servers
		SET transport = $1, command = NULLIF($2, ''), args = $3, env = $4, url = NULLIF($5, ''), enabled = $6, insecure = $7, updated_at = now()
		WHERE id = $8
	`, transport, command, argsJSON, envJSON, urlVal, enabled, insecure, id)
	if err != nil {
		return nil, fmt.Errorf("update mcp server: %w", err)
	}

	return m.GetServer(ctx, id)
}

// DeleteServer deletes an MCP server and cascades cached tools.
func (m *Manager) DeleteServer(ctx context.Context, id int64) error {
	res, err := m.pool.Exec(ctx, `DELETE FROM mcp_servers WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete mcp server: %w", err)
	}
	if res.RowsAffected() == 0 {
		return fmt.Errorf("mcp server %d not found", id)
	}
	return nil
}

// GetCachedTools retrieves previously discovered tools for a server.
func (m *Manager) GetCachedTools(ctx context.Context, serverID int64) ([]MCPToolModel, error) {
	query := `
		SELECT id, server_id, tool_name, COALESCE(description, ''), schema, discovered_at
		FROM mcp_tools_cache
		WHERE server_id = $1
		ORDER BY tool_name ASC
	`
	rows, err := m.pool.Query(ctx, query, serverID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var res []MCPToolModel
	for rows.Next() {
		var t MCPToolModel
		if err := rows.Scan(&t.ID, &t.ServerID, &t.ToolName, &t.Description, &t.Schema, &t.DiscoveredAt); err != nil {
			return nil, err
		}
		res = append(res, t)
	}
	return res, nil
}

// Connect dials an MCP server according to its configured transport.
func (m *Manager) Connect(ctx context.Context, s *MCPServerModel) (Client, error) {
	switch s.Transport {
	case "stdio":
		return NewStdioClient(ctx, s.Name, s.Command, s.Env, s.Args...)
	case "http", "sse":
		return NewHTTPClient(ctx, s.Name, s.Transport, s.URL, s.Env, s.Insecure)
	default:
		return nil, fmt.Errorf("unknown transport %q", s.Transport)
	}
}

// DiscoverTools dials an MCP server, queries its tools/list, and persists them into mcp_tools_cache.
func (m *Manager) DiscoverTools(ctx context.Context, serverID int64) ([]RemoteTool, error) {
	server, err := m.GetServer(ctx, serverID)
	if err != nil {
		return nil, err
	}

	cl, err := m.Connect(ctx, server)
	if err != nil {
		return nil, fmt.Errorf("connect to %q: %w", server.Name, err)
	}
	defer cl.Close()

	remoteTools, err := cl.Tools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list tools from %q: %w", server.Name, err)
	}

	tx, err := m.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// Clean existing tool cache for this server
	_, err = tx.Exec(ctx, `DELETE FROM mcp_tools_cache WHERE server_id = $1`, serverID)
	if err != nil {
		return nil, fmt.Errorf("clean cache: %w", err)
	}

	for _, rt := range remoteTools {
		schemaJSON := rt.InputSchema
		if len(schemaJSON) == 0 {
			schemaJSON = json.RawMessage(`{}`)
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO mcp_tools_cache (server_id, tool_name, description, schema)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (server_id, tool_name) DO UPDATE SET
				description = EXCLUDED.description,
				schema = EXCLUDED.schema,
				discovered_at = now()
		`, serverID, rt.Name, rt.Description, schemaJSON)
		if err != nil {
			return nil, fmt.Errorf("cache tool %q: %w", rt.Name, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}

	slog.Info("mcp tools cached successfully", "server", server.Name, "tools_count", len(remoteTools))
	return remoteTools, nil
}

// BuildAllTools connects to all enabled MCP servers, fetches their tools,
// wraps them in ToolAdapter (implementing tools.Tool), and returns the slice
// along with a cleanup function that callers must defer to terminate clients.
func (m *Manager) BuildAllTools(ctx context.Context) ([]tools.Tool, func()) {
	servers, err := m.ListServers(ctx)
	if err != nil {
		slog.Error("failed to list mcp servers for runtime assembly", "error", err)
		return nil, func() {}
	}

	var assembled []tools.Tool
	var closers []Client

	for _, s := range servers {
		if !s.Enabled {
			continue
		}
		sCopy := s
		cl, err := m.Connect(ctx, &sCopy)
		if err != nil {
			slog.Warn("skipping mcp server due to connection failure", "server", s.Name, "error", err)
			continue
		}
		closers = append(closers, cl)

		ts, err := cl.Tools(ctx)
		if err != nil {
			slog.Warn("skipping mcp server tools due to list error", "server", s.Name, "error", err)
			continue
		}

		for _, t := range ts {
			adapter := NewToolAdapter(s.Name, t, cl)
			assembled = append(assembled, adapter)
		}
	}

	cleanup := func() {
		for _, cl := range closers {
			_ = cl.Close()
		}
	}

	return assembled, cleanup
}
