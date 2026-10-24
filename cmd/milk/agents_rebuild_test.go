package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/router"
)

// TestHandleInitWizardKey_CommitRebuildsRunners pins the /config init wiring
// that a live end-to-end run caught missing once: when the wizard commit
// carries a config, the TUI must rebuild the runners from it — the configured
// agent has to be usable on the next turn without a restart.
func TestHandleInitWizardKey_CommitRebuildsRunners(t *testing.T) {
	// The commit really writes config.json — keep it off the developer's ~/.milk.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("MILK_CONFIG_ROOT", t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer srv.Close()

	cfg := config.Config{}
	st := &interactiveState{cfg: cfg, cwd: t.TempDir()}
	m := newModel(context.Background(), st, router.New(cfg, nil), dispatchAgents{}, nil)
	m.pendingInit, _ = initWizardStart("")

	answer := func(s string) {
		t.Helper()
		m.ta.SetValue(s)
		res, _ := m.handleInitWizardKey(tea.KeyPressMsg{Code: tea.KeyEnter})
		m = res.(model)
	}

	answer("mock")       // agent name
	answer("1")          // provider: local
	answer(srv.URL)      // server URL
	answer("default")    // run_cmd: skip
	answer("mock-model") // model
	answer("default")    // context window: skip
	answer("n")          // escalation: no — this answer commits the config

	if !m.hasInferenceAgent {
		t.Error("the wizard commit must mark an inference agent as configured")
	}
	if _, ok := m.agents.primary.(*localRunner); !ok {
		t.Fatalf("after the commit the primary runner is %T, want *localRunner", m.agents.primary)
	}
	if !m.agents.localAvail {
		t.Error("a healthy primary must read available right after the commit")
	}
	if got := m.transcript.String(); !strings.Contains(got, "agents rebuilt from config") {
		t.Errorf("the commit must announce the live rebuild; transcript:\n%s", got)
	}

	// The editor question follows the commit; answering it finishes the wizard.
	answer("n")
	if m.pendingInit != nil {
		t.Error("the wizard must be finished after the editor question")
	}
}

// newRebuildTestModel builds the minimum model rebuildRunners needs — with
// the transcript accumulators newModel sets up (appendTranscript writes to
// them on the warning paths).
func newRebuildTestModel(t *testing.T, cfg config.Config) model {
	t.Helper()
	return model{
		ctx:        context.Background(),
		st:         &interactiveState{cfg: cfg, cwd: t.TempDir()},
		transcript: &strings.Builder{},
	}
}

// TestAgentConfigChanged pins what triggers a live runner rebuild on config
// hot-reload: only changes that affect what rebuildRunners constructs.
func TestAgentConfigChanged(t *testing.T) {
	base := config.Config{
		Agents: []config.AgentConfig{{Name: "a", Provider: "local", URL: "http://x", Model: "m"}},
	}
	if agentConfigChanged(base, base) {
		t.Error("identical configs must not report an agent change")
	}

	edited := base
	edited.Agents = []config.AgentConfig{{Name: "a", Provider: "local", URL: "http://y", Model: "m"}}
	if !agentConfigChanged(base, edited) {
		t.Error("agent URL edit must report an agent change")
	}

	sel := base
	sel.EscalationAgent = "claude"
	if !agentConfigChanged(base, sel) {
		t.Error("escalation_agent edit must report an agent change")
	}

	tools := base
	tools.AgentTools = []config.AgentToolEntry{{Agent: "a"}}
	if !agentConfigChanged(base, tools) {
		t.Error("agent-as-tool edit must report an agent change")
	}

	meta := base
	meta.UpdateLastCheck = "2026-01-01T00:00:00Z"
	meta.DirectBash = true
	if agentConfigChanged(base, meta) {
		t.Error("non-agent fields must not report an agent change")
	}
}

// TestRebuildRunners_SubprocessProviders pins the live-rebuild coverage the
// old /agent switch and /agent add paths were missing: subprocess providers
// (smolagent/aider-cli) must rebuild without a restart, for both roles.
func TestRebuildRunners_SubprocessProviders(t *testing.T) {
	cfg := config.Config{
		Agent: "sm",
		Agents: []config.AgentConfig{
			{Name: "sm", Provider: "subprocess", Bin: "/bin/true", Model: "m"},
			{Name: "aid", Provider: "aider-cli", Bin: "/bin/true"},
		},
		EscalationAgent: "aid",
	}
	m := newRebuildTestModel(t, cfg)
	m, _ = m.rebuildRunners()

	if _, ok := m.agents.primary.(*subprocessRunner); !ok {
		t.Fatalf("primary runner = %T, want *subprocessRunner", m.agents.primary)
	}
	if m.agents.subprocessPrimary == nil {
		t.Error("subprocessPrimary must be set for a subprocess primary")
	}
	if m.agents.local != nil {
		t.Error("local agent must be cleared when the primary is a subprocess provider")
	}
	if _, ok := m.agents.escalation.(*subprocessRunner); !ok {
		t.Fatalf("escalation runner = %T, want *subprocessRunner", m.agents.escalation)
	}
	if m.agents.subprocessAgent == nil {
		t.Error("subprocessAgent must be set for a subprocess escalation")
	}
	if m.agents.escalationLocal != nil {
		t.Error("escalationLocal must be cleared when the escalation agent is a subprocess provider")
	}
}

