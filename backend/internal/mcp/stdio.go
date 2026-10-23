package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// StdioClient connects to an external MCP server running as a local subprocess
// communicating over newline-delimited JSON-RPC 2.0 on standard input and output.
type StdioClient struct {
	serverName string
	cmd        *exec.Cmd
	stdin      io.WriteCloser
	mu         sync.Mutex
	nextID     int
	pending    map[int]chan RPCResponse
	closeCh    chan struct{}
	once       sync.Once
}

// NewStdioClient spawns the target command and runs the MCP initialize handshake.
func NewStdioClient(ctx context.Context, serverName, command string, env map[string]string, args ...string) (*StdioClient, error) {
	if strings.TrimSpace(command) == "" {
		return nil, errors.New("mcp stdio command cannot be empty")
	}

	cmd := exec.Command(command, args...)
	if len(env) > 0 {
		cmd.Env = os.Environ()
		for k, v := range env {
			cmd.Env = append(cmd.Env, k+"="+v)
		}
	}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe error: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe error: %w", err)
	}

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, fmt.Errorf("failed to start mcp subprocess %q: %w", command, err)
	}

	sc := &StdioClient{
		serverName: serverName,
		cmd:        cmd,
		stdin:      stdin,
		pending:    make(map[int]chan RPCResponse),
		closeCh:    make(chan struct{}),
	}

	go sc.readLoop(stdout)

	// Perform MCP handshake
	if err := sc.initialize(ctx); err != nil {
		_ = sc.Close()
		return nil, fmt.Errorf("mcp initialize failed for %q: %w", serverName, err)
	}

	return sc, nil
}

func (c *StdioClient) ServerName() string {
	return c.serverName
}

func (c *StdioClient) readLoop(r io.Reader) {
	scanner := bufio.NewScanner(r)
	// Buffer up to 16MB for large DOM / tool list responses
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)

	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var resp RPCResponse
		if err := json.Unmarshal(line, &resp); err != nil {
			// Skip notifications or unparseable lines
			continue
		}

		if resp.ID == 0 {
			continue
		}

		c.mu.Lock()
		ch, exists := c.pending[resp.ID]
		if exists {
			delete(c.pending, resp.ID)
		}
		c.mu.Unlock()

		if exists && ch != nil {
			ch <- resp
		}
	}

	c.once.Do(func() {
		close(c.closeCh)
	})
}

func (c *StdioClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	ch := make(chan RPCResponse, 1)
	c.pending[id] = ch

	req := RPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	data, err := json.Marshal(req)
	if err != nil {
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, err
	}
	data = append(data, '\n')

	_, err = c.stdin.Write(data)
	c.mu.Unlock()

	if err != nil {
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, fmt.Errorf("write rpc frame: %w", err)
	}

	select {
	case <-ctx.Done():
		c.mu.Lock()
		delete(c.pending, id)
		c.mu.Unlock()
		return nil, ctx.Err()
	case <-c.closeCh:
		return nil, errors.New("mcp: connection closed")
	case resp := <-ch:
		if resp.Error != nil {
			return nil, resp.Error
		}
		return resp.Result, nil
	}
}

func (c *StdioClient) notify(method string, params any) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	req := RPCRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	_, err = c.stdin.Write(data)
	return err
}

func (c *StdioClient) initialize(ctx context.Context) error {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersionStdio,
		Capabilities:    map[string]any{},
		ClientInfo: ClientInfo{
			Name:    "oh-my-offensive-agent",
			Version: "1.0",
		},
	}

	_, err := c.call(ctx, "initialize", params)
	if err != nil {
		return err
	}

	return c.notify("notifications/initialized", map[string]any{})
}

// Tools lists remote tools via tools/list.
func (c *StdioClient) Tools(ctx context.Context) ([]RemoteTool, error) {
	raw, err := c.call(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}

	var res ToolListResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("decode tools/list: %w", err)
	}

	return res.Tools, nil
}

// Call executes a remote tool via tools/call and returns text output.
func (c *StdioClient) Call(ctx context.Context, toolName string, args json.RawMessage) (string, error) {
	var parsedArgs any
	if len(args) > 0 {
		_ = json.Unmarshal(args, &parsedArgs)
	}
	if parsedArgs == nil {
		parsedArgs = map[string]any{}
	}

	raw, err := c.call(ctx, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": parsedArgs,
	})
	if err != nil {
		return "", err
	}

	var res CallToolResult
	if err := json.Unmarshal(raw, &res); err != nil {
		return string(raw), nil
	}

	var sb strings.Builder
	for i, item := range res.Content {
		if i > 0 {
			sb.WriteString("\n")
		}
		if item.Text != "" {
			sb.WriteString(item.Text)
		} else if item.Data != "" {
			sb.WriteString(fmt.Sprintf("[%s data: %d bytes]", item.MimeType, len(item.Data)))
		}
	}

	out := sb.String()
	if res.IsError {
		return out, fmt.Errorf("tool %q returned error: %s", toolName, out)
	}
	return out, nil
}

// Close terminates the subprocess and closes input streams.
func (c *StdioClient) Close() error {
	var closeErr error
	c.once.Do(func() {
		close(c.closeCh)
		if c.stdin != nil {
			_ = c.stdin.Close()
		}
		if c.cmd != nil && c.cmd.Process != nil {
			_ = c.cmd.Process.Kill()
			_ = c.cmd.Wait()
		}
	})
	return closeErr
}
