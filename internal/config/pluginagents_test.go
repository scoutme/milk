package config

import (
	"strings"
	"testing"
)

// withPluginAgents installs a plugin agent set for the test and clears it on
// completion — the seam is process-wide, so every test owns its reset.
func withPluginAgents(t *testing.T, set PluginAgentSet) {
	t.Helper()
	SetPluginAgents(func() PluginAgentSet { return set })
	t.Cleanup(func() { SetPluginAgents(nil) })
}

func pluginTestSet() PluginAgentSet {
	return PluginAgentSet{
		Agents: []AgentConfig{
			{Name: "code-reviewer", URL: "http://plugin.example/v1", Model: "m", Prompt: "review body"},
			{Name: "shadow", URL: "http://shadow.example/v1"},
		},
		Tools: []AgentToolEntry{
			{Agent: "code-reviewer", Description: "Expert code review"},
			{Agent: "shadow", Description: "collides with config"},
		},
	}
}

func TestPluginAgents_JoinResolution(t *testing.T) {
	withPluginAgents(t, pluginTestSet())
	cfg := Config{Agents: []AgentConfig{ // "shadow" collides with the plugin set
		{Name: "prim", URL: "http://p"},
		{Name: "shadow", URL: "http://config-shadow"},
	}}

	all := cfg.AllAgents()
	names := make([]string, 0, len(all))
	for _, a := range all {
		names = append(names, a.Name)
	}
	joined := strings.Join(names, ",")
	if !strings.Contains(joined, "code-reviewer") {
		t.Fatalf("AllAgents = %v want code-reviewer", names)
	}
	// collision: user config wins, plugin copy dropped everywhere
	for _, a := range all {
		if a.Name == "shadow" && a.URL == "http://shadow.example/v1" {
			t.Errorf("plugin shadow entry survived a config collision: %+v", a)
		}
		if a.Name == "shadow" && a.URL != "http://config-shadow" {
			t.Errorf("shadow resolved to %+v want the config entry", a.URL)
		}
	}
	// implicit claude default still present exactly once
	claudeCount := 0
	for _, a := range all {
		if strings.EqualFold(a.Name, "claude") {
			claudeCount++
		}
	}
	if claudeCount != 1 {
		t.Errorf("claude entries = %d want 1", claudeCount)
	}

	cfgd := cfg.ConfiguredAgents()
	if len(cfgd) != 3 { // prim + shadow(config) + code-reviewer — no claude default, no plugin shadow
		t.Fatalf("ConfiguredAgents = %d (%v) want 3", len(cfgd), cfgd)
	}
}

func TestPluginAgents_ActiveAgentFallbackNeverPicksPlugin(t *testing.T) {
	withPluginAgents(t, PluginAgentSet{
		Agents: []AgentConfig{{Name: "code-reviewer", URL: "http://plugin.example/v1"}},
	})
	// No user agents: the plugin must NOT become primary by fallback.
	cfg := Config{}
	if got := cfg.ActiveAgent(); got.Name != "" {
		t.Errorf("ActiveAgent = %+v want zero value (plugin skipped, claude is external)", got)
	}
	// With a real agent configured, that agent wins regardless of order.
	cfg = Config{Agents: []AgentConfig{{Name: "prim", URL: "http://p"}}}
	if got := cfg.ActiveAgent(); got.Name != "prim" {
		t.Errorf("ActiveAgent = %q want prim", got.Name)
	}
	// An explicit cfg.Agent name still honours a plugin agent.
	cfg.Agent = "code-reviewer"
	if got := cfg.ActiveAgent(); got.Name != "code-reviewer" || got.URL != "http://plugin.example/v1" {
		t.Errorf("ActiveAgent = %+v want the named plugin entry", got)
	}
}

func TestPluginAgents_ToolExposureSeeded(t *testing.T) {
	withPluginAgents(t, pluginTestSet())
	cfg := Config{
		Agents:     []AgentConfig{{Name: "prim"}},
		AgentTools: []AgentToolEntry{{Agent: "shadow", Description: "global wins over plugin"}},
	}
	entries := cfg.EffectiveToolAgents("prim")
	byAgent := map[string]string{}
	for _, e := range entries {
		if _, dup := byAgent[strings.ToLower(e.Agent)]; dup {
			t.Fatalf("duplicate exposure entry for %q: %v", e.Agent, entries)
		}
		byAgent[strings.ToLower(e.Agent)] = e.Description
	}
	if byAgent["code-reviewer"] != "Expert code review" {
		t.Errorf("code-reviewer exposure = %q", byAgent["code-reviewer"])
	}
	// global entry with the same name wins (config authoritative)
	if byAgent["shadow"] != "global wins over plugin" {
		t.Errorf("shadow exposure = %q want the global entry", byAgent["shadow"])
	}
	// caller cycle guard still applies to plugin peers
	self := cfg.EffectiveToolAgents("code-reviewer")
	for _, e := range self {
		if e.Agent == "code-reviewer" {
			t.Errorf("caller got itself as a peer: %v", self)
		}
	}
	// caller-level override replaces the seeded plugin entry (no dup)
	cfg2 := Config{
		Agents: []AgentConfig{
			{Name: "prim", Tools: []AgentToolEntry{{Agent: "code-reviewer", Description: "overridden by caller"}}},
		},
	}
	withPluginAgents(t, pluginTestSet())
	got := cfg2.EffectiveToolAgents("prim")
	count := 0
	for _, e := range got {
		if e.Agent == "code-reviewer" {
			count++
			if e.Description != "overridden by caller" {
				t.Errorf("description = %q want caller override", e.Description)
			}
		}
	}
	if count != 1 {
		t.Errorf("code-reviewer entries = %d want 1", count)
	}
}

func TestPluginAgents_IsPluginAgent(t *testing.T) {
	if IsPluginAgent("anything") {
		t.Error("no seam installed: must be false")
	}
	withPluginAgents(t, pluginTestSet())
	if !IsPluginAgent("code-reviewer") || !IsPluginAgent("CODE-REVIEWER") {
		t.Error("want true (case-insensitive)")
	}
	if IsPluginAgent("prim") {
		t.Error("want false for config agents")
	}
}
