package interp

import (
	"os/exec"
	"strings"
)

// gitDiffSummary returns a human-readable summary of uncommitted changes via
// `git diff HEAD` (falls back to `git diff` if HEAD doesn't exist). Used by
// Stage.EmptyOutputFallback == "git_diff" when a turn completed entirely via
// tool calls without producing a closing text summary. Ported from
// internal/workflow/dev.gitDiffSummary.
func gitDiffSummary() string {
	stat, err := exec.Command("git", "diff", "--stat", "HEAD").Output() //nolint:gosec
	if err != nil {
		stat, err = exec.Command("git", "diff", "--stat").Output() //nolint:gosec
		if err != nil {
			return "(turn used tools to make changes; no git diff available)"
		}
	}
	if strings.TrimSpace(string(stat)) == "" {
		return "(turn completed via tool calls; no uncommitted changes detected)"
	}
	diff, err := exec.Command("git", "diff", "HEAD").Output() //nolint:gosec
	if err != nil {
		diff, _ = exec.Command("git", "diff").Output() //nolint:gosec
	}
	const maxDiffBytes = 32 * 1024
	diffStr := string(diff)
	if len(diffStr) > maxDiffBytes {
		diffStr = diffStr[:maxDiffBytes] + "\n... (diff truncated)"
	}
	return "Completed via tool calls. Changes made:\n\n```diff\n" + strings.TrimSpace(string(stat)) + "\n```\n\n" + diffStr
}
