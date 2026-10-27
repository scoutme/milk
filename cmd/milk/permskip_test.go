package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestSkipPermissionsFor(t *testing.T) {
	cfg := config.Config{
		Agent: "primary",
		Agents: []config.AgentConfig{
			{Name: "primary", Provider: "bearer", Model: "m"},
			{Name: "claude", Provider: "claude-cli", Bin: "claude"},
		},
	}

	// Neither flag set → no skip.
	if skipPermissionsFor(cfg, cfg.ActiveAgent()) {
		t.Error("must not skip when no flag is set")
	}

	// The agent's own per-agent flag (documented meaning) now takes effect.
	cfg.Agents[0].DangerouslySkipPermissions = true
	if !skipPermissionsFor(cfg, cfg.ActiveAgent()) {
		t.Error("agent's own dangerously_skip_permissions must take effect")
	}

	// The historical cross-wiring (flag on the claude-cli entry, acting as
	// the session-wide switch) keeps working.
	cfg.Agents[0].DangerouslySkipPermissions = false
	cfg.Agents[1].DangerouslySkipPermissions = true
	if !skipPermissionsFor(cfg, cfg.ActiveAgent()) {
		t.Error("claude-cli entry's flag must keep working as the global switch")
	}
}
