// Package mcp implements a minimal MCP (Model Context Protocol) client.
//
// Specification: https://modelcontextprotocol.io/specification/2025-03-26
//
// Transport: Streamable HTTP (2025-03-26) with automatic fallback to the
// deprecated HTTP+SSE transport (2024-11-05) for older servers.
// Each request is a JSON-RPC 2.0 message POSTed to the MCP endpoint; responses
// arrive either as a single JSON object (Content-Type: application/json) or as
// a server-sent event stream (Content-Type: text/event-stream).
//
// Lifecycle: initialize → initialized → tools/list → tools/call (repeated).
// The session ID returned by the server in the Mcp-Session-Id response header
// is attached to all subsequent requests.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
)

const protocolVersion = "2025-03-26"

// Tool is an MCP tool definition as returned by tools/list.
type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

// CallResult is the normalised result of a tools/call invocation.
type CallResult struct {
	Content []ContentItem `json:"content"`
	IsError bool          `json:"isError"`
}

// ContentItem is one element of a tool call result.
type ContentItem struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
	// image/audio/resource fields omitted — milk only surfaces text to the LLM
}

// Text returns the concatenated text of all text-type content items.
func (r CallResult) Text() string {
	var sb strings.Builder
	for _, c := range r.Content {
		if c.Type == "text" {
			sb.WriteString(c.Text)
		}
	}
	return sb.String()
}

