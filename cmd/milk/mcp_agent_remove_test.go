package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestRemoveMCPServer_RemovesAndCleansAgentRefs(t *testing.T) {
	cfg := config.Config{
		MCPServers: []config.MCPServerConfig{{Name: "GitHub"}, {Name: "other"}},
		Agents: []config.AgentConfig{
			{Name: "n", MCPServers: []string{"github", "other"}},
		},
	}

	if !removeMCPServer(&cfg, "github") {
		t.Fatal("expected removeMCPServer to report found")
	}
	if len(cfg.MCPServers) != 1 || cfg.MCPServers[0].Name != "other" {
		t.Errorf("MCPServers = %+v, want only \"other\" left", cfg.MCPServers)
	}
	if got := cfg.Agents[0].MCPServers; len(got) != 1 || got[0] != "other" {
		t.Errorf("agent MCPServers = %v, want [\"other\"] (github reference cleaned up)", got)
	}
}

func TestRemoveMCPServer_NotFound(t *testing.T) {
	cfg := config.Config{MCPServers: []config.MCPServerConfig{{Name: "x"}}}
	if removeMCPServer(&cfg, "nonexistent") {
		t.Error("expected removeMCPServer to report not found")
	}
	if len(cfg.MCPServers) != 1 {
		t.Errorf("MCPServers should be unchanged on a miss, got %+v", cfg.MCPServers)
	}
}

func TestRemoveAgentConfig_RefusesActivePrimary(t *testing.T) {
	cfg := config.Config{
		Agent:  "n",
		Agents: []config.AgentConfig{{Name: "n"}, {Name: "claude", Provider: "claude-cli"}},
	}
	outcome, _ := removeAgentConfig(&cfg, "n")
	if outcome != agentRemoveIsPrimary {
		t.Errorf("outcome = %v, want agentRemoveIsPrimary", outcome)
	}
	if len(cfg.Agents) != 2 {
		t.Error("cfg.Agents should be unchanged when removal is refused")
	}
}

func TestRemoveAgentConfig_RefusesActiveEscalation(t *testing.T) {
	cfg := config.Config{
		Agent:           "n",
		EscalationAgent: "claude",
		Agents:          []config.AgentConfig{{Name: "n"}, {Name: "claude", Provider: "claude-cli"}},
	}
	outcome, _ := removeAgentConfig(&cfg, "claude")
	if outcome != agentRemoveIsEscalation {
		t.Errorf("outcome = %v, want agentRemoveIsEscalation", outcome)
	}
}

func TestRemoveAgentConfig_RemovesInactiveAgent(t *testing.T) {
	cfg := config.Config{
		Agent:  "n",
		Agents: []config.AgentConfig{{Name: "n"}, {Name: "extra"}, {Name: "claude", Provider: "claude-cli"}},
	}
	outcome, removed := removeAgentConfig(&cfg, "extra")
	if outcome != agentRemoveOK {
		t.Fatalf("outcome = %v, want agentRemoveOK", outcome)
	}
	if removed != "extra" {
		t.Errorf("removed = %q, want %q", removed, "extra")
	}
	if len(cfg.Agents) != 2 {
		t.Errorf("Agents = %+v, want 2 remaining", cfg.Agents)
	}
}

func TestRemoveAgentConfig_NotFound(t *testing.T) {
	cfg := config.Config{Agent: "n", Agents: []config.AgentConfig{{Name: "n"}}}
	outcome, _ := removeAgentConfig(&cfg, "nonexistent")
	if outcome != agentRemoveNotFound {
		t.Errorf("outcome = %v, want agentRemoveNotFound", outcome)
	}
}
