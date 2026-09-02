package interp

import (
	"strings"
	"testing"
)

func TestIsTerminatedTurn(t *testing.T) {
	tests := []struct {
		name string
		out  string
		want bool
	}{
		{"empty", "", false},
		{"normal verdict", "good_to_go", false},
		{"normal prose", "The sprint looks good. good_to_go", false},
		{"terminated turn", "[turn terminated: the model was stuck in a reasoning repetition loop and could not self-recover after multiple attempts]", true},
		{"terminated with tool trail", "[turn ended without a final summary — tool activity this turn:]\n- read_file(src/game.ts)\n  → (content)", true},
		{"terminated mixed case", "[Turn Terminated: something]", true},
		{"partial match no bracket", "the model was stuck in a turn terminated loop", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTerminatedTurn(tt.out)
			if got != tt.want {
				t.Errorf("isTerminatedTurn(%q) = %v, want %v", tt.out, got, tt.want)
			}
		})
	}
}

func TestTruncateLargeVars(t *testing.T) {
	t.Run("short vars unchanged", func(t *testing.T) {
		vars := map[string]any{
			"plan":  "short plan",
			"count": 42,
		}
		got := truncateLargeVars(vars)
		if got["plan"] != "short plan" {
			t.Errorf("expected plan unchanged, got %v", got["plan"])
		}
		if got["count"] != 42 {
			t.Errorf("expected count unchanged, got %v", got["count"])
		}
	})

	t.Run("long var truncated", func(t *testing.T) {
		long := strings.Repeat("x", maxVarChars+1000)
		vars := map[string]any{
			"plan": long,
		}
		got := truncateLargeVars(vars)
		result := got["plan"].(string)
		if len(result) >= len(long) {
			t.Errorf("expected truncation, got len %d >= %d", len(result), len(long))
		}
		if !strings.Contains(result, "chars omitted") {
			t.Error("expected omission marker in truncated output")
		}
		// Should preserve head and tail
		if !strings.HasPrefix(result, "xxxxx") {
			t.Error("expected head preserved")
		}
		if !strings.HasSuffix(result, "xxxxx") {
			t.Error("expected tail preserved")
		}
	})

	t.Run("non-string values passed through", func(t *testing.T) {
		vars := map[string]any{
			"num":  42,
			"flag": true,
			"list": []int{1, 2, 3},
		}
		got := truncateLargeVars(vars)
		if got["num"] != 42 {
			t.Error("expected num unchanged")
		}
		if got["flag"] != true {
			t.Error("expected flag unchanged")
		}
	})
}

func TestTruncateLargeVarsWithBudget(t *testing.T) {
	long := strings.Repeat("y", sectionCharBudget+5000)
	vars := map[string]any{
		"sprint_output": long,
		"plan":          "short",
	}
	got := truncateLargeVarsWithBudget(vars)

	// Long var should be truncated
	result := got["sprint_output"].(string)
	if len(result) >= len(long) {
		t.Errorf("expected truncation, got len %d >= %d", len(result), len(long))
	}
	if !strings.Contains(result, "chars omitted") {
		t.Error("expected omission marker")
	}

	// Short var should be unchanged
	if got["plan"] != "short" {
		t.Error("expected plan unchanged")
	}
}

func TestSummarizeLongOutput(t *testing.T) {
	t.Run("short output unchanged", func(t *testing.T) {
		out := "short output"
		got := summarizeLongOutput(out, 1000)
		if got != out {
			t.Errorf("expected unchanged, got different")
		}
	})

	t.Run("long output truncated", func(t *testing.T) {
		long := strings.Repeat("z", 5000)
		got := summarizeLongOutput(long, 1000)
		if len(got) >= len(long) {
			t.Errorf("expected truncation, got len %d >= %d", len(got), len(long))
		}
		if !strings.Contains(got, "chars omitted") {
			t.Error("expected omission marker")
		}
	})
}

func TestTruncatePromptSections(t *testing.T) {
	t.Run("short prompt unchanged", func(t *testing.T) {
		prompt := "short prompt"
		got := truncatePromptSections(prompt, 1000)
		if got != prompt {
			t.Error("expected unchanged")
		}
	})

	t.Run("long prompt truncated", func(t *testing.T) {
		long := strings.Repeat("a", 50000)
		got := truncatePromptSections(long, 30000)
		if len(got) >= len(long) {
			t.Errorf("expected truncation, got len %d >= %d", len(got), len(long))
		}
		if !strings.Contains(got, "chars omitted") {
			t.Error("expected omission marker")
		}
	})
}
