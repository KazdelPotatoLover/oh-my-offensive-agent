package mcp

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// HTTPClient implements the MCP Streamable HTTP and SSE protocol for remote servers.
type HTTPClient struct {
	serverName string
	url        string
	headers    map[string]string
	client     *http.Client
	mu         sync.Mutex
	nextID     int
	sessionID  string
	transport  string // "http" or "sse"
}

// NewHTTPClient creates and initializes a Streamable HTTP or legacy SSE client.
func NewHTTPClient(ctx context.Context, serverName, transport, endpointURL string, env map[string]string, insecure bool) (*HTTPClient, error) {
	if strings.TrimSpace(endpointURL) == "" {
		return nil, errors.New("mcp endpoint url cannot be empty")
	}

	transportMode := strings.ToLower(transport)
	if transportMode != "http" && transportMode != "sse" {
		transportMode = "http"
	}

	transportConfig := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
	}
	if insecure {
		transportConfig.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}

	httpClient := &http.Client{
		Transport: transportConfig,
		Timeout:   120 * time.Second,
	}

	headers := make(map[string]string)
	for k, v := range env {
		headers[k] = v
	}

	c := &HTTPClient{
		serverName: serverName,
		url:        endpointURL,
		headers:    headers,
		client:     httpClient,
		transport:  transportMode,
	}

	if err := c.initialize(ctx); err != nil {
		return nil, fmt.Errorf("mcp http initialize failed for %q: %w", serverName, err)
	}

	return c, nil
}

func (c *HTTPClient) ServerName() string {
	return c.serverName
}

func (c *HTTPClient) post(ctx context.Context, reqBody any) (*RPCResponse, error) {
	data, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", c.url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json, text/event-stream")
	httpReq.Header.Set("MCP-Protocol-Version", ProtocolVersionHTTP)

	c.mu.Lock()
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	for k, v := range c.headers {
		httpReq.Header.Set(k, v)
	}
	c.mu.Unlock()

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if newSession := resp.Header.Get("Mcp-Session-Id"); newSession != "" {
		c.mu.Lock()
		c.sessionID = newSession
		c.mu.Unlock()
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("mcp http status %d: %s", resp.StatusCode, string(snippet))
	}

	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "text/event-stream") {
		// Parse SSE stream
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data:") {
				payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				var rpcResp RPCResponse
				if err := json.Unmarshal([]byte(payload), &rpcResp); err == nil && rpcResp.ID != 0 {
					return &rpcResp, nil
				}
			}
		}
		return nil, errors.New("mcp: no response in sse event stream")
	}

	var rpcResp RPCResponse
	if err := json.NewDecoder(resp.Body).Decode(&rpcResp); err != nil {
		return nil, fmt.Errorf("decode json response: %w", err)
	}
	return &rpcResp, nil
}

func (c *HTTPClient) call(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	c.nextID++
	id := c.nextID
	c.mu.Unlock()

	req := RPCRequest{
		JSONRPC: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	}

	resp, err := c.post(ctx, req)
	if err != nil {
		return nil, err
	}
	if resp.Error != nil {
		return nil, resp.Error
	}
	return resp.Result, nil
}

func (c *HTTPClient) initialize(ctx context.Context) error {
	params := InitializeParams{
		ProtocolVersion: ProtocolVersionHTTP,
		Capabilities:    map[string]any{},
		ClientInfo: ClientInfo{
			Name:    "oh-my-offensive-agent",
			Version: "1.0",
		},
	}

	_, err := c.call(ctx, "initialize", params)
	return err
}

func (c *HTTPClient) Tools(ctx context.Context) ([]RemoteTool, error) {
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

func (c *HTTPClient) Call(ctx context.Context, toolName string, args json.RawMessage) (string, error) {
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

func (c *HTTPClient) Close() error {
	c.mu.Lock()
	sid := c.sessionID
	c.sessionID = ""
	c.mu.Unlock()

	if sid != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, "DELETE", c.url, nil)
		if err == nil {
			req.Header.Set("Mcp-Session-Id", sid)
			for k, v := range c.headers {
				req.Header.Set(k, v)
			}
			_, _ = c.client.Do(req)
		}
	}
	return nil
}
