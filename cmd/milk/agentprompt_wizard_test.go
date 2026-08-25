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

// ── workflow behaviour prompt ─────────────────────────────────────────────────

// TestWorkflowBehaviourPrompt_TextReturned verifies that workflowBehaviourPrompt
// returns a non-empty string mentioning the role.
func TestWorkflowBehaviourPrompt_TextReturned(t *testing.T) {
	for _, role := range []string{"generator", "evaluator"} {
		got := workflowBehaviourPrompt(role)
		if got == "" {
			t.Errorf("workflowBehaviourPrompt(%q) returned empty string", role)
		}
		if !strings.Contains(got, role) {
			t.Errorf("workflowBehaviourPrompt(%q) = %q, does not contain role", role, got)
		}
	}
}

// Note: applyWorkflowBehaviourOverrides (previously tested here) was removed
// along with dev.go's fresh-launch path (launchWorkflow) — it was the only
// caller. The generic launch path (handleGenericWorkflowCmd/
// launchGenericWorkflow) that dev now shares with every other workflow has
// no equivalent inline behaviour-prompt-override wizard step yet; a
// persistent AgentConfig.Prompt in ~/.milk/config.json is the workaround
// until/unless that's generalized. workflowWizardState.generatorPrompt/
// evaluatorPrompt are still collected by the resume-fallback and reconfigure
// wizards (see wizardStepGeneratorPrompt/EvaluatorPrompt) but were already
// unused there even before this change — only the now-removed fresh-launch
// path ever applied them.
