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
	add := func(s string) { lines = append(lines, s) }

	// Title row (matches memory panel style)
	add(stylePanelTitle.Render(truncatePanel(" workflow", inner)))
	add("")

	if st == nil || st.WorkflowName == "" {
		add(dim("no active workflow"))
	} else {
		// Task description — word-wrap across multiple lines if needed
		if st.Task != "" {
			for _, line := range wordWrapPanel(st.Task, inner) {
				add(dim(line))
			}
			add("")
		}

		// Current sprint / pass / role
		add(truncatePanel(bold(st.WorkflowName)+"  sprint "+fmt.Sprintf("%d", st.Sprint), inner))
		add(dim(fmt.Sprintf("pass %d  role: %s", st.Pass, st.Role)))
		add("")

		// Verdict history
		for _, v := range st.VerdictHistory {
			icon := "✓"
			if v.Verdict == "needs_refinement" || v.Verdict == "unknown" {
				icon = "·"
			}
			add(truncatePanel(dim(fmt.Sprintf("  %s s%d p%d → %s", icon, v.Sprint, v.Pass, v.Verdict)), inner))
		}
		if st.Role != "" && st.Role != "done" {
			add(truncatePanel(dim(fmt.Sprintf("  → s%d p%d %s…", st.Sprint, st.Pass, st.Role)), inner))
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

// wordWrapPanel wraps s into lines of at most maxWidth visible characters.
func wordWrapPanel(s string, maxWidth int) []string {
	if maxWidth <= 0 {
		return []string{s}
	}
	words := strings.Fields(s)
	if len(words) == 0 {
		return nil
	}
	var out []string
	line := words[0]
	for _, w := range words[1:] {
		if len(line)+1+len(w) <= maxWidth {
			line += " " + w
		} else {
			out = append(out, line)
			line = w
		}
	}
	return append(out, line)
}

// workflowPanelLineCount returns the number of content lines the panel would occupy.
func workflowPanelLineCount(st *workflow.State) int {
	// title + blank
	n := 2
	if st == nil || st.WorkflowName == "" {
		return n + 1 // "no active workflow"
	}
	inner := workflowPanelWidth - 2
	if st.Task != "" {
		n += len(wordWrapPanel(st.Task, inner)) + 1 // task lines + blank
	}
	n += 3 // workflow+sprint, pass+role, blank
	n += len(st.VerdictHistory)
	if st.Role != "" && st.Role != "done" {
		n++ // in-progress arrow
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
