package local

import (
	"strings"
	"testing"
)

// TestClassifyPrompts_SingleFileRule pins the single-file mechanical
// transformation rule in every classifier variant. The rule came out of the
// 2026-11-25 flip-rate study (1 of 5 runs escalated a single-file
// extract-method refactor at temperature 0); without it the escalation
// criteria leave that prompt genuinely ambiguous.
func TestClassifyPrompts_SingleFileRule(t *testing.T) {
	prompts := map[string]string{
		"shared":  classifyPrompt("do the thing"),
		"bedrock": classifyPromptBedrock("do the thing"),
	}
	for name, p := range prompts {
		if !strings.Contains(p, "Single-file mechanical transformations") {
			t.Errorf("%s prompt lacks the single-file mechanical transformations rule:\n%s", name, p)
		}
		if !strings.Contains(p, "do the thing") {
			t.Errorf("%s prompt does not embed the task text", name)
		}
	}
	// The escalate criteria must stay present: the single-file line is a
	// clarification of them, not a replacement.
	for name, p := range prompts {
		for _, want := range []string{"multi-file", "architectural"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt lost the escalate criterion %q", name, want)
			}
		}
	}
}
