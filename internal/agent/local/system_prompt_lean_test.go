package local

import (
	"strings"
	"testing"
)

// TestBuildSystemPrompt_LeanMarkers pins the lean v2 contract (issue #462
// system side): the standard behavioral core and role directives verbatim
// (the restraint mechanisms the A/B measured as worth their bytes), minus the
// toolbox protocol prose and tool-family guidance blocks.
func TestBuildSystemPrompt_LeanMarkers(t *testing.T) {
	lean := buildSystemPrompt("/tmp/repo", "mimo-local", "", false, "lean", true)
	for _, want := range []string{
		// standard behavioral core verbatim
		"verify the claim with a fresh tool call",
		"use find_files — never use grep for this",
		"After issuing a tool call, stop",
		"Always act first, ask only if the working directory alone is genuinely insufficient",
		// distilled honesty + conciseness
		"never invent a commit message",
		"Be concise.",
		// standard role directives verbatim (incl. the fabrication guard)
		"Use escalate only for architectural design",
		"get_session_context",
		"**MANDATORY — unknown recent work**",
	} {
		if !strings.Contains(lean, want) {
			t.Errorf("lean prompt missing proven rule %q", want)
		}
	}
	for _, banned := range []string{
		"**MANDATORY — current_need**",
		"**MANDATORY — git operations**",
		"history labels",
		"get_metrics",
		"get_memory",
		"record_memory",
		"spawn_background_agent",
		"create_task",
		"milk_config_help",
		"gh issue list",
		"Table",
		"Additional guidance:",
	} {
		if strings.Contains(lean, banned) {
			t.Errorf("lean prompt must not contain protocol prose %q", banned)
		}
	}
}

// TestBuildSystemPrompt_LeanRoleTailsMatchStandard pins the v2 core design
// decision: lean carries the STANDARD role tails verbatim (shared consts), so
// the restraint directives cannot drift between tiers.
func TestBuildSystemPrompt_LeanRoleTailsMatchStandard(t *testing.T) {
	escLean := buildSystemPrompt("/tmp/repo", "mimo-pro", "mimo-pro", false, "lean", true)
	escStd := buildSystemPrompt("/tmp/repo", "mimo-pro", "mimo-pro", false, "standard", true)
	priLean := buildSystemPrompt("/tmp/repo", "mimo-local", "", false, "lean", true)
	if !strings.Contains(escLean, escalationHandoffRoleTail) {
		t.Error("lean escalation prompt must carry the standard handoff tail verbatim")
	}
	if !strings.Contains(escStd, escalationHandoffRoleTail) {
		t.Error("sanity: standard escalation prompt must carry the handoff tail")
	}
	if !strings.Contains(priLean, primaryRoleTail) {
		t.Error("lean primary prompt must carry the standard primary tail verbatim")
	}
	for _, want := range []string{
		"escalation target",
		"Never fabricate a summary of work you did not perform",
	} {
		if !strings.Contains(escLean, want) {
			t.Errorf("lean escalation prompt missing %q", want)
		}
	}
}

// TestBuildSystemPrompt_LeanSizeBudget: the tier exists for the per-request
// byte diet — pin the ceiling so the lean prompt can't creep back toward
// standard. v2 budget trades v1's 2.2KB ceiling for the restraint content the
// A/B proved worth keeping; measured at v2 introduction ≈ lean 4.1KB vs
// standard ≈ 6.7KB (framing + rules/tails only, no project instructions).
func TestBuildSystemPrompt_LeanSizeBudget(t *testing.T) {
	lean := buildSystemPrompt("", "mimo-local", "", false, "lean", false)
	std := buildSystemPrompt("", "mimo-local", "", false, "standard", false)
	if len(lean) > 4800 {
		t.Errorf("lean system prompt is %d bytes — budget is 4800", len(lean))
	}
	if float64(len(lean)) > 0.65*float64(len(std)) {
		t.Errorf("lean (%d bytes) must be at most 65%% of standard (%d bytes)", len(lean), len(std))
	}
	if len(std) < len(lean) {
		t.Errorf("sanity: standard (%d) should be larger than lean (%d)", len(std), len(lean))
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