// jsonrpcRequest is a JSON-RPC 2.0 request object.
type jsonrpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      *int64 `json:"id,omitempty"` // nil for notifications
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// jsonrpcResponse is a JSON-RPC 2.0 response object.
type jsonrpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      *int64          `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *jsonrpcError   `json:"error,omitempty"`
}

type jsonrpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *jsonrpcError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.Code, e.Message)
}

// Client is a connected MCP client for a single server.
// Call Connect before using; call Close when done.
type Client struct {
	cfg            config.MCPServerConfig
	endpoint       string
	sessionID      string
	httpClient     *http.Client
	idSeq          atomic.Int64
	mu             sync.Mutex
	tools          []Tool
	ready          bool
	dead           bool          // set after a lazy-reconnect attempt fails; prevents per-turn retry storms
	connectTimeout time.Duration // hint for startup callers; see ConnectTimeout()

	// tokenOnce caches a dynamic Bearer token resolved from TokenCmd.
	tokenOnce   sync.Once
	cachedToken string
}

// New builds a Client from an MCPServerConfig but does not connect yet.
func New(cfg config.MCPServerConfig) *Client {
	timeout := 30 * time.Second
	if cfg.Timeout != "" {
		if d, err := time.ParseDuration(cfg.Timeout); err == nil && d > 0 {
			timeout = d
		}
	}
	connectTimeout := 5 * time.Second
	if cfg.ConnectTimeout != "" {
		if d, err := time.ParseDuration(cfg.ConnectTimeout); err == nil && d > 0 {
			connectTimeout = d
		}
	}
	transport := http.DefaultTransport
	if cfg.TLSSkipVerify {
		transport = insecureTLSTransport()
	}
	return &Client{
		cfg:      cfg,
		endpoint: cfg.URL,
		httpClient: &http.Client{
			Timeout:   timeout,
			Transport: transport,
		},
		connectTimeout: connectTimeout,
	}
}

// Connect performs the MCP initialization handshake and fetches the initial
// tool list. It is safe to call from multiple goroutines; only the first call
// takes effect. Returns an error if the server is unreachable or rejects the
// initialize request.
func (c *Client) Connect(ctx context.Context) (retErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ready {
		return nil
	}
	defer func() {
		if retErr != nil {
			obs.Info("mcp.connect.failed", "server", c.cfg.Name, "error", retErr.Error())
		}
	}()

	// Resolve dynamic token upfront so it's available for all requests.
	if strings.ToLower(c.cfg.Auth) == "token_cmd" && c.cfg.TokenCmd != "" {
		if err := c.resolveToken(); err != nil {
			return fmt.Errorf("mcp %q: token_cmd failed: %w", c.cfg.Name, err)
		}
	}

	// Phase 1: initialize
	initResult, err := c.roundtrip(ctx, "initialize", map[string]any{
		"protocolVersion": protocolVersion,
		"capabilities": map[string]any{
			"roots":    map[string]any{},
			"sampling": map[string]any{},
		},
		"clientInfo": map[string]any{
			"name":    "milk",
			"version": "0.1.0",
		},
	})
	if err != nil {
		// Fallback: try the old HTTP+SSE transport (2024-11-05).
		// If the server returns 405/404 on POST, we don't support the old transport
		// without a persistent SSE connection — surface the original error.
		return fmt.Errorf("mcp %q: initialize failed: %w", c.cfg.Name, err)
	}

	// Extract session ID from the stored response header (set during roundtrip).
	// Accept any protocolVersion the server responds with.
	_ = initResult

	// Phase 2: send initialized notification (no response expected).
	if err := c.notify(ctx, "notifications/initialized", nil); err != nil {
		return fmt.Errorf("mcp %q: initialized notification failed: %w", c.cfg.Name, err)
	}

	// Phase 3: list tools.
	tools, err := c.listTools(ctx)
	if err != nil {
		return fmt.Errorf("mcp %q: tools/list failed: %w", c.cfg.Name, err)
	}
	c.tools = tools
	c.ready = true
	obs.Info("mcp.connect", "server", c.cfg.Name, "tools", len(c.tools))
	return nil
}

// Tools returns the cached list of tools discovered during Connect.
// RefreshTools can be called to update the list.
func (c *Client) Tools() []Tool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.tools
}

// RefreshTools re-issues tools/list and updates the cached list.
// If the client did not connect successfully at startup, it performs a lazy
// reconnect using ctx before refreshing.
func (c *Client) RefreshTools(ctx context.Context) error {
	c.mu.Lock()
	ready := c.ready
	c.mu.Unlock()
	if !ready {
		if err := c.Connect(ctx); err != nil {
			return err
		}
	}
	tools, err := c.listTools(ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.tools = tools
	c.mu.Unlock()
	return nil
}

// Call invokes a tool by name with the given arguments JSON and returns the result.
// If the client did not connect successfully at startup (e.g. server was unreachable),
// it performs a single lazy reconnect (bounded by connectTimeout) before proceeding;
// on failure the client is marked dead and subsequent calls return immediately.
func (c *Client) Call(ctx context.Context, toolName, argsJSON string) (CallResult, error) {
	c.mu.Lock()
	ready, dead := c.ready, c.dead
	c.mu.Unlock()
	if dead {
		return CallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: "MCP server " + c.cfg.Name + " is unavailable"}}}, nil
	}
	if !ready {
		obs.Info("mcp.call.reconnect", "server", c.cfg.Name, "tool", toolName)
		reconnCtx, cancel := context.WithTimeout(ctx, c.connectTimeout)
		defer cancel()
		if err := c.Connect(reconnCtx); err != nil {
			c.mu.Lock()
			c.dead = true
			c.mu.Unlock()
			return CallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: err.Error()}}}, nil
		}
	}

	var args map[string]any
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return CallResult{}, fmt.Errorf("invalid tool arguments: %w", err)
		}
	}

	raw, err := c.roundtrip(ctx, "tools/call", map[string]any{
		"name":      toolName,
		"arguments": args,
	})
	if err != nil {
		obs.Info("mcp.call.failed", "server", c.cfg.Name, "tool", toolName, "error", err.Error())
		return CallResult{IsError: true, Content: []ContentItem{{Type: "text", Text: err.Error()}}}, nil
	}

	var result CallResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return CallResult{}, fmt.Errorf("mcp %q tools/call: unexpected response: %w", c.cfg.Name, err)
	}
	obs.Info("mcp.call", "server", c.cfg.Name, "tool", toolName, "is_error", result.IsError)
	return result, nil
}

// listTools fetches all pages of tools/list and returns the flat list.
func (c *Client) listTools(ctx context.Context) ([]Tool, error) {
	var all []Tool
	cursor := ""
	for {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		raw, err := c.roundtrip(ctx, "tools/list", params)
		if err != nil {
			return nil, err
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, fmt.Errorf("tools/list response: %w", err)
		}
		all = append(all, page.Tools...)
		if page.NextCursor == "" {
			break
		}
		cursor = page.NextCursor
	}
	return all, nil
}

// roundtrip sends a JSON-RPC request and returns the result bytes.
// It attaches the session ID header when one has been established and updates
// the session ID from the response header on the initialize call.
func (c *Client) roundtrip(ctx context.Context, method string, params any) (json.RawMessage, error) {
	id := c.idSeq.Add(1)
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		ID:      &id,
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.applyAuth(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}

	// Capture session ID from initialize response.
	if method == "initialize" {
		if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
			c.sessionID = sid
		}
	}

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		raw = extractSSEData(raw)
	}
	return c.readJSONResult(bytes.NewReader(raw), id)
}

// notify sends a JSON-RPC notification (no ID, no response expected).
func (c *Client) notify(ctx context.Context, method string, params any) error {
	req := jsonrpcRequest{
		JSONRPC: "2.0",
		Method:  method,
		Params:  params,
	}
	body, err := json.Marshal(req)
	if err != nil {
		return err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream, application/json")
	if c.sessionID != "" {
		httpReq.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	c.applyAuth(httpReq)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 202 Accepted is the expected response for notifications/responses.
	if resp.StatusCode != http.StatusAccepted && resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	return nil
}

// readJSONResult decodes a single JSON-RPC response from r.
func (c *Client) readJSONResult(r io.Reader, id int64) (json.RawMessage, error) {
	var rpc jsonrpcResponse
	if err := json.NewDecoder(r).Decode(&rpc); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	if rpc.Error != nil {
		return nil, rpc.Error
	}
	return rpc.Result, nil
}

// readSSEResult reads an SSE stream and returns the result of the response
// matching id. Server requests/notifications that arrive before the response
// are silently discarded (milk does not implement server-initiated requests).
func (c *Client) readSSEResult(r io.Reader, id int64) (json.RawMessage, error) {
	scanner := bufio.NewScanner(r)
	var dataBuf strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "data: ") {
			dataBuf.WriteString(strings.TrimPrefix(line, "data: "))
		} else if line == "" && dataBuf.Len() > 0 {
			// End of one SSE event — try to decode it.
			var rpc jsonrpcResponse
			if err := json.Unmarshal([]byte(dataBuf.String()), &rpc); err != nil {
				dataBuf.Reset()
				continue
			}
			dataBuf.Reset()
			if rpc.ID == nil || *rpc.ID != id {
				// Server notification or response to a different request — ignore.
				continue
			}
			if rpc.Error != nil {
				return nil, rpc.Error
			}
			return rpc.Result, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("SSE stream error: %w", err)
	}
	return nil, fmt.Errorf("SSE stream ended without response for id %d", id)
}

// applyAuth sets the Authorization header according to the configured auth method.
func (c *Client) applyAuth(req *http.Request) {
	switch strings.ToLower(c.cfg.Auth) {
	case "bearer":
		if c.cfg.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
		}
	case "token_cmd":
		if c.cachedToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.cachedToken)
		}
	}
}

// resolveToken executes TokenCmd and stores the trimmed stdout as cachedToken.
func (c *Client) resolveToken() error {
	out, err := exec.Command("sh", "-c", c.cfg.TokenCmd).Output()
	if err != nil {
		return err
	}
	c.cachedToken = strings.TrimSpace(string(out))
	return nil
}

// Schemas converts the client's tool list into OpenAI function-call schema
// entries that can be appended to the local agent's tools array.
// Each tool name is prefixed with "mcp_<serverName>_" to avoid collisions.
// If the client did not connect successfully at startup (e.g. connect timeout),
// it performs a single lazy reconnect (bounded by connectTimeout) before returning;
// on failure the client is marked dead and nil is returned for all future calls.
func (c *Client) Schemas(ctx context.Context) []map[string]any {
	c.mu.Lock()
	ready, dead := c.ready, c.dead
	c.mu.Unlock()
	if dead {
		return nil
	}
	if !ready {
		reconnCtx, cancel := context.WithTimeout(ctx, c.connectTimeout)
		defer cancel()
		if err := c.Connect(reconnCtx); err != nil {
			obs.Info("mcp.reconnect.failed", "server", c.cfg.Name, "error", err.Error())
			c.mu.Lock()
			c.dead = true
			c.mu.Unlock()
			return nil
		}
		c.mu.Lock()
		obs.Info("mcp.reconnect", "server", c.cfg.Name, "tools", len(c.tools))
		c.mu.Unlock()
	}

	c.mu.Lock()
	tools := c.tools
	c.mu.Unlock()

	result := make([]map[string]any, 0, len(tools))
	prefix := mcpToolPrefix(c.cfg.Name)
	for _, t := range tools {
		schema := t.InputSchema
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		desc := t.Description
		if desc == "" {
			desc = t.Name
		}
		result = append(result, map[string]any{
			"type": "function",
			"function": map[string]any{
				"name":        prefix + sanitiseMCPToolName(t.Name),
				"description": fmt.Sprintf("[MCP:%s] %s", c.cfg.Name, desc),
				"parameters":  schema,
			},
		})
	}
	return result
}

// OriginalToolName strips the mcp_<serverName>_ prefix and returns the
// original MCP tool name. Returns ("", false) when name doesn't match.
func (c *Client) OriginalToolName(name string) (string, bool) {
	prefix := mcpToolPrefix(c.cfg.Name)
	if !strings.HasPrefix(name, prefix) {
		return "", false
	}
	return strings.TrimPrefix(name, prefix), true
}

// ServerName returns the config name of this server.
func (c *Client) ServerName() string { return c.cfg.Name }

// ConnectTimeout returns the configured connect-handshake timeout.
// Startup callers (e.g. attachMCPToolSet) use this to derive a per-client
// context deadline rather than applying a single global timeout.
func (c *Client) ConnectTimeout() time.Duration { return c.connectTimeout }

// Close terminates the session. For HTTP transport this sends a DELETE to the
// MCP endpoint with the session ID header; failure is silently ignored.
func (c *Client) Close(ctx context.Context) {
	c.mu.Lock()
	sid := c.sessionID
	c.mu.Unlock()
	if sid == "" {
		return
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, c.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", sid)
	c.applyAuth(req)
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return
	}
	resp.Body.Close()
}

// extractSSEData scans a raw SSE body and returns the payload of the first
// "data:" line. If no such line is found the original bytes are returned so
// the caller can still attempt to unmarshal a plain JSON response.
func extractSSEData(body []byte) []byte {
	for _, line := range bytes.Split(body, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if bytes.HasPrefix(line, []byte("data:")) {
			return bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
	}
	return body // fallback: return as-is
}

// mcpToolPrefix returns the OpenAI-safe prefix for a given MCP server name.
// e.g. "my-tools" → "mcp_my_tools_"
func mcpToolPrefix(serverName string) string {
	return "mcp_" + sanitiseMCPToolName(serverName) + "_"
}

// sanitiseMCPToolName lowercases s and replaces non-alphanumeric runs with "_".
func sanitiseMCPToolName(s string) string {
	var sb strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			sb.WriteRune(r)
			prevUnderscore = false
		} else if !prevUnderscore {
			sb.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(sb.String(), "_")
}
