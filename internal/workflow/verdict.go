package workflow

import "strings"

// WorkflowVerdict is the structured outcome of an evaluator turn.
type WorkflowVerdict int

const (
	VerdictUnknown         WorkflowVerdict = iota
	VerdictGoodToGo                        // sprint accepted; advance or finish
	VerdictNeedsRefinement                 // retry same sprint
	VerdictNextSprint                      // advance to next sprint unconditionally
)

func (v WorkflowVerdict) String() string {
	switch v {
	case VerdictGoodToGo:
		return "good_to_go"
	case VerdictNeedsRefinement:
		return "needs_refinement"
	case VerdictNextSprint:
		return "next_sprint"
	default:
		return "unknown"
	}
}

// ParseVerdict extracts a structured verdict from the evaluator's text response.
// Keywords are matched case-insensitively; precedence: good_to_go > next_sprint > needs_refinement.
func ParseVerdict(response string) WorkflowVerdict {
	lower := strings.ToLower(response)
	switch {
	case strings.Contains(lower, "good_to_go") || strings.Contains(lower, "good to go"):
		return VerdictGoodToGo
	case strings.Contains(lower, "next_sprint") || strings.Contains(lower, "next sprint"):
		return VerdictNextSprint
	case strings.Contains(lower, "needs_refinement") || strings.Contains(lower, "needs refinement"):
		return VerdictNeedsRefinement
	default:
		return VerdictUnknown
	}
}
