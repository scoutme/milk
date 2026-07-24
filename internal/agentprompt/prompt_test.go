package agentprompt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func TestRender_AllPlaceholders(t *testing.T) {
	ac := config.AgentConfig{
		Prompt: "mem={{milk:memory}} need={{milk:need}} esc={{milk:escalation}} tools={{milk:tools}}",
	}
	vars := PlaceholderVars{
		Memory:     "M",
		Need:       "N",
		Escalation: "E",
		Tools:      "T",
	}
	got, err := Render(ac, vars)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := "mem=M need=N esc=E tools=T"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestRender_EmptyVarRemovesPlaceholder(t *testing.T) {
	ac := config.AgentConfig{
		Prompt: "before {{milk:memory}} after",
	}
	got, err := Render(ac, PlaceholderVars{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.Contains(got, "{{milk:") {
		t.Errorf("placeholder not removed: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("surrounding text lost: %q", got)
	}
}

func TestRender_NoPlaceholderAutoAppend(t *testing.T) {
	ac := config.AgentConfig{
		Prompt: "You are a strict code reviewer.",
	}
	got, err := Render(ac, PlaceholderVars{Memory: "M"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, milkFooter) {
		t.Errorf("milkFooter not appended; got: %q", got)
	}
}

func TestRender_PromptFileWinsOverInline(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(path, []byte("from file {{milk:need}}"), 0o644); err != nil {
		t.Fatalf("write file: %v", err)
	}
	ac := config.AgentConfig{
		Prompt:     "inline prompt",
		PromptFile: path,
	}
	got, err := Render(ac, PlaceholderVars{Need: "NEED"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "from file") {
		t.Errorf("PromptFile not used; got: %q", got)
	}
	if strings.Contains(got, "inline prompt") {
		t.Errorf("inline Prompt used when PromptFile is set; got: %q", got)
	}
	if !strings.Contains(got, "NEED") {
		t.Errorf("placeholder not substituted; got: %q", got)
	}
}

func TestRender_MissingFileError(t *testing.T) {
	ac := config.AgentConfig{
		PromptFile: "/nonexistent/path/prompt.md",
	}
	_, err := Render(ac, PlaceholderVars{})
	if err == nil {
		t.Error("expected error for missing prompt_file, got nil")
	}
	if !strings.Contains(err.Error(), "agentprompt") {
		t.Errorf("error missing package context: %v", err)
	}
}

func TestRender_NeitherSet(t *testing.T) {
	ac := config.AgentConfig{Name: "test"}
	got, err := Render(ac, PlaceholderVars{Memory: "M"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string when no prompt set, got %q", got)
	}
}

func TestRender_ToolsSubstitution(t *testing.T) {
	ac := config.AgentConfig{
		Prompt: "Available tools: {{milk:tools}}",
	}
	got, err := Render(ac, PlaceholderVars{Tools: "bash, read_file"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(got, "bash, read_file") {
		t.Errorf("tools not substituted; got: %q", got)
	}
}

func TestRender_TrimSpace(t *testing.T) {
	ac := config.AgentConfig{Prompt: "  padded  "}
	got, err := Render(ac, PlaceholderVars{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.HasPrefix(got, " ") || strings.HasSuffix(got, " ") {
		t.Errorf("output not trimmed: %q", got)
	}
}
