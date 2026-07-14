package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/scoutme/milk/internal/workflow"
)

const workflowPanelWidth = 30

var styleWorkflowPanel = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderLeft(true).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#AAA", Dark: "#555"}).
	PaddingLeft(1)

// renderWorkflowPanel renders the workflow progress panel into exactly h lines.
func (m *model) renderWorkflowPanel(h int) string {
	st := m.workflowState
	inner := workflowPanelWidth - 2 // left border + padding

	var lines []string

	if st == nil || st.WorkflowName == "" {
		lines = append(lines, dim("no active workflow"))
	} else {
		header := bold(st.WorkflowName+" workflow") + "  sprint " + fmt.Sprintf("%d", st.Sprint)
		lines = append(lines, truncatePanel(header, inner))
		lines = append(lines, dim(fmt.Sprintf("pass %d  role: %s", st.Pass, st.Role)))
		lines = append(lines, "")

		for _, v := range st.VerdictHistory {
			icon := "✓"
			if v.Verdict == "needs_refinement" || v.Verdict == "unknown" {
				icon = "·"
			}
			line := dim(fmt.Sprintf("  %s s%d p%d → %s", icon, v.Sprint, v.Pass, v.Verdict))
			lines = append(lines, truncatePanel(line, inner))
		}
		if st.Role != "" && st.Role != "done" {
			arrow := dim(fmt.Sprintf("  → s%d p%d %s…", st.Sprint, st.Pass, st.Role))
			lines = append(lines, truncatePanel(arrow, inner))
		}
	}

	// Pad or trim to exactly h lines.
	for len(lines) < h {
		lines = append(lines, "")
	}
	if len(lines) > h {
		lines = lines[:h]
	}

	content := strings.Join(lines, "\n")
	return styleWorkflowPanel.Width(inner).Render(content)
}

// workflowPanelLineCount returns the number of content lines the panel would occupy.
func workflowPanelLineCount(st *workflow.State) int {
	if st == nil {
		return 1
	}
	n := 3 + len(st.VerdictHistory)
	if st.Role != "" && st.Role != "done" {
		n++
	}
	return n
}

func truncatePanel(s string, maxWidth int) string {
	if maxWidth <= 0 {
		return s
	}
	stripped := []rune(stripANSI(s))
	if len(stripped) <= maxWidth {
		return s
	}
	return string(stripped[:maxWidth-1]) + "…"
}
