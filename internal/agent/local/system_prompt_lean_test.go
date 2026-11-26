package local

import (
	"strings"
	"testing"
)

// TestBuildSystemPrompt_LeanMarkers pins the lean tier's contract: the
// behavioral rules the evals proved to pay (tool discipline, the
// completion-claim verification rule, conciseness, the role's escalation /
// context-recovery directives) stay; the MANDATORY protocol prose, the
// history-label rule and the tool-family guidance blocks go — their
// when-to-use content lives in the tool schemas (issue #462 system side).
func TestBuildSystemPrompt_LeanMarkers(t *testing.T) {
	lean := buildSystemPrompt("/tmp/repo", "mimo-local", "", false, "lean", true)
	for _, want := range []string{
		"verify the claim with a fresh tool call",
		"find_files finds files by name",
		"Be concise.",
		"escalate only for architectural design",
		"get_session_context",
		"never invent a commit message",
	} {
		if !strings.Contains(lean, want) {
			t.Errorf("lean prompt missing proven rule %q", want)
		}
	}
	for _, banned := range []string{
		"MANDATORY",
		"Additional guidance:",
		"history labels",
		"get_memory",
		"record_memory",
		"current_need",
		"spawn_background_agent",
		"create_task",
		"milk_config_help",
		"Table",
	} {
		if strings.Contains(lean, banned) {
			t.Errorf("lean prompt must not contain protocol prose %q", banned)
		}
	}
}

// Lean keeps the role-specific directives: the escalation role is told it is
// the escalation target and to never fabricate past work, from both framings.
func TestBuildSystemPrompt_LeanRoleTails(t *testing.T) {
	handoff := buildSystemPrompt("/tmp/repo", "mimo-pro", "mimo-pro", false, "lean", true)
	owned := buildSystemPrompt("/tmp/repo", "mimo-pro", "mimo-pro", false, "lean", true)
	for label, got := range map[string]string{"handoff": handoff, "owned": owned} {
		for _, want := range []string{
			"escalation target",
			"Never fabricate a summary of work you did not perform",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("lean %s prompt missing %q", label, want)
			}
		}
	}
	if strings.Contains(handoff, "MANDATORY") {
		t.Error("lean escalation prompt must not contain MANDATORY protocols")
	}
}

// TestBuildSystemPrompt_LeanSizeBudget: the whole point of the tier is the
// per-request byte diet — pin the ceiling so the lean prompt can't creep back
// toward standard. Measured at introduction: lean ≈ 1.9KB vs standard ≈ 7.4KB
// (role framing + rules/tails only, no project instructions).
func TestBuildSystemPrompt_LeanSizeBudget(t *testing.T) {
	lean := buildSystemPrompt("", "mimo-local", "", false, "lean", false)
	std := buildSystemPrompt("", "mimo-local", "", false, "standard", false)
	if len(lean) > 2200 {
		t.Errorf("lean system prompt is %d bytes — budget is 2200", len(lean))
	}
	if len(lean)*2 >= len(std) {
		t.Errorf("lean (%d bytes) must be less than half of standard (%d bytes)", len(lean), len(std))
	}
	if len(std) < len(lean) {
		t.Errorf("sanity: standard (%d) should be larger than lean (%d)", len(lean), len(std))
	}
}

// The lean tier shares the environment tail (date line, working directory,
// project instructions) with the other tiers — same builder path, so parity
// is asserted on the rendered output.
func TestBuildSystemPrompt_LeanEnvTail(t *testing.T) {
	lean := buildSystemPrompt("/tmp/repo", "mimo-local", "", false, "lean", false)
	for _, want := range []string{"Working directory: /tmp/repo", "Current date:"} {
		if !strings.Contains(lean, want) {
			t.Errorf("lean prompt missing env-tail element %q", want)
		}
	}
}

// TestIncludeFamilyGuidance pins the guidance gate: only "lean" drops the
// tool-family mandate blocks.
func TestIncludeFamilyGuidance(t *testing.T) {
	for tier, want := range map[string]bool{
		"":         true,
		"minimal":  true,
		"standard": true,
		"lean":     false,
		"full":     true,
	} {
		if got := includeFamilyGuidance(tier); got != want {
			t.Errorf("includeFamilyGuidance(%q) = %v, want %v", tier, got, want)
		}
	}
}
