package local

import (
	"strings"
	"testing"
)

// TestBuildSystemPrompt_TierStandard verifies that the default ("standard") tier
// produces a prompt that contains the shared rules block.
func TestBuildSystemPrompt_TierStandard(t *testing.T) {
	prompt := buildSystemPrompt("", "myagent", "", false, "standard")
	if !strings.Contains(prompt, "Rules:") {
		t.Error("standard tier must contain 'Rules:' section from systemPromptShared")
	}
	if !strings.Contains(prompt, "MANDATORY") {
		t.Error("standard tier must contain MANDATORY guidance from systemPromptShared")
	}
}

// TestBuildSystemPrompt_TierEmpty verifies that an empty tier string behaves
// identically to "standard".
func TestBuildSystemPrompt_TierEmpty(t *testing.T) {
	standard := buildSystemPrompt("", "myagent", "", false, "standard")
	empty := buildSystemPrompt("", "myagent", "", false, "")
	if standard != empty {
		t.Errorf("empty tier must equal standard tier\nstandard:\n%s\nempty:\n%s", standard, empty)
	}
}

// TestBuildSystemPrompt_TierMinimal_ShorterThanStandard verifies that the
// "minimal" tier produces a prompt strictly shorter than "standard" and that it
// does not contain the tool-use instruction or reasoning guidance sections.
func TestBuildSystemPrompt_TierMinimal_ShorterThanStandard(t *testing.T) {
	standard := buildSystemPrompt("", "myagent", "", false, "standard")
	minimal := buildSystemPrompt("", "myagent", "", false, "minimal")

	if len(minimal) >= len(standard) {
		t.Errorf("minimal tier (%d chars) must be shorter than standard (%d chars)", len(minimal), len(standard))
	}
	if strings.Contains(minimal, "Rules:") {
		t.Error("minimal tier must NOT contain 'Rules:' tool-use instruction block")
	}
	if strings.Contains(minimal, "MANDATORY") {
		t.Error("minimal tier must NOT contain 'MANDATORY' reasoning guidance")
	}
}

// TestBuildSystemPrompt_TierFull_AtLeastAsLongAsStandard verifies that the
// "full" tier produces a prompt at least as long as "standard".
func TestBuildSystemPrompt_TierFull_AtLeastAsLongAsStandard(t *testing.T) {
	standard := buildSystemPrompt("", "myagent", "", false, "standard")
	full := buildSystemPrompt("", "myagent", "", false, "full")

	if len(full) < len(standard) {
		t.Errorf("full tier (%d chars) must be >= standard (%d chars)", len(full), len(standard))
	}
}

// TestBuildSystemPrompt_TierFull_ContainsExtraGuidance verifies that the "full"
// tier includes content from systemPromptSharedFull not present in "standard".
func TestBuildSystemPrompt_TierFull_ContainsExtraGuidance(t *testing.T) {
	standard := buildSystemPrompt("", "myagent", "", false, "standard")
	full := buildSystemPrompt("", "myagent", "", false, "full")

	// systemPromptSharedFull introduces a unique marker "Additional guidance:"
	if !strings.Contains(full, "Additional guidance:") {
		t.Error("full tier must contain 'Additional guidance:' from systemPromptSharedFull")
	}
	if strings.Contains(standard, "Additional guidance:") {
		t.Error("standard tier must NOT contain 'Additional guidance:' from systemPromptSharedFull")
	}
}

// TestBuildSystemPrompt_TierMinimal_EscalationRole verifies that "minimal" works
// correctly for the escalation role too.
func TestBuildSystemPrompt_TierMinimal_EscalationRole(t *testing.T) {
	standard := buildSystemPrompt("", "haiku", "haiku", false, "standard")
	minimal := buildSystemPrompt("", "haiku", "haiku", false, "minimal")

	if len(minimal) >= len(standard) {
		t.Errorf("escalation minimal (%d chars) must be shorter than standard (%d chars)", len(minimal), len(standard))
	}
	if strings.Contains(minimal, "Rules:") {
		t.Error("escalation minimal must NOT contain 'Rules:' block")
	}
	// Role framing must still be present.
	if !strings.Contains(minimal, "haiku") {
		t.Error("escalation minimal must still include agent name in framing")
	}
	if !strings.Contains(minimal, "escalation agent") {
		t.Error("escalation minimal must still include role framing")
	}
}

// TestBuildSystemPrompt_TierMinimal_WorkflowRole verifies "minimal" for a
// workflow step executor.
func TestBuildSystemPrompt_TierMinimal_WorkflowRole(t *testing.T) {
	standard := buildSystemPrompt("", "gen", "", true, "standard")
	minimal := buildSystemPrompt("", "gen", "", true, "minimal")

	if len(minimal) >= len(standard) {
		t.Errorf("workflow minimal (%d chars) must be shorter than standard (%d chars)", len(minimal), len(standard))
	}
	if strings.Contains(minimal, "Rules:") {
		t.Error("workflow minimal must NOT contain 'Rules:' block")
	}
}

// TestBuildSystemPrompt_CWDAppended verifies that the working directory is
// appended for all tier values.
func TestBuildSystemPrompt_CWDAppended(t *testing.T) {
	for _, tier := range []string{"minimal", "standard", "full"} {
		prompt := buildSystemPrompt("/home/user/myproject", "agent", "", false, tier)
		if !strings.Contains(prompt, "Working directory: /home/user/myproject") {
			t.Errorf("tier %q: expected working directory in prompt", tier)
		}
	}
}
