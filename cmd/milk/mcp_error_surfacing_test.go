package main

import (
	"context"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// TestRefreshMCPForRole_ErrorSurfacedToTranscript verifies that when
// buildMCPToolSet fails during a mid-session MCP reconnect, the error appears
// in the TUI transcript (not leaked to stderr above the TUI).
func TestRefreshMCPForRole_ErrorSurfacedToTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	cfg := config.Config{
		Agents: []config.AgentConfig{
			{Name: "test-primary", Provider: "local", MCPServers: []string{"broken-mcp"}},
		},
		MCPServers: []config.MCPServerConfig{
			{Name: "broken-mcp", URL: "http://127.0.0.1:1"},
		},
	}
	st.cfg = cfg

	// Build a local agent and wrap it in a localRunner so refreshMCPForRole
	// enters the *localRunner case.
	la := local.NewFromConfig(cfg.Agents[0])
	runner := newLocalRunner(la, "test-primary")
	m := newModel(context.Background(), st, nil, dispatchAgents{
		primary: runner,
		local:   la,
	}, nil)

	m, changed := m.refreshMCPForRole(RolePrimary, "test-primary")
	if !changed {
		t.Fatal("expected refreshMCPForRole to return changed=true")
	}

	transcript := m.transcript.String()
	if !strings.Contains(transcript, "MCP connect error") {
		t.Errorf("expected transcript to contain MCP connect error, got %q", transcript)
	}
	if !strings.Contains(transcript, "test-primary") {
		t.Errorf("expected transcript to mention the agent name, got %q", transcript)
	}
}
