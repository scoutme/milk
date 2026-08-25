package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"github.com/scoutme/milk/internal/workflow"
)

const workflowPanelWidth = 31        // total incl. 1 scrollbar column (used to size the main area)
const workflowPanelContentWidth = 30 // border + padding + inner content, excl. scrollbar

var styleWorkflowPanel = lipgloss.NewStyle().
	BorderStyle(lipgloss.NormalBorder()).
	BorderLeft(true).
	BorderForeground(lipgloss.AdaptiveColor{Light: "#AAA", Dark: "#555"}).
	PaddingLeft(1)

// buildWorkflowPanelLines returns the full (unwindowed, unpadded) content
// lines for the workflow panel. Scrolling and highlighting both operate on
// this slice before it is windowed down to the visible h rows.
func buildWorkflowPanelLines(st *workflow.State, inner int) []string {
	var lines []string
	add := func(ss ...string) { lines = append(lines, ss...) }

	// Title row (matches memory panel style)
	add(stylePanelTitle.Render(truncatePanel(" workflow", inner)))
	add("")

	switch {
	case st == nil || st.WorkflowName == "":
		add(dim("no active workflow"))
	case st.Generic:
		add(buildGenericWorkflowPanelLines(st, inner)...)
	default:
		add(buildDevWorkflowPanelLines(st, inner)...)
	}

	return lines
}

// buildDevWorkflowPanelLines renders dev.go's own Sprint/Pass/TotalSprints/
// VerdictHistory progress reporting (wfdev.WorkflowProgressMsg).
func buildDevWorkflowPanelLines(st *workflow.State, inner int) []string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	// Task description — word-wrap across multiple lines if needed
	if st.Task != "" {
		for _, line := range wordWrapPanel(st.Task, inner) {
			add(dim(line))
		}
		add("")
	}

	// Current sprint / pass / role
	sprintLabel := fmt.Sprintf("%d", st.Sprint)
	if st.TotalSprints > 0 {
		sprintLabel = fmt.Sprintf("%d/%d", st.Sprint, st.TotalSprints)
	}
	add(truncatePanel(bold(st.WorkflowName)+"  sprint "+sprintLabel, inner))
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

	return lines
}

// buildGenericWorkflowPanelLines renders an interpreter-driven run's
// progress (workflow.ProgressMsg): StagePath instead of Sprint/Pass, no
// verdict history (the checkpoint's trace is the record of what happened,
// not surfaced in this panel).
func buildGenericWorkflowPanelLines(st *workflow.State, inner int) []string {
	var lines []string
	add := func(s string) { lines = append(lines, s) }

	if st.Task != "" {
		for _, line := range wordWrapPanel(st.Task, inner) {
			add(dim(line))
		}
		add("")
	}

	add(truncatePanel(bold(st.WorkflowName), inner))
	if st.Role != "" {
		add(dim("role: " + st.Role))
	}
	add("")

	if st.StagePath != "" {
		for _, line := range wordWrapPanel(st.StagePath, inner) {
			add(truncatePanel(dim("  "+line), inner))
		}
	}

	return lines
}

// renderWorkflowPanel renders the workflow progress panel into exactly h lines,
// scrolled to m.workflowPanelOffset (clamped so it never scrolls past the last
// screenful) and with any active panel-text selection highlighted.
func (m *model) renderWorkflowPanel(h int) string {
	inner := workflowPanelContentWidth - 2 // left border + padding

	all := buildWorkflowPanelLines(m.workflowState, inner)
	total := len(all)

	maxOffset := max(total-h, 0)
	if m.workflowPanelOffset > maxOffset {
		m.workflowPanelOffset = maxOffset
	}

	if m.panelSelRegion == regionWorkflow {
		all = applyPanelSelectionHighlight(all, m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
	}

	// Pad or trim to exactly h lines.
	lines := all[m.workflowPanelOffset:]
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]

	// Pre-pad each line to exactly inner cols so lipgloss does not re-wrap when
	// the style is rendered — Width() on a bordered style triggers cellbuf.Wrap,
	// which adds lines and makes the panel taller than h.
	var rows []string
	for _, line := range lines {
		lineW := len([]rune(stripANSI(line)))
		if lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, line)
	}
	content := strings.Join(rows, "\n")
	return styleWorkflowPanel.Render(content)
}

// renderWorkflowPanelScrollbar returns a 1-column string of h lines: a dim │
// track with a ▌ thumb when the panel content overflows, or a blank column
// otherwise. Mirrors renderPanelScrollbar for the memory panel.
func (m *model) renderWorkflowPanelScrollbar(h int) string {
	inner := workflowPanelContentWidth - 2
	total := len(buildWorkflowPanelLines(m.workflowState, inner))
	needsBar := total > h

	var rows []string
	if !needsBar {
		for range h {
			rows = append(rows, " ")
		}
		return strings.Join(rows, "\n")
	}

	thumbTop, thumbBot := scrollThumb(h, total, m.workflowPanelOffset)
	for i := range h {
		if i >= thumbTop && i <= thumbBot {
			rows = append(rows, dim("▌"))
		} else {
			rows = append(rows, dim("│"))
		}
	}
	return strings.Join(rows, "\n")
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
	return len(buildWorkflowPanelLines(st, workflowPanelContentWidth-2))
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
