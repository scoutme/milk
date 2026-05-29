package config

import (
	"testing"
)

func TestActiveAgent_ByName(t *testing.T) {
	cfg := Config{
		Agent: "second",
		Agents: []AgentConfig{
			{Name: "first", URL: "http://first", Model: "m1"},
			{Name: "second", URL: "http://second", Model: "m2", Provider: "bedrock"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "second" || got.URL != "http://second" || got.Provider != "bedrock" {
		t.Fatalf("expected second/bedrock, got %+v", got)
	}
}

func TestActiveAgent_FirstNonCLI(t *testing.T) {
	cfg := Config{
		Agents: []AgentConfig{
			{Name: "claude", Provider: "claude-cli"},
			{Name: "alpha", URL: "http://alpha", Model: "ma"},
			{Name: "beta", URL: "http://beta", Model: "mb"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "alpha" {
		t.Fatalf("expected alpha (first non-cli), got %q", got.Name)
	}
}

func TestActiveAgent_CaseInsensitive(t *testing.T) {
	cfg := Config{
		Agent: "HAIKU",
		Agents: []AgentConfig{
			{Name: "haiku", URL: "http://haiku", Model: "h"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "haiku" {
		t.Fatalf("expected haiku, got %q", got.Name)
	}
}

func TestActiveAgent_UnknownNameFallsToFirst(t *testing.T) {
	cfg := Config{
		Agent: "nonexistent",
		Agents: []AgentConfig{
			{Name: "only", URL: "http://only", Model: "m"},
		},
	}
	got := cfg.ActiveAgent()
	if got.Name != "only" {
		t.Fatalf("expected only, got %q", got.Name)
	}
}

func TestActiveAgent_EmptyConfigReturnsZero(t *testing.T) {
	cfg := Config{}
	got := cfg.ActiveAgent()
	if got.URL != "" || got.Model != "" {
		t.Fatalf("empty config should return zero AgentConfig, got %+v", got)
	}
}

func TestEscalationAgentConfig_DefaultClaude(t *testing.T) {
	cfg := Config{}
	got := cfg.EscalationAgentConfig()
	if !got.IsCLI() || got.Name != "claude" {
		t.Fatalf("expected default claude-cli, got %+v", got)
	}
}

func TestEscalationAgentConfig_NamedLocal(t *testing.T) {
	cfg := Config{
		EscalationAgent: "haiku-aws",
		Agents: []AgentConfig{
			{Name: "haiku-aws", URL: "http://bedrock", Model: "arn:x", Provider: "bedrock"},
		},
	}
	got := cfg.EscalationAgentConfig()
	if got.Provider != "bedrock" || got.Name != "haiku-aws" {
		t.Fatalf("expected haiku-aws/bedrock, got %+v", got)
	}
}
