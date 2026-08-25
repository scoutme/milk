package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// ── addStepPrompt wizard path ─────────────────────────────────────────────────

// TestAddAgentWizard_InlinePromptSerialised verifies that entering inline text
// at the addStepPrompt step sets ac.Prompt and leaves ac.PromptFile empty.
func TestAddAgentWizard_InlinePromptSerialised(t *testing.T) {
	st := &addAgentState{
		ac:          config.AgentConfig{Name: "test", URL: "http://x", Model: "m"},
		step:        addStepPrompt,
		promptAsked: false,
	}
	answer := "You are a strict code reviewer."
	// Simulate the handler: inline text (no "file=" prefix) sets Prompt.
	if strings.HasPrefix(answer, "file=") {
		st.ac.PromptFile = strings.TrimPrefix(answer, "file=")
	} else if answer != "" {
		st.ac.Prompt = answer
	}
	st.promptAsked = true

	if st.ac.Prompt != answer {
		t.Errorf("Prompt = %q, want %q", st.ac.Prompt, answer)
	}
	if st.ac.PromptFile != "" {
		t.Errorf("PromptFile should be empty, got %q", st.ac.PromptFile)
	}
}

// TestAddAgentWizard_FilePromptSerialised verifies that entering "file=<path>"
// at the addStepPrompt step sets ac.PromptFile and leaves ac.Prompt empty.
func TestAddAgentWizard_FilePromptSerialised(t *testing.T) {
	st := &addAgentState{
		ac:          config.AgentConfig{Name: "test", URL: "http://x", Model: "m"},
		step:        addStepPrompt,
		promptAsked: false,
	}
	answer := "file=/home/user/.milk/prompts/reviewer.md"
	if strings.HasPrefix(answer, "file=") {
		st.ac.PromptFile = strings.TrimPrefix(answer, "file=")
	} else if answer != "" {
		st.ac.Prompt = answer
	}
	st.promptAsked = true

	if st.ac.PromptFile != "/home/user/.milk/prompts/reviewer.md" {
		t.Errorf("PromptFile = %q, want %q", st.ac.PromptFile, "/home/user/.milk/prompts/reviewer.md")
	}
	if st.ac.Prompt != "" {
		t.Errorf("Prompt should be empty, got %q", st.ac.Prompt)
	}
}

// TestAddAgentWizard_SkipPromptLeavesEmpty verifies that pressing Enter (empty
// input) at the addStepPrompt step leaves both Prompt and PromptFile empty.
func TestAddAgentWizard_SkipPromptLeavesEmpty(t *testing.T) {
	st := &addAgentState{
		ac:          config.AgentConfig{Name: "test", URL: "http://x", Model: "m"},
		step:        addStepPrompt,
		promptAsked: false,
	}
	answer := ""
	if strings.HasPrefix(answer, "file=") {
		st.ac.PromptFile = strings.TrimPrefix(answer, "file=")
	} else if answer != "" {
		st.ac.Prompt = answer
	}
	st.promptAsked = true

	if st.ac.Prompt != "" {
		t.Errorf("Prompt should be empty after skip, got %q", st.ac.Prompt)
	}
	if st.ac.PromptFile != "" {
		t.Errorf("PromptFile should be empty after skip, got %q", st.ac.PromptFile)
	}
}

// Note: applyWorkflowBehaviourOverrides and the inline behaviour-prompt wizard
// steps it fed (previously tested here) were removed entirely, not just for
// dev's retired fresh-launch path — they were already dead in every
// remaining path (resume-fallback, reconfigure) even before that. See
// workflowWizardState's doc for why: a task description (and the plan the
// designer produces from it) can already carry a specific behaviour request
// into the generator/evaluator prompts; a persistent AgentConfig.Prompt in
// ~/.milk/config.json remains the way to give an agent a standing override.
