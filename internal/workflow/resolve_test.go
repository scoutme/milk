package workflow_test

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/workflow"
)

func makeTestConfig(primaryName, escalationName string, extraAgents ...string) config.Config {
	agents := []config.AgentConfig{
		{Name: primaryName, URL: "http://localhost:11434"},
	}
	for _, name := range extraAgents {
		agents = append(agents, config.AgentConfig{Name: name, URL: "http://localhost:11435"})
	}
	return config.Config{
		Agent:           primaryName,
		EscalationAgent: escalationName,
		Agents:          agents,
	}
}

func TestResolveAgentNames_Aliases(t *testing.T) {
	cfg := makeTestConfig("local-model", "claude")
	roles := map[string]string{
		"designer":  "primary",
		"generator": "escalation",
		"evaluator": "escalation",
	}
	got, err := workflow.ResolveAgentNames(roles, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["designer"] != "local-model" {
		t.Errorf("designer: got %q, want %q", got["designer"], "local-model")
	}
	if got["generator"] != "claude" {
		t.Errorf("generator: got %q, want %q", got["generator"], "claude")
	}
	if got["evaluator"] != "claude" {
		t.Errorf("evaluator: got %q, want %q", got["evaluator"], "claude")
	}
}

func TestResolveAgentNames_ConcreteName(t *testing.T) {
	cfg := makeTestConfig("local-model", "claude", "bedrock-agent")
	roles := map[string]string{
		"designer": "bedrock-agent",
	}
	got, err := workflow.ResolveAgentNames(roles, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["designer"] != "bedrock-agent" {
		t.Errorf("designer: got %q, want %q", got["designer"], "bedrock-agent")
	}
}

func TestResolveAgentNames_UnknownName(t *testing.T) {
	cfg := makeTestConfig("local-model", "claude")
	roles := map[string]string{
		"designer": "nonexistent-agent",
	}
	_, err := workflow.ResolveAgentNames(roles, cfg)
	if err == nil {
		t.Error("expected error for unknown agent name, got nil")
	}
}

func TestResolveAgentNames_BuiltInClaude(t *testing.T) {
	cfg := makeTestConfig("local-model", "claude")
	roles := map[string]string{
		"generator": "claude",
	}
	got, err := workflow.ResolveAgentNames(roles, cfg)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["generator"] != "claude" {
		t.Errorf("generator: got %q, want %q", got["generator"], "claude")
	}
}
