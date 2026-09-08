package main

import (
	"strings"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/agent/local"
)

// sidePanelLines returns the full (unwindowed) content lines and inner
// content width for the given side-panel region. This is the single place
// that knows how to build each panel's content; rendering, scroll clamping,
// and selection-text extraction all go through it so the three can never
// drift out of sync with each other.
func (m *model) sidePanelLines(region panelRegion) (lines []string, inner int) {
	switch region {
	case regionMemory:
		return buildPanelLines(m.mem, memoryPanelInner, m.currentSessionBricks()), memoryPanelInner
	case regionTasks:
		return buildTasksPanelLines(m.taskStore, tasksPanelInner), tasksPanelInner
	case regionBackground:
		var jobs []local.Job
		if m.agents.backgroundMgr != nil {
			jobs = m.agents.backgroundMgr.Jobs()
		}
		return buildBackgroundPanelLines(jobs, backgroundPanelInner), backgroundPanelInner
	case regionWorkflow:
		return buildWorkflowPanelLines(m.workflowState, workflowPanelInner), workflowPanelInner
	}
	return nil, 0
}

// panelOffsetPtr returns a pointer to the model's persisted scroll offset for
// the given side-panel region, or nil for regionNone/an unknown region.
// Returning a pointer (rather than a value + a separate setter) lets wheel
// scrolling and render-time clamping share one read-modify-write site per
// region instead of a growing switch in each caller.
func (m *model) panelOffsetPtr(region panelRegion) *int {
	switch region {
	case regionMemory:
		return &m.panelOffset
	case regionTasks:
		return &m.tasksOffset
	case regionBackground:
		return &m.backgroundOffset
	case regionWorkflow:
		return &m.workflowPanelOffset
	}
	return nil
}

// renderSidePanel renders any side panel into exactly h lines at its own
// inner content width: clamps the region's persisted scroll offset, applies
// the active panel-text selection highlight when this region owns it, and
// pads every row to the inner width. Scrolling, selection, and padding are
// therefore identical across panels — only content (sidePanelLines) differs
// per panel.
func (m *model) renderSidePanel(region panelRegion, h int) string {
	if !isTTY {
		return strings.Repeat("\n", h)
	}
	all, inner := m.sidePanelLines(region)
	total := len(all)

	offset := m.panelOffsetPtr(region)
	maxOffset := max(total-h, 0)
	if *offset > maxOffset {
		*offset = maxOffset
	}

	if m.panelSelRegion == region {
		all = applyPanelSelectionHighlight(all, m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
	}

	lines := all[*offset:]
	for len(lines) < h {
		lines = append(lines, "")
	}
	lines = lines[:h]

	var rows []string
	for _, line := range lines {
		lineW := utf8.RuneCountInString(stripANSI(line))
		if lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, line)
	}
	return strings.Join(rows, "\n")
}

// renderSidePanelScrollbar returns the 1-column scrollbar for any side
// panel: a dim │ track with a ▌ thumb when content overflows h rows, or a
// blank column otherwise.
func (m *model) renderSidePanelScrollbar(region panelRegion, h int) string {
	all, _ := m.sidePanelLines(region)
	total := len(all)
	needsBar := total > h

	var rows []string
	if !needsBar {
		for range h {
			rows = append(rows, " ")
		}
		return strings.Join(rows, "\n")
	}

	thumbTop, thumbBot := scrollThumb(h, total, *m.panelOffsetPtr(region))
	for i := range h {
		if i >= thumbTop && i <= thumbBot {
			rows = append(rows, dim("▌"))
		} else {
			rows = append(rows, dim("│"))
		}
	}
	return strings.Join(rows, "\n")
}

// scrollThumb computes the inclusive [top, bot] row indices of the scroll thumb
// within a viewport of height h showing content of length total starting at offset.
func scrollThumb(h, total, offset int) (top, bot int) {
	thumbH := max(h*h/total, 1)
	top = offset * (h - thumbH) / (total - h)
	bot = top + thumbH - 1
	if bot >= h {
		bot = h - 1
	}
	return top, bot
}
