package workflow_test

import (
	"testing"

	"github.com/scoutme/milk/internal/workflow"
)

func TestParseVerdict(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected workflow.WorkflowVerdict
	}{
		{"good_to_go exact", "good_to_go", workflow.VerdictGoodToGo},
		{"good to go spaced", "This sprint is good to go.", workflow.VerdictGoodToGo},
		{"good_to_go uppercase", "GOOD_TO_GO", workflow.VerdictGoodToGo},
		{"good_to_go in prose", "All criteria met. good_to_go", workflow.VerdictGoodToGo},

		{"needs_refinement exact", "needs_refinement", workflow.VerdictNeedsRefinement},
		{"needs refinement spaced", "The sprint needs refinement.", workflow.VerdictNeedsRefinement},
		{"needs_refinement uppercase", "NEEDS_REFINEMENT", workflow.VerdictNeedsRefinement},
		{"needs_refinement in prose", "There are issues. needs_refinement", workflow.VerdictNeedsRefinement},

		{"next_sprint exact", "next_sprint", workflow.VerdictNextSprint},
		{"next sprint spaced", "Move to next sprint.", workflow.VerdictNextSprint},
		{"next_sprint uppercase", "NEXT_SPRINT", workflow.VerdictNextSprint},
		{"next_sprint in prose", "Sprint done. next_sprint", workflow.VerdictNextSprint},

		// Precedence: good_to_go wins over next_sprint
		{"good_to_go beats next_sprint", "next_sprint but actually good_to_go", workflow.VerdictGoodToGo},
		// Precedence: good_to_go wins over needs_refinement
		{"good_to_go beats needs_refinement", "needs_refinement, wait no good_to_go", workflow.VerdictGoodToGo},
		// Precedence: next_sprint wins over needs_refinement
		{"next_sprint beats needs_refinement", "needs_refinement, but next_sprint", workflow.VerdictNextSprint},

		{"empty", "", workflow.VerdictUnknown},
		{"no keyword", "The sprint output looks fine.", workflow.VerdictUnknown},
		{"random prose", "I am an evaluator and I have thoughts.", workflow.VerdictUnknown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := workflow.ParseVerdict(tt.input)
			if got != tt.expected {
				t.Errorf("ParseVerdict(%q) = %v, want %v", tt.input, got, tt.expected)
			}
		})
	}
}
