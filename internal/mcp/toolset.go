package mcp

import (
	"context"
	"crypto/tls"
	"net/http"
	"strings"
)

// ToolSet manages a set of connected MCP clients and exposes them as a unified
// tool interface to the local agent. It is safe for concurrent use.
type ToolSet struct {
	clients []*Client
}

// NewToolSet creates an unconnected ToolSet from a slice of clients.
// Call Connect on each client (or use ConnectAll) before using schemas/dispatch.
func NewToolSet(clients []*Client) *ToolSet {
	return &ToolSet{clients: clients}
}

// ConnectAll connects all clients concurrently. Returns the first error
// encountered; any clients that connected successfully are still usable.
func (ts *ToolSet) ConnectAll(ctx context.Context) error {
	type result struct{ err error }
	ch := make(chan result, len(ts.clients))
	for _, c := range ts.clients {
		go func(cl *Client) {
			ch <- result{cl.Connect(ctx)}
		}(c)
	}
	var first error
	for range ts.clients {
		if r := <-ch; r.err != nil && first == nil {
			first = r.err
		}
	}
	return first
}

// Schemas returns OpenAI function-call schema entries for all tools across all
// connected clients. Tool names are prefixed to avoid cross-server collisions.
func (ts *ToolSet) Schemas() []map[string]any {
	var result []map[string]any
	for _, c := range ts.clients {
		result = append(result, c.Schemas()...)
	}
	return result
}

// Dispatch routes a tool call (by prefixed name) to the appropriate MCP client
// and returns the text result. Returns ("", false) when the name doesn't match
// any known MCP tool, so the caller can fall through to built-in tools.
func (ts *ToolSet) Dispatch(ctx context.Context, toolName, argsJSON string) (string, bool) {
	for _, c := range ts.clients {
		orig, ok := c.OriginalToolName(toolName)
		if !ok {
			continue
		}
		res, err := c.Call(ctx, orig, argsJSON)
		if err != nil {
			return `{"error":"` + strings.ReplaceAll(err.Error(), `"`, `'`) + `"}`, true
		}
		text := res.Text()
		if res.IsError {
			return `{"error":` + jsonQuote(text) + `}`, true
		}
		return `{"output":` + jsonQuote(text) + `}`, true
	}
	return "", false
}

// Close closes all client sessions.
func (ts *ToolSet) Close(ctx context.Context) {
	for _, c := range ts.clients {
		c.Close(ctx)
	}
}

// Len returns the number of clients in the set.
func (ts *ToolSet) Len() int { return len(ts.clients) }

// Clients returns the underlying client slice (for status display / /mcp list).
func (ts *ToolSet) Clients() []*Client { return ts.clients }

// jsonQuote returns a JSON-encoded string literal for s.
func jsonQuote(s string) string {
	b, _ := jsonMarshalString(s)
	return string(b)
}

func jsonMarshalString(s string) ([]byte, error) {
	var sb strings.Builder
	sb.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			sb.WriteString(`\"`)
		case '\\':
			sb.WriteString(`\\`)
		case '\n':
			sb.WriteString(`\n`)
		case '\r':
			sb.WriteString(`\r`)
		case '\t':
			sb.WriteString(`\t`)
		default:
			if r < 0x20 {
				sb.WriteString(`\u00`)
				sb.WriteByte("0123456789abcdef"[r>>4])
				sb.WriteByte("0123456789abcdef"[r&0xf])
			} else {
				sb.WriteRune(r)
			}
		}
	}
	sb.WriteByte('"')
	return []byte(sb.String()), nil
}

// insecureTLSTransport returns an HTTP transport with TLS verification disabled.
func insecureTLSTransport() http.RoundTripper {
	return &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec
	}
}
