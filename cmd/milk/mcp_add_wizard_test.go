package main

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// TestFindMCPServer_CaseInsensitive verifies name lookup ignores case.
func TestFindMCPServer_CaseInsensitive(t *testing.T) {
	servers := []config.MCPServerConfig{{Name: "GitHub", URL: "https://x"}}

	got, ok := findMCPServer(servers, "github")
	if !ok {
		t.Fatal("expected match for case-insensitive name")
	}
	if got.URL != "https://x" {
		t.Errorf("URL = %q, want %q", got.URL, "https://x")
	}

	if _, ok := findMCPServer(servers, "gitlab"); ok {
		t.Error("expected no match for unrelated name")
	}
}

// TestMcpMergeExisting_KeepsExistingWhenNoOverride verifies that fields not
// supplied inline fall back to the existing server's current values.
func TestMcpMergeExisting_KeepsExistingWhenNoOverride(t *testing.T) {
	existing := config.MCPServerConfig{
		Name: "GitHub", URL: "https://old", Auth: "bearer", APIKey: "secret",
	}
	overrides := config.MCPServerConfig{Name: "github"} // only the (lowercased) name typed

	merged := mcpMergeExisting(existing, overrides)

	if merged.Name != "GitHub" {
		t.Errorf("Name = %q, want original casing %q", merged.Name, "GitHub")
	}
	if merged.URL != "https://old" {
		t.Errorf("URL = %q, want existing value preserved", merged.URL)
	}
	if merged.Auth != "bearer" || merged.APIKey != "secret" {
		t.Errorf("Auth/APIKey should be preserved from existing, got %+v", merged)
	}
}

// TestMcpMergeExisting_InlineOverridesWin verifies inline key=val args take
// precedence over the existing server's current values.
func TestMcpMergeExisting_InlineOverridesWin(t *testing.T) {
	existing := config.MCPServerConfig{Name: "GitHub", URL: "https://old"}
	overrides := config.MCPServerConfig{Name: "github", URL: "https://new"}

	merged := mcpMergeExisting(existing, overrides)

	if merged.URL != "https://new" {
		t.Errorf("URL = %q, want inline override %q", merged.URL, "https://new")
	}
}

// TestMcpStepValue_DefaultsMatchPromptDefaults verifies the displayed/prefilled
// value for transport and auth mirrors the wizard's own defaulting so that
// pressing enter unmodified round-trips through commitAddMCP correctly.
func TestMcpStepValue_DefaultsMatchPromptDefaults(t *testing.T) {
	sc := config.MCPServerConfig{} // zero-value: implicit http, no auth

	if got := mcpStepValue(mcpStepTransport, sc); got != "http" {
		t.Errorf("transport hint = %q, want %q", got, "http")
	}
	if got := mcpStepValue(mcpStepAuth, sc); got != "none" {
		t.Errorf("auth hint = %q, want %q", got, "none")
	}
}

// TestMcpNextEditStep_WalksEveryFieldRegardlessOfValue verifies edit mode
// visits every relevant step in order, unlike mcpFirstMissingStep which
// skips fields that already have a value.
func TestMcpNextEditStep_WalksEveryFieldRegardlessOfValue(t *testing.T) {
	sc := config.MCPServerConfig{Name: "x", Transport: "http", URL: "https://y", Auth: "bearer", APIKey: "k"}

	steps := []addMCPStep{mcpStepName}
	cur := mcpStepName
	for cur != mcpStepDone {
		cur = mcpNextEditStep(cur, sc)
		steps = append(steps, cur)
	}

	want := []addMCPStep{mcpStepName, mcpStepTransport, mcpStepURL, mcpStepAuth, mcpStepAPIKey, mcpStepDone}
	if len(steps) != len(want) {
		t.Fatalf("steps = %v, want %v", steps, want)
	}
	for i, s := range steps {
		if s != want[i] {
			t.Errorf("step[%d] = %v, want %v", i, s, want[i])
		}
	}
}

// TestMcpNextEditStep_StdioCommandBranch verifies the stdio branch asks for
// command instead of url.
func TestMcpNextEditStep_StdioCommandBranch(t *testing.T) {
	sc := config.MCPServerConfig{Transport: "stdio"}
	if got := mcpNextEditStep(mcpStepTransport, sc); got != mcpStepCommand {
		t.Errorf("next step = %v, want mcpStepCommand", got)
	}
}

// TestMcpAddPrompt_AnnotatesCurrentValueWhenEditing verifies the prompt shows
// the current value hint only when one is supplied.
func TestMcpAddPrompt_AnnotatesCurrentValueWhenEditing(t *testing.T) {
	withHint := mcpAddPrompt(mcpStepURL, "https://old")
	if got, want := withHint, milkTag()+" url: [current: https://old]"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}

	noHint := mcpAddPrompt(mcpStepURL, "")
	if got, want := noHint, milkTag()+" url:"; got != want {
		t.Errorf("prompt = %q, want %q", got, want)
	}
}
