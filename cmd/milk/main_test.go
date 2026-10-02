package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/mcp"
	"github.com/scoutme/milk/internal/mcpauth"
)

// TestMergeConnectResults_CollapsesByServerName pins the #161 merge
// semantics: the same server name can appear once per agent toolset it
// backs, but the user only needs one line per server, in first-seen order,
// and a name counts as connected when *any* of its instances connected.
func TestMergeConnectResults_CollapsesByServerName(t *testing.T) {
	errA := errors.New("boom-a")
	in := []mcpConnectResult{
		{server: "a", err: errA},
		{server: "b", err: nil},
		{server: "a", err: nil}, // a's other instance connected: must clear the earlier error
		{server: "c", err: errors.New("boom-c")},
	}
	got := mergeConnectResults(in)
	want := []mcpConnectResult{
		{server: "a", err: nil},
		{server: "b", err: nil},
		{server: "c", err: errors.New("boom-c")},
	}
	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].server != want[i].server {
			t.Errorf("[%d].server = %q, want %q", i, got[i].server, want[i].server)
		}
		gotErr, wantErr := got[i].err, want[i].err
		if (gotErr == nil) != (wantErr == nil) {
			t.Errorf("[%d].err = %v, want %v", i, gotErr, wantErr)
		}
	}
}

// newFakeMCPServer implements just enough of the Streamable HTTP JSON-RPC
// transport (initialize, notifications/initialized) for Client.Connect to
// succeed — mirrors internal/mcp's own toolset_test.go fixture, duplicated
// here since that one is unexported in a different package.
func newFakeMCPServer(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusOK)
			return
		}
		var req struct {
			ID     *int64 `json:"id"`
			Method string `json:"method"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		switch req.Method {
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "initialize":
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Mcp-Session-Id", "sess-1")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"protocolVersion":"2025-03-26"}}`, *req.ID)
		case "tools/list":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[]}}`, *req.ID)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

// TestConnectToolSets_ConnectsInParallelAndMergesAcrossToolSets pins the
// #161 deferred startup connect: every client across every toolset connects
// concurrently (bounded by its own timeout, not serialized), an unreachable
// server never blocks a reachable sibling, and a server backing two agents'
// toolsets (same name, two client instances) collapses to one result.
func TestConnectToolSets_ConnectsInParallelAndMergesAcrossToolSets(t *testing.T) {
	good := newFakeMCPServer(t)
	defer good.Close()

	shared := func() *mcp.Client {
		return mcp.New(config.MCPServerConfig{Name: "shared", URL: good.URL})
	}
	tsPrimary := mcp.NewToolSet([]*mcp.Client{
		shared(),
		mcp.New(config.MCPServerConfig{Name: "unreachable", URL: "http://127.0.0.1:1"}),
	})
	tsEscalation := mcp.NewToolSet([]*mcp.Client{shared()})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	start := time.Now()
	results := connectToolSets(ctx, []*mcp.ToolSet{tsPrimary, tsEscalation})
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("connectToolSets took %s — clients did not connect in parallel", elapsed)
	}

	byName := map[string]mcpConnectResult{}
	for _, r := range results {
		byName[r.server] = r
	}
	if len(byName) != 2 {
		t.Fatalf("results = %+v, want exactly 2 distinct servers", results)
	}
	if r, ok := byName["shared"]; !ok || r.err != nil {
		t.Errorf("shared = %+v, want connected (both instances point at the same working server)", r)
	}
	if r, ok := byName["unreachable"]; !ok || r.err == nil {
		t.Errorf("unreachable = %+v, want a connect error", r)
	}
}

// TestUpdate_MCPConnectReadyMsg_NotifiesPerOutcome pins the #161 toast
// contract: the deferred startup connect's merged per-server outcomes
// render as toasts (never the transcript, per ADR-0048 — turn-unrelated
// startup noise), classified into auth-required vs. plain-unavailable, plus
// a summary line reflecting partial vs. full connectivity.
func TestUpdate_MCPConnectReadyMsg_NotifiesPerOutcome(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	updated, _ := m.Update(mcpConnectReadyMsg{results: []mcpConnectResult{
		{server: "cloudflare", err: nil},
	}})
	got := updated.(model)
	if !toastMentions(got, "MCP: 1 server(s) connected") {
		t.Errorf("expected an all-connected toast, got %#v", got.toastVisible)
	}

	m2 := layoutTestModel(t, 120, 40)
	updated2, _ := m2.Update(mcpConnectReadyMsg{results: []mcpConnectResult{
		{server: "ukb-coll", err: fmt.Errorf("mcp %q: %w", "ukb-coll", mcpauth.ErrNoRefreshToken)},
	}})
	got2 := updated2.(model)
	if !toastMentions(got2, `MCP "ukb-coll": authorization required`) {
		t.Errorf("expected an auth-required toast, got %#v", got2.toastVisible)
	}

	m3 := layoutTestModel(t, 120, 40)
	updated3, _ := m3.Update(mcpConnectReadyMsg{results: []mcpConnectResult{
		{server: "blender", err: errors.New("dial tcp: connection refused")},
	}})
	got3 := updated3.(model)
	if !toastMentions(got3, `MCP "blender": unavailable, will retry on first use`) {
		t.Errorf("expected an unavailable toast, got %#v", got3.toastVisible)
	}

	m4 := layoutTestModel(t, 120, 40)
	updated4, _ := m4.Update(mcpConnectReadyMsg{results: []mcpConnectResult{
		{server: "cloudflare", err: nil},
		{server: "blender", err: errors.New("boom")},
	}})
	got4 := updated4.(model)
	if !toastMentions(got4, "MCP: 1 of 2 servers connected") {
		t.Errorf("expected a partial-connectivity toast, got %#v", got4.toastVisible)
	}
	if strings.Contains(got4.transcript.String(), "MCP") {
		t.Errorf("startup MCP connect results must never enter the transcript (ADR-0048), got %q", got4.transcript.String())
	}
}
