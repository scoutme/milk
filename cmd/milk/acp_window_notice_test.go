package main

import (
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/testutil"
	"github.com/scoutme/milk/internal/transport/acp"
)

// newWindowNoticeServer builds an ACP server whose config has an agent with
// no context_window_tokens and a model outside the models.dev catalog — the
// "budget is silently the built-in fallback" case the session-start advisory
// exists for. cfgJSON (a printf template taking the stub URL) is written
// verbatim into ~/.milk/config.json.
func newWindowNoticeServer(t *testing.T, cfgJSON string) (*acpServer, *fakeACPConn) {
	t.Helper()
	var bodies []string
	var mu sync.Mutex
	srv := httptest.NewServer(stubRecordingBackend(&bodies, &mu))
	t.Cleanup(srv.Close)

	home := testutil.SandboxHome(t)
	milkDir := filepath.Join(home, ".milk")
	if err := os.MkdirAll(milkDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(milkDir, "config.json"), []byte(fmt.Sprintf(cfgJSON, srv.URL)), 0o600); err != nil {
		t.Fatalf("WriteFile config: %v", err)
	}
	t.Setenv("HOME", home)

	cfg, err := config.LoadMerged()
	if err != nil {
		t.Fatalf("LoadMerged: %v", err)
	}
	conn := &fakeACPConn{}
	return newACPServer(cfg, conn), conn
}

// windowNoticeWith returns the first notice chunk mentioning the unknown
// context window, or "" when the session stayed silent.
func windowNoticeWith(t *testing.T, conn *fakeACPConn) string {
	t.Helper()
	for _, u := range sentUpdates(t, conn) {
		if c, ok := u.(acp.ContentChunk); ok &&
			strings.Contains(c.Content.Text, "no known context window") {
			return c.Content.Text
		}
	}
	return ""
}

// TestACPSession_WindowBudgetNotice pins the ACP session-start advisory:
// session/new for a milk-loop agent with no known context window and no
// forced tool-iteration budget must tell the client which config keys define
// the budget — and a configured window must keep the session silent.
func TestACPSession_WindowBudgetNotice(t *testing.T) {
	t.Run("unknown window → notice chunk with both config keys", func(t *testing.T) {
		cfgJSON := `{"agent":"test-local","agents":[{"name":"test-local","url":"%s","model":"test-model","provider":"local"}]}`
		server, conn := newWindowNoticeServer(t, cfgJSON)
		result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: t.TempDir()})
		server.AfterResponse("session/new", result) // the transport's post-response step (stdio.go)

		got := windowNoticeWith(t, conn)
		if got == "" {
			t.Fatal("expected a session-start notice about the unknown context window")
		}
		for _, want := range []string{"context_window_tokens", "limits.max_tool_iterations", "falls back to 40"} {
			if !strings.Contains(got, want) {
				t.Errorf("notice missing %q: %q", want, got)
			}
		}
	})

	t.Run("window defined → no notice", func(t *testing.T) {
		cfgJSON := `{"agent":"test-local","agents":[{"name":"test-local","url":"%s","model":"test-model","provider":"local","context_window_tokens":32768}]}`
		server, conn := newWindowNoticeServer(t, cfgJSON)
		result := acpRequest(t, server, "session/new", acp.NewSessionRequest{CWD: t.TempDir()})
		server.AfterResponse("session/new", result)

		if got := windowNoticeWith(t, conn); got != "" {
			t.Errorf("unexpected notice for a configured window: %q", got)
		}
	})
}
