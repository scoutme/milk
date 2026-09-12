package main

import (
	"strings"
	"unicode/utf8"

	"github.com/charmbracelet/lipgloss"
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

// panelShortcut returns the F-key label shown next to a panel's title,
// matching the F1-F4 global shortcuts wired into repl.go's key handler
// (mirrored by /panel <name>): memory, tasks, background, workflow — the
// same left-to-right order panels are joined in View().
func panelShortcut(region panelRegion) string {
	switch region {
	case regionMemory:
		return "F1"
	case regionTasks:
		return "F2"
	case regionBackground:
		return "F3"
	case regionWorkflow:
		return "F4"
	}
	return ""
}

// panelTitleLine renders a side panel's title row: the title left-aligned
// (styled via stylePanelTitle) and its F1-F4 shortcut right-aligned within
// inner columns, dimmed — so the toggle key is visible without opening
// /panel or memorizing it. Title is truncated, not the hint, if both
// together would overflow inner.
func panelTitleLine(title string, region panelRegion, inner int) string {
	hint := panelShortcut(region)
	if hint == "" {
		return stylePanelTitle.Render(truncate(title, inner))
	}
	hintW := utf8.RuneCountInString(hint)
	maxTitleW := max(inner-hintW-1, 1)
	titleRunes := []rune(title)
	if len(titleRunes) > maxTitleW {
		title = string(titleRunes[:maxTitleW])
	}
	pad := max(inner-utf8.RuneCountInString(title)-hintW, 1)
	return stylePanelTitle.Render(title) + strings.Repeat(" ", pad) + dim(hint)
}

// autoOpenPanel opens the given panel because its content just became
// active (a workflow started, a background job was spawned, a task was
// saved, ...) — unless the user has explicitly shown or hidden that panel
// via /panel or its F1-F4 shortcut this session (see panelManualOverride),
// in which case their choice sticks and this is a no-op.
func (m *model) autoOpenPanel(region panelRegion) {
	if m.panelManualOverride[region] {
		return
	}
	switch region {
	case regionMemory:
		m.panelMemory = true
	case regionTasks:
		m.panelTasks = true
	case regionBackground:
		m.panelBackground = true
	case regionWorkflow:
		m.workflowPanelOpen = true
	}
}

// panelOpen reports whether the given region's panel is currently part of
// the layout — the same conditions View() checks before joining it in.
func (m *model) panelOpen(region panelRegion) bool {
	switch region {
	case regionMemory:
		return m.panelMemory
	case regionTasks:
		return m.panelTasks
	case regionBackground:
		return m.panelBackground
	case regionWorkflow:
		return m.workflowPanelVisible()
	}
	return false
}

// panelRenderOrder is the left-to-right order side panels are joined in by
// View() — memory, tasks, background, workflow.
var panelRenderOrder = []panelRegion{regionMemory, regionTasks, regionBackground, regionWorkflow}

// panelAltBackground reports whether region should render with the subtle
// alternating background tint (see withPanelBackground): true for every
// other panel among those *currently open*, counted left-to-right in
// panelRenderOrder — not a fixed per-region parity. Counting only open
// panels means two panels that end up visually adjacent because something
// between them is closed never land on the same background by coincidence.
func (m *model) panelAltBackground(region panelRegion) bool {
	idx := 0
	for _, r := range panelRenderOrder {
		if !m.panelOpen(r) {
			continue
		}
		if r == region {
			return idx%2 == 1
		}
		idx++
	}
	return false
}

// panelAltBackgroundCode returns the raw SGR "set background" escape for the
// alternating panel tint, or "" when not a TTY. Deliberately a small, fixed
// step off pure black/white rather than derived from the terminal's actual
// background (which lipgloss/termenv can't read back) — HasDarkBackground
// only tells us which direction to step.
func panelAltBackgroundCode() string {
	if !isTTY {
		return ""
	}
	if lipgloss.HasDarkBackground() {
		return "\033[48;2;24;24;28m"
	}
	return "\033[48;2;236;236;239m"
}

// withPanelBackground re-applies bg immediately after every full SGR reset
// embedded in line so previously-colored spans (status badges, dim text,
// panel titles, ...) don't cut the background back to the terminal default
// midway through a line — both this file's colorize-based helpers (ansi.go)
// and lipgloss's own Render calls close a styled span with the identical
// "\x1b[0m", so one substitution covers both. The one gap: staleContentColor
// (memory panel only) opens its truecolor spans with a combined reset+set
// code rather than a plain reset, so the background briefly drops out for
// just those tinted characters — accepted rather than special-cased, since
// it's a single existing call site and the effect is barely visible.
func withPanelBackground(line, bg string) string {
	if bg == "" || line == "" {
		return line
	}
	line = strings.ReplaceAll(line, ansiReset, ansiReset+bg)
	return bg + line + ansiReset
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

	bg := ""
	if m.panelAltBackground(region) {
		bg = panelAltBackgroundCode()
	}

	var rows []string
	for _, line := range lines {
		lineW := utf8.RuneCountInString(stripANSI(line))
		if lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, withPanelBackground(line, bg))
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

	bg := ""
	if m.panelAltBackground(region) {
		bg = panelAltBackgroundCode()
	}

	var rows []string
	if !needsBar {
		for range h {
			rows = append(rows, withPanelBackground(" ", bg))
		}
		return strings.Join(rows, "\n")
	}

	thumbTop, thumbBot := scrollThumb(h, total, *m.panelOffsetPtr(region))
	for i := range h {
		if i >= thumbTop && i <= thumbBot {
			rows = append(rows, withPanelBackground(dim("▌"), bg))
		} else {
			rows = append(rows, withPanelBackground(dim("│"), bg))
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
