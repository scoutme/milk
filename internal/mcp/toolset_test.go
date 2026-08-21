package mcp_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcp"
)

// newFakeMCPServer implements just enough of the Streamable HTTP JSON-RPC
// transport (initialize, notifications/initialized, tools/list, tools/call)
// to exercise ToolSet.Dispatch's routing. Calling a tool not in `tools`
// returns a JSON-RPC error, mirroring how a real server (e.g. Cloudflare's)
// responds to a tool name that doesn't exist on it.
func newFakeMCPServer(t *testing.T, tools map[string]string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			// Client.Close sends a session-teardown DELETE with no body.
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			ID     *int64          `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}

		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26"}}`, *req.ID)
			return
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			var names []string
			for name := range tools {
				names = append(names, fmt.Sprintf(`{"name":%q,"description":%q,"inputSchema":{"type":"object","properties":{}}}`, name, name))
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[%s]}}`, *req.ID, strings.Join(names, ","))
			return
		case "tools/call":
			var params struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(req.Params, &params)
			w.Header().Set("Content-Type", "application/json")
			if text, ok := tools[params.Name]; ok {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"content":[{"type":"text","text":%q}],"isError":false}}`, *req.ID, text)
				return
			}
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"error":{"code":-32602,"message":"Tool %s not found"}}`, *req.ID, params.Name)
			return
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestDispatch_PrefixCollisionPicksLongestMatch reproduces a real bug: when a
// server name is a literal string-prefix of a sibling server's name (e.g.
// "cloudflare" and "cloudflare-bindings"), a tool call meant for the longer,
// more specific server must not be swallowed by the shorter one just because
// its prefix also happens to match.
func TestDispatch_PrefixCollisionPicksLongestMatch(t *testing.T) {
	general := newFakeMCPServer(t, map[string]string{"docs": "general docs result"})
	defer general.Close()
	bindings := newFakeMCPServer(t, map[string]string{"workers_list": "bindings workers result"})
	defer bindings.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// "cloudflare" listed before "cloudflare-bindings", matching the reported
	// config order that triggered the misroute.
	ts := mcp.NewToolSet([]*mcp.Client{
		mcp.New(config.MCPServerConfig{Name: "cloudflare", URL: general.URL}),
		mcp.New(config.MCPServerConfig{Name: "cloudflare-bindings", URL: bindings.URL}),
	})
	if err := ts.ConnectAll(ctx); err != nil {
		t.Fatalf("ConnectAll: %v", err)
	}
	defer ts.Close(context.Background())

	result, ok := ts.Dispatch(ctx, "mcp_cloudflare_bindings_workers_list", "{}")
	if !ok {
		t.Fatal("Dispatch: tool not found")
	}
	if strings.Contains(result, "not found") || strings.Contains(result, "error") {
		t.Errorf("expected the bindings server's result, got misrouted error: %s", result)
	}
	if !strings.Contains(result, "bindings workers result") {
		t.Errorf("expected the bindings server's tool result, got %s", result)
	}

	// The shorter server's own tool still resolves correctly.
	result, ok = ts.Dispatch(ctx, "mcp_cloudflare_docs", "{}")
	if !ok {
		t.Fatal("Dispatch: tool not found")
	}
	if !strings.Contains(result, "general docs result") {
		t.Errorf("expected the general server's own tool result, got %s", result)
	}
}
