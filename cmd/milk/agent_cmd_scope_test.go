package main

// Regression tests for the /agent family's config scope: it re-reads the
// config on every invocation and used to call config.Load() — global only —
// silently dropping any project-local .milk/config.json agents. /agent add
// saved to the local overlay (preferredSaveScope) while /agent switch re-read
// global-only, so an agent added inside a project was reported "unknown
// agent" immediately afterwards. Reads must merge against the active dir's
// overlay exactly like /reload (TestHandleReloadCmd_MergesActiveDirOverlay).

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/session"
)

// TestHandleAgentCmd_MergesActiveDirOverlay pins that /agent switch validates
// names against the merged config (global + the bound session's project
// overlay), not the global config alone.
func TestHandleAgentCmd_MergesActiveDirOverlay(t *testing.T) {
	_, launchDir := withSandboxedRoot(t)
	if err := config.Save(config.Config{Agent: "base", Agents: []config.AgentConfig{{Name: "base", URL: "http://localhost:1", Model: "m", Provider: "local"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	proj := t.TempDir()
	writeLocalOverlay(t, proj, `{"agents":[{"name":"overlay","provider":"claude-cli"}]}`)

	st := &interactiveState{cwd: launchDir, sess: &session.Session{CWD: proj}}
	m := model{st: st, transcript: &strings.Builder{}, ta: textarea.New()}
	m, _ = m.handleAgentCmd("switch overlay")

	if m.pendingSwitch == nil {
		t.Fatalf("local-only agent not visible to /agent switch — transcript:\n%s", m.transcript.String())
	}
	if m.pendingSwitch.name != "overlay" {
		t.Fatalf("pendingSwitch.name = %q, want overlay", m.pendingSwitch.name)
	}
}

// TestHandleAgentCmd_AddThenSwitchSeesAgent reproduces the reported bug
// end-to-end: /agent add inside a project with a local overlay persists the
// agent to that overlay, and a following /agent switch in the same session
// must see it — the re-read merges the overlay the add just wrote.
func TestHandleAgentCmd_AddThenSwitchSeesAgent(t *testing.T) {
	_, launchDir := withSandboxedRoot(t)
	if err := config.Save(config.Config{Agent: "base", Agents: []config.AgentConfig{{Name: "base", URL: "http://localhost:1", Model: "m", Provider: "local"}}}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	proj := t.TempDir()
	writeLocalOverlay(t, proj, `{}`) // a local overlay exists → add saves local

	st := &interactiveState{cwd: launchDir, sess: &session.Session{CWD: proj}}
	m := model{st: st, transcript: &strings.Builder{}, ta: textarea.New()}
	m, _ = m.handleAgentCmd("add name=claude provider=claude-cli")
	if !strings.Contains(m.transcript.String(), "added agent") {
		t.Fatalf("add did not commit — transcript:\n%s", m.transcript.String())
	}

	m, _ = m.handleAgentCmd("switch claude")
	if m.pendingSwitch == nil {
		t.Fatalf("agent added in this session invisible to /agent switch — transcript:\n%s", m.transcript.String())
	}
	if m.pendingSwitch.name != "claude" {
		t.Fatalf("pendingSwitch.name = %q, want claude", m.pendingSwitch.name)
	}
}
