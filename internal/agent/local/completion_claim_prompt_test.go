package local

import (
	"strings"
	"testing"
)

// TestBuildSystemPrompt_CompletionClaimVerification: the shared rules must
// require a fresh verification tool call before any completion claim. The
// milk-vs-pi eval run (fix-typo, 2026-11-25) scored a hallucinated "no
// remaining occurrences" claim made straight from memory while the diff
// contradicted it — nothing in the prompt guarded against it. The rule lives
// in systemPromptShared and systemPromptSharedLean, so every non-minimal tier
// (including lean) and both roles get it verbatim.
func TestBuildSystemPrompt_CompletionClaimVerification(t *testing.T) {
	wants := []string{
		"verify the claim with a fresh tool call",
		"Match the check to the claim's scope",
		"case-insensitive search",
	}

	for _, tier := range []string{"lean", "standard", "full"} {
		for _, tc := range []struct {
			label          string
			workflow       bool
			escalationName string
		}{
			{"primary", false, ""},
			{"workflow", true, ""},
			{"escalation", false, "mimo-pro"},
		} {
			t.Run(tc.label+"/"+tier, func(t *testing.T) {
				got := buildSystemPrompt("/tmp/repo", "mimo-local", tc.escalationName, tc.workflow, tier, false)
				for _, want := range wants {
					if !strings.Contains(got, want) {
						t.Errorf("prompt (tier=%q, %s) missing completion-claim verification rule %q", tier, tc.label, want)
					}
				}
			})
		}
	}
}