// TestRebuildRunners_LocalPrimary pins the /config init first-agent case: a
// local (HTTP) primary must end up wired as the dispatch target with the
// router rebuilt against it — no restart required.
func TestRebuildRunners_LocalPrimary(t *testing.T) {
	cfg := config.Config{
		Agent: "qwen",
		Agents: []config.AgentConfig{
			{Name: "qwen", Provider: "local", URL: "://invalid", Model: "qwen2.5-coder"},
		},
	}
	m := newRebuildTestModel(t, cfg)
	m, _ = m.rebuildRunners()

	if m.agents.local == nil {
		t.Fatal("local agent must be built for a local primary")
	}
	if _, ok := m.agents.primary.(*localRunner); !ok {
		t.Fatalf("primary runner = %T, want *localRunner", m.agents.primary)
	}
	if m.agents.localAvail {
		t.Error("a primary pointing at an unreachable server must not report available")
	}
	if m.rtr == nil {
		t.Error("router must be rebuilt against the new primary")
	}
}

// TestRefreshMCPForRole_CLIPrimaryDoesNotClobberEscalation guards the role
// handling in the cliRunner case: refreshing the primary's MCP server list
// must swap m.agents.primary, never m.agents.escalation.
func TestRefreshMCPForRole_CLIPrimaryDoesNotClobberEscalation(t *testing.T) {
	ac := config.AgentConfig{Name: "claude", Provider: "claude-cli", MCPServers: []string{"srv"}}
	cfg := config.Config{
		Agent:           "claude",
		Agents:          []config.AgentConfig{ac},
		MCPServers:      []config.MCPServerConfig{{Name: "srv", URL: "http://127.0.0.1:1/mcp"}},
		EscalationAgent: "claude",
	}
	primary := newCLIRunner(newCLIAgent(ac), "claude", permContext{}, nil)
	esc := newCLIRunner(newCLIAgent(ac), "claude", permContext{}, nil)
	m := model{
		ctx: context.Background(),
		st:  &interactiveState{cfg: cfg, cwd: t.TempDir()},
		agents: dispatchAgents{
			primary:    primary,
			escalation: esc,
		},
	}

	m2, changed := m.refreshMCPForRole(RolePrimary, "claude")
	if !changed {
		t.Fatal("refresh must report a change when servers were never seen")
	}
	if m2.agents.escalation != TurnRunner(esc) {
		t.Error("primary refresh must not touch the escalation runner")
	}
	if m2.agents.primary == TurnRunner(primary) {
		t.Error("primary runner must be swapped for one carrying the server list")
	}
}

// TestRefreshTurnAvailability_SelfHeals pins the per-turn availability
// re-check: an agent that read unavailable when the runners were built (server
// still booting, config reloaded while it was down) must be picked up as soon
// as it answers, instead of freezing the escalation fallback until a restart.
func TestRefreshTurnAvailability_SelfHeals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "a", Provider: "local", URL: srv.URL, Model: "m"}}}
	agent := local.NewFromConfig(cfg.Agents[0])
	agents := &dispatchAgents{
		primary: newLocalRunner(agent, "a"),
		local:   agent,
		// Built while the server was down — the frozen state being tested.
		localAvail: false,
	}
	rtr := router.New(cfg, nil)

	localAvail, _ := refreshTurnAvailability(context.Background(), rtr, agents)
	srv.Close()
	if !localAvail {
		t.Error("a healthy primary must self-heal to available")
	}
	if !agents.localAvail {
		t.Error("the recovered flag must stick on the agent set")
	}
}

// TestRefreshTurnAvailability_DownAgentStaysDownBounded pins the other side:
// a genuinely unreachable agent keeps falling back — and the re-check is
// bounded so it can never stall the turn.
func TestRefreshTurnAvailability_DownAgentStaysDownBounded(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // now refuses connections

	agent := local.NewFromConfig(config.AgentConfig{Name: "d", URL: url, Model: "m"})
	agents := &dispatchAgents{primary: newLocalRunner(agent, "d"), localAvail: false}

	start := time.Now()
	localAvail, _ := refreshTurnAvailability(context.Background(), router.New(config.Config{}, nil), agents)
	if localAvail {
		t.Error("an unreachable primary must stay unavailable")
	}
	if agents.localAvail {
		t.Error("the agent set must not record an unreachable agent as available")
	}
	if elapsed := time.Since(start); elapsed > availPingTimeout+2*time.Second {
		t.Errorf("re-check took %v — it must be bounded by availPingTimeout", elapsed)
	}
}

// TestRebuildRunners_StartsRunCmdServer pins the /config init regression: a
// local provider committed with a run_cmd must have its server started by the
// live rebuild (as startup does), so the configured primary is available
// immediately instead of falling back to escalation until a restart.
func TestRebuildRunners_StartsRunCmdServer(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available to serve as a test inference server")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserving a port: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()

	// Keep PID/log files out of the real home directory.
	t.Setenv("HOME", t.TempDir())
	t.Cleanup(func() { _, _ = serverStop("qwen") })

	cfg := config.Config{
		Agent: "qwen",
		Agents: []config.AgentConfig{{
			Name:     "qwen",
			Provider: "local",
			URL:      fmt.Sprintf("http://127.0.0.1:%d", port),
			Model:    "m",
			// exec replaces the sh -c wrapper so serverStop's PID is the
			// server itself.
			RunCmd: fmt.Sprintf("exec python3 -m http.server %d --bind 127.0.0.1", port),
		}},
	}
	m := newRebuildTestModel(t, cfg)
	m, _ = m.rebuildRunners()

	if !m.agents.localAvail {
		t.Fatal("rebuild must start run_cmd and see the primary as available")
	}
	if _, ok := m.agents.primary.(*localRunner); !ok {
		t.Fatalf("primary runner = %T, want *localRunner", m.agents.primary)
	}
}
