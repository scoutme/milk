package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// With several claude-cli entries, escalation_agent must pick which one runs —
// not whichever claude-cli entry happens to come first in agents.
func TestCLIAgentConfig_HonoursEscalationAgent(t *testing.T) {
	agents := []config.AgentConfig{
		{Name: "local", URL: "http://localhost:8080"},
		{Name: "claude-alt", Provider: "claude-cli", Bin: "claude-alt"},
		{Name: "claude-work", Provider: "claude-cli", Bin: "claude-work"},
	}
	cases := []struct {
		escalation, want string
	}{
		{"claude-work", "claude-work"},
		{"claude", "claude"},    // built-in entry, no agent named "claude" in the list
		{"", "claude-alt"},      // unset: first claude-cli entry, as before
		{"local", "claude-alt"}, // non-CLI escalation target: first claude-cli entry
	}
	for _, c := range cases {
		got := cliAgentConfig(config.Config{Agents: agents, EscalationAgent: c.escalation})
		if got.Name != c.want {
			t.Errorf("escalation_agent=%q: got %q, want %q", c.escalation, got.Name, c.want)
		}
	}
}
