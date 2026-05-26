package config

import (
	"testing"
)

func TestActiveLocalAgent_ListByName(t *testing.T) {
	cfg := Config{
		LocalAgent: "second",
		LocalAgents: []LocalAgentConfig{
			{Name: "first", URL: "http://first", Model: "m1"},
			{Name: "second", URL: "http://second", Model: "m2", Provider: "bedrock"},
		},
	}
	got := cfg.ActiveLocalAgent()
	if got.Name != "second" || got.URL != "http://second" || got.Provider != "bedrock" {
		t.Fatalf("expected second/bedrock, got %+v", got)
	}
}

func TestActiveLocalAgent_ListFirstWhenNameEmpty(t *testing.T) {
	cfg := Config{
		LocalAgents: []LocalAgentConfig{
			{Name: "alpha", URL: "http://alpha", Model: "ma"},
			{Name: "beta", URL: "http://beta", Model: "mb"},
		},
	}
	got := cfg.ActiveLocalAgent()
	if got.Name != "alpha" {
		t.Fatalf("expected alpha, got %q", got.Name)
	}
}

func TestActiveLocalAgent_ListCaseInsensitive(t *testing.T) {
	cfg := Config{
		LocalAgent: "HAIKU",
		LocalAgents: []LocalAgentConfig{
			{Name: "haiku", URL: "http://haiku", Model: "h"},
		},
	}
	got := cfg.ActiveLocalAgent()
	if got.Name != "haiku" {
		t.Fatalf("expected haiku, got %q", got.Name)
	}
}

func TestActiveLocalAgent_ListUnknownNameFallsToFirst(t *testing.T) {
	cfg := Config{
		LocalAgent: "nonexistent",
		LocalAgents: []LocalAgentConfig{
			{Name: "only", URL: "http://only", Model: "m"},
		},
	}
	got := cfg.ActiveLocalAgent()
	if got.Name != "only" {
		t.Fatalf("expected only, got %q", got.Name)
	}
}

func TestActiveLocalAgent_EmptyConfigReturnsZero(t *testing.T) {
	// Empty config means no provider configured — returns zero LocalAgentConfig.
	cfg := Config{}
	got := cfg.ActiveLocalAgent()
	if got.URL != "" || got.Model != "" {
		t.Fatalf("empty config should return zero LocalAgentConfig, got %+v", got)
	}
}
