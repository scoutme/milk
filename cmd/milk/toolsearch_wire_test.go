package main

// Wire-level test for deferred tool schemas with on-demand loading
// (issue #480, tool_search "auto").
//
// Two real turns of a real local agent run against a scripted
// OpenAI-compatible server with one fake MCP tool on the wire. The model is
// scripted to call tool_search mid-turn, then the tool it found. The wire
// contract under test: the tools array is byte-identical across every request
// of both turns (loading never touches the wire — stub stability), the search
// result is delivered as tool-result text carrying the full schema, and the
// deferred tool call executes once its schema is loaded.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

// searchWireServer records the raw JSON of every request's tools array and
// messages and answers from a fixed script:
//   - user turn asking to "frobnicate" → a tool_search call;
//   - tool result carrying "Loaded" (the search hit) → the deferred MCP call;
//   - anything else (tool results, follow-ups) → a plain text reply.
type searchWireServer struct {
	mu       sync.Mutex
	tools    []json.RawMessage // per request, raw "tools" field
	messages [][]json.RawMessage
}

func (s *searchWireServer) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Messages []json.RawMessage `json:"messages"`
			Tools    json.RawMessage   `json:"tools"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.tools = append(s.tools, req.Tools)
		s.messages = append(s.messages, req.Messages)
		n := len(s.messages)
		s.mu.Unlock()

		wantSearch, wantWidget := false, false
		if k := len(req.Messages); k > 0 {
			var last struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal(req.Messages[k-1], &last)
			wantSearch = last.Role == "user" && strings.Contains(last.Content, "frobnicate")
			wantWidget = last.Role == "tool" && strings.Contains(last.Content, "Loaded")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		var delta map[string]any
		finish := "stop"
		switch {
		case wantSearch:
			args, _ := json.Marshal(map[string]any{"query": "widget"})
			delta = map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "tool_search", "arguments": string(args)},
			}}}
			finish = "tool_calls"
		case wantWidget:
			args, _ := json.Marshal(map[string]any{"id": "w1"})
			delta = map[string]any{"tool_calls": []map[string]any{{
				"index": 0, "id": "call_2", "type": "function",
				"function": map[string]any{"name": "mcp__srv__widget", "arguments": string(args)},
			}}}
			finish = "tool_calls"
		default:
			delta = map[string]any{"content": fmt.Sprintf("reply %d", n)}
		}
		b, _ := json.Marshal(map[string]any{"choices": []map[string]any{{"delta": delta, "finish_reason": finish}}})
		fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", b)
	})
}

// fakeSearchMCP is the mcpToolSet seam: one deferred tool with a full schema.
type fakeSearchMCP struct {
	mu    sync.Mutex
	calls []string
}

func (f *fakeSearchMCP) Schemas(ctx context.Context) []map[string]any {
	return []map[string]any{{
		"type": "function",
		"function": map[string]any{
			"name":        "mcp__srv__widget",
			"description": "Frobnicates widgets on the frob service. Manages the full widget lifecycle with create, update and retire operations against the admin API.",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"id": map[string]any{"type": "string", "description": "The widget id to frobnicate."}},
				"required":   []string{"id"},
			},
		},
	}}
}

func (f *fakeSearchMCP) Dispatch(ctx context.Context, toolName, argsJSON string) (string, []string, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, toolName)
	return "widget frobnicated", nil, true
}

func (f *fakeSearchMCP) dispatched() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.calls...)
}

func TestToolSearchWireE2E(t *testing.T) {
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	srv := &searchWireServer{}
	ts := httptest.NewServer(srv.handler())
	defer ts.Close()

	ac := config.AgentConfig{Name: "primary", URL: ts.URL, Model: "m", ToolSearch: "auto", ContextWindowTokens: 1 << 26}
	cfg := config.Config{Agent: "primary", Agents: []config.AgentConfig{ac}}
	mcp := &fakeSearchMCP{}
	agent := local.NewFromConfig(ac).WithSkipPermissions(true).WithMCPToolSet(mcp)
	runner := newLocalRunner(agent, "primary")

	sess := &session.Session{CWD: t.TempDir()}
	var out strings.Builder
	for _, prompt := range []string{"please frobnicate the widget", "thanks"} {
		if err := runPrimary(context.Background(), cfg, sess, runner, nil, nil, prompt, &out, nil, nil, nil, nil); err != nil {
			t.Fatalf("runPrimary(%q): %v", prompt, err)
		}
	}

	srv.mu.Lock()
	tools, messages := srv.tools, srv.messages
	srv.mu.Unlock()
	if len(tools) < 3 {
		t.Fatalf("expected at least 3 requests (search → widget → reply), got %d", len(tools))
	}

	// Stub stability: the tools array is byte-identical across every request
	// of both turns — including the request right after tool_search loaded
	// the deferred schema.
	canon := func(raw json.RawMessage) string {
		var v any
		_ = json.Unmarshal(raw, &v)
		b, _ := json.Marshal(v)
		return string(b)
	}
	first := canon(tools[0])
	if !strings.Contains(first, `"name":"tool_search"`) {
		t.Errorf("wire must carry the tool_search tool, got %.300s", first)
	}
	if !strings.Contains(first, `"name":"mcp__srv__widget"`) {
		t.Errorf("wire must carry a stub for the deferred tool, got %.300s", first)
	}
	for i, raw := range tools {
		if canon(raw) != first {
			t.Fatalf("tools array changed at request %d/%d — loading must never touch the wire", i, len(tools))
		}
	}

	// The search result is delivered as tool text with the full schema.
	found := false
	for _, msgs := range messages {
		for _, m := range msgs {
			var msg struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			_ = json.Unmarshal(m, &msg)
			if msg.Role == "tool" && strings.Contains(msg.Content, "Loaded 1 tools:") && strings.Contains(msg.Content, "mcp__srv__widget") && strings.Contains(msg.Content, "schema: ") {
				found = true
			}
		}
	}
	if !found {
		t.Error("tool_search result (Loaded 1 tools: + schema) never arrived as tool text")
	}

	// The deferred tool call executed after its schema was loaded.
	if got := mcp.dispatched(); len(got) != 1 || got[0] != "mcp__srv__widget" {
		t.Errorf("deferred tool dispatches = %v, want exactly [mcp__srv__widget]", got)
	}
}
