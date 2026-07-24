package main

import (
	"context"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// TestBuildToolRunner_CLIRequiresDangerouslySkipPermissions verifies that
// buildToolRunner returns an error for a claude-cli agent without the flag.
func TestBuildToolRunner_CLIRequiresDangerouslySkipPermissions(t *testing.T) {
	ac := config.AgentConfig{
		Name:     "claude-tool",
		Provider: "claude-cli",
		Bin:      "claude",
		// DangerouslySkipPermissions intentionally left false
	}
	_, err := buildToolRunner(context.Background(), ac, config.Config{})
	if err == nil {
		t.Fatal("expected an error when DangerouslySkipPermissions is false, got nil")
	}
	errMsg := err.Error()
	for _, want := range []string{"dangerously_skip_permissions", "claude-tool"} {
		if !containsStr(errMsg, want) {
			t.Errorf("error message %q does not contain %q", errMsg, want)
		}
	}
}

// TestBuildToolRunner_CLIWithSkipPermissions verifies that buildToolRunner
// returns a non-nil runner and no error for a claude-cli agent with the flag set.
func TestBuildToolRunner_CLIWithSkipPermissions(t *testing.T) {
	ac := config.AgentConfig{
		Name:                       "claude-tool",
		Provider:                   "claude-cli",
		Bin:                        "claude",
		DangerouslySkipPermissions: true,
	}
	runner, err := buildToolRunner(context.Background(), ac, config.Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner == nil {
		t.Fatal("expected non-nil runner")
	}
	if runner.Name() != "claude-tool" {
		t.Errorf("runner.Name() = %q, want %q", runner.Name(), "claude-tool")
	}
	// Verify it is a cliRunner (not a localRunner or subprocessRunner).
	if _, ok := runner.(*cliRunner); !ok {
		t.Errorf("expected *cliRunner, got %T", runner)
	}
}

// TestBuildToolRunner_CLIDefaultName ensures the runner gets a default name when
// the agent config Name is empty.
func TestBuildToolRunner_CLIDefaultName(t *testing.T) {
	ac := config.AgentConfig{
		Provider:                   "claude-cli",
		Bin:                        "claude",
		DangerouslySkipPermissions: true,
	}
	runner, err := buildToolRunner(context.Background(), ac, config.Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner.Name() != "tool-agent" {
		t.Errorf("runner.Name() = %q, want %q", runner.Name(), "tool-agent")
	}
}

// TestBuildToolRunner_LocalAgent verifies local HTTP agents still work (regression).
func TestBuildToolRunner_LocalAgent(t *testing.T) {
	ac := config.AgentConfig{
		Name:     "local-tool",
		Provider: "local",
		URL:      "http://localhost:19999",
		Model:    "mock-model",
	}
	runner, err := buildToolRunner(context.Background(), ac, config.Config{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if runner == nil {
		t.Fatal("expected non-nil runner")
	}
	if _, ok := runner.(*localRunner); !ok {
		t.Errorf("expected *localRunner, got %T", runner)
	}
}

// containsStr is a helper to avoid importing strings in tests.
func containsStr(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || findSubstr(s, sub))
}

func findSubstr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
