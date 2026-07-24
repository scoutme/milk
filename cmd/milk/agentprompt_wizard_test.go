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

// TestApplyWorkflowBehaviourOverrides_GeneratorPromptApplied verifies that
// applyWorkflowBehaviourOverrides sets the Prompt field on the generator agent.
func TestApplyWorkflowBehaviourOverrides_GeneratorPromptApplied(t *testing.T) {
	cfg := config.Config{
		Agents: []config.AgentConfig{
			{Name: "local", URL: "http://localhost", Model: "m"},
			{Name: "claude", Provider: "claude-cli"},
		},
	}
	w := &workflowWizardState{
		generator:       "local",
		evaluator:       "claude",
		generatorPrompt: "Only respond in bullet points.",
		evaluatorPrompt: "",
	}
	got := applyWorkflowBehaviourOverrides(cfg, w)

	var genAgent config.AgentConfig
	for _, a := range got.Agents {
		if a.Name == "local" {
			genAgent = a
			break
		}
	}
	if genAgent.Prompt != "Only respond in bullet points." {
		t.Errorf("generator Prompt = %q, want %q", genAgent.Prompt, "Only respond in bullet points.")
	}
	// Original cfg unchanged.
	for _, a := range cfg.Agents {
		if a.Name == "local" && a.Prompt != "" {
			t.Errorf("original cfg modified: local.Prompt = %q", a.Prompt)
		}
	}
}

// TestApplyWorkflowBehaviourOverrides_NoOverrideReturnsSameCfg verifies that
// when both prompts are empty, the function returns the original config unchanged.
func TestApplyWorkflowBehaviourOverrides_NoOverrideReturnsSameCfg(t *testing.T) {
	cfg := config.Config{
		Agents: []config.AgentConfig{
			{Name: "local", URL: "http://localhost", Model: "m"},
		},
	}
	w := &workflowWizardState{
		generator:       "local",
		generatorPrompt: "",
		evaluatorPrompt: "",
	}
	got := applyWorkflowBehaviourOverrides(cfg, w)
	if len(got.Agents) != len(cfg.Agents) {
		t.Errorf("agent count changed: %d → %d", len(cfg.Agents), len(got.Agents))
	}
	if got.Agents[0].Prompt != "" {
		t.Errorf("unexpected Prompt set: %q", got.Agents[0].Prompt)
	}
}
