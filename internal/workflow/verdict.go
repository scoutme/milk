package workflow

import "strings"

// WorkflowVerdict is the structured outcome of an evaluator turn.
type WorkflowVerdict int

const (
	VerdictUnknown         WorkflowVerdict = iota
	VerdictGoodToGo                        // sprint accepted; advance or finish
	VerdictNeedsRefinement                 // retry same sprint
	VerdictSprintDone                      // sprint deliverables complete; advance unconditionally
)

func (v WorkflowVerdict) String() string {
	switch v {
	case VerdictGoodToGo:
		return "good_to_go"
	case VerdictNeedsRefinement:
		return "needs_refinement"
	case VerdictSprintDone:
		return "sprint_done"
	default:
		return "unknown"
	}
}

// ParseVerdict extracts a structured verdict from the evaluator's text response.
// It scans lines from the end of the response and returns the verdict of the
// last line that contains a recognised keyword (case-insensitive). Scanning
// from the end ensures that the evaluator's final verdict line wins over any
// discussion of verdicts earlier in the reasoning section.
func ParseVerdict(response string) WorkflowVerdict {
	lines := strings.Split(response, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.ToLower(strings.TrimSpace(lines[i]))
		switch {
		case strings.Contains(line, "good_to_go") || strings.Contains(line, "good to go"):
			return VerdictGoodToGo
		case strings.Contains(line, "sprint_done") || strings.Contains(line, "sprint done"):
			return VerdictSprintDone
		case strings.Contains(line, "needs_refinement") || strings.Contains(line, "needs refinement"):
			return VerdictNeedsRefinement
		}
	}
	return VerdictUnknown
}
