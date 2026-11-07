package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/sgr"
)

// panelRegion identifies which side panel (if any) a screen column falls in.
// Mouse handling routes wheel/click/drag events to the right panel by column
// range so that scrolling and text selection stay scoped to a single panel
// instead of bleeding across panel boundaries.
type panelRegion int

const (
	regionNone panelRegion = iota
	regionMemory
	regionTasks
	regionBackground
	regionWorkflow
	regionProject
	regionGit
)

// regionAt maps an absolute terminal column to the panel region it falls in
// (regionNone for the main viewport) and the column offset within that
// region's own rendered width, walking the panels in the same left-to-right
// order View() joins them in (panelRenderOrder) — and the same visibility
// test (panelVisible), so drawn columns and hit targets can never drift.
func (m *model) regionAt(x int) (panelRegion, int) {
	off := m.mainWidth()
	if x < off {
		return regionNone, x
	}
	for _, r := range panelRenderOrder {
		if !m.panelVisible(r) {
			continue
		}
		if x < off+panelWidth(r) {
			return r, x - off
		}
		off += panelWidth(r)
	}
	return regionNone, x
}

// panelScrollOffset returns the current scroll offset for a panel region.
func (m *model) panelScrollOffset(region panelRegion) int {
	if p := m.panelOffsetPtr(region); p != nil {
		return *p
	}
	return 0
}

// panelMaxOffset returns the largest scroll offset that still shows a full
// screenful of content for the given region, at rendered height h. Mirrors
// the clamp each render*Panel function applies to its own (copy-local) offset —
// callers that mutate the persisted offset (e.g. handleMouse, via Update's
// value-receiver model) must clamp against this themselves, since View() runs
// on a throwaway copy and any clamping done there never reaches real state.
func (m *model) panelMaxOffset(region panelRegion, h int) int {
	all, _ := m.sidePanelLines(region)
	return max(len(all)-h, 0)
}

// panelContentCol maps a column offset within a panel's rendered width
// (regionX, 0-based from the panel's leftmost screen column) to the column
// index into that panel's plain content lines. No panel reserves any
// left-side columns (border/padding lives, if at all, on the right as the
// scrollbar column, which is not part of any region's own width), so this is
// currently just a defensive clamp.
func panelContentCol(_ panelRegion, regionX int) int {
	if regionX < 0 {
		regionX = 0
	}
	return regionX
}

// panelSelLines returns the full (unwindowed) content lines for the panel
// region that currently owns the active panel selection, matching the line
// indices used by panelSelAnchorLine/panelSelEndLine.
func (m *model) panelSelLines() []string {
	lines, _ := m.sidePanelLines(m.panelSelRegion)
	return lines
}

// handlePanelMouse handles left-button mouse events over any side panel: a
// click that starts and ends on the same cell runs the region's own click
// behavior (memory/tasks expand content in place on double-click; a no-op for
// the project and git panels on a plain click); any drag selects panel text
// for copying, scoped to that panel's own lines so the selection never crosses
// into the transcript or another panel.
func (m *model) handlePanelMouse(region panelRegion, regionX int, ev mouseEvent) (tea.Model, tea.Cmd) {
	// Same measured chrome as the main viewport (viewportRows): the panel
	// column starts below the header bar's rendered rows, never a hardcoded 2.
	panelRowStart, _ := m.viewportRows()
	lineIdx := m.panelScrollOffset(region) + (ev.Y - panelRowStart)
	col := panelContentCol(region, regionX)
	var dragCmd tea.Cmd // set by press/motion; returned at the end

	switch ev.Action {
	case mousePress:
		// Stamp the press: the release judges whether this pair is a click
		// at all (press→release within panelDoubleClickWindow — holding the
		// button down is not a normal single click). Lives here, not in
		// repl.go's dispatch, so direct handlePanelMouse sequences (tests,
		// future callers) are judged identically.
		m.panelPressAt = time.Now()
		if ev.Ctrl && m.panelSelRegion == region && m.panelSelAnchorLine >= 0 {
			// Extend (or shrink) the existing selection to the clicked position,
			// mirroring the transcript viewport's ctrl+click behavior.
			m.panelSelEndLine = lineIdx
			m.panelSelEndCol = col
			m.panelSelDragging = true
			m.panelSelText = selectedText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
			setMouseDragMode(true)
			m.dragResetPending = true
			m.dragResetGen++
			dragCmd = dragResetCmd(m.dragResetGen)
			break
		}
		m.clearSelection()
		if m.attached != nil && m.attachSelAnchorLine >= 0 {
			// A panel click ends an attach-view selection too — both are on
			// screen while attached, and only one selection may be live.
			m.clearAttachSelection()
			m.syncAttachedContent()
		}
		if m.panelSelRegion != region {
			m.clearPanelSelection()
		}
		m.panelSelRegion = region
		m.panelSelAnchorLine = lineIdx
		m.panelSelAnchorCol = col
		m.panelSelEndLine = -1
		m.panelSelEndCol = 0
		m.panelSelDragging = false
		m.panelSelText = ""
		setMouseDragMode(true)
		m.dragResetPending = true
		m.dragResetGen++
		dragCmd = dragResetCmd(m.dragResetGen)
	case mouseMotion:
		if m.panelSelRegion == region && m.panelSelAnchorLine >= 0 {
			m.panelSelDragging = true
			m.panelSelEndLine = lineIdx
			m.panelSelEndCol = col
			m.dragResetGen++
			dragCmd = dragResetCmd(m.dragResetGen) // reschedule: release hasn't arrived yet
		}
	case mouseRelease:
		m.dragResetPending = false
		setMouseDragMode(false)
		if m.panelSelRegion != region || m.panelSelAnchorLine < 0 {
			break
		}
		if !m.panelSelDragging || (lineIdx == m.panelSelAnchorLine && col == m.panelSelAnchorCol) {
			// A click, not a drag: clear the zero-length selection and run
			// the region's own click behavior — the memory panel's existing
			// percept/brick detail lookup (prints to the transcript), or for
			// background/workflow, attach to that job's/the workflow's live
			// buffer (ADR-0047) instead of printing.
			m.clearPanelSelection()
			// Press and release must be close together for this to count
			// as a click at all: holding the button down for long enough
			// is not a normal single click, so it neither focuses nor
			// acts.
			if time.Since(m.panelPressAt) <= panelDoubleClickWindow {
				// Overflow tabs (issue #373, every theme per ADR-0098): the
				// group column — the rightmost one — carries the strip of the
				// grouped panels. Clicking a grouped tab moves that panel to
				// the top of its group (stamp, never toggled closed); a click
				// never closes anything (see handleBudgetTabClick).
				if ev.Y == panelRowStart && lineIdx == 0 {
					if tab := budgetTabAt(m.budgetTabGroupFor(region), panelWidth(region)-1, col); tab != regionNone {
						dragCmd = m.handleBudgetTabClick(tab)
						break
					}
				}
				// Every panel routes through the one click cadence
				// (panelClickAction): single = deferred focus, double =
				// the panel's action without ever moving focus, triple =
				// @-mention (project file rows).
				dragCmd = m.panelClickAction(region, lineIdx)
			}
			break
		}
		m.panelSelEndLine = lineIdx
		m.panelSelEndCol = col
		// A real drag (selection) in any panel changes NOTHING focus-wise:
		// it is not a click at all — press and release are not consistent
		// (the movement IS the gesture), and holding the button down is
		// judged at click-branch time anyway. Selecting text must never
		// move the keyboard target; only a genuine, close-in-time click
		// defers focus (panelClickAction), and only its double acts.
		m.panelSelText = selectedText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
		if cmd := m.copyOnSelectRelease(m.panelSelText); cmd != nil {
			m.clearPanelSelection()
			dragCmd = cmd
		}
	}
	return m, dragCmd
}

// handleBudgetTabClick runs a click on a width-budget overflow tab (issue
// #373, every theme per ADR-0098) — and it only ever moves a panel to the
// top of its group: on a grouped panel that is not on top, exactly the
// F-key / /panel <name> bring-to-front — its recency stamp makes it the
// group's freshest, so the group column shows it ("between the grouped
// ones, the fresher one is on top"). The sets themselves never move (the
// grouped ones are always the rightmost open ones) and nothing is ever
// toggled closed (ADR-0089's rule). On the top's own label — the panel
// already on screen in the group column — a deliberate no-op: clicking what
// you are pointing at must not move or close it; closing stays with the
// F-key / /panel press, which handlePanelCmdToggle still toggles.
func (m *model) handleBudgetTabClick(region panelRegion) tea.Cmd {
	if m.panelVisible(region) {
		return nil // on-screen tab: forward-only navigation, never a toggle
	}
	res, cmd := m.handlePanelCmd(panelSubName(region))
	if mm, ok := res.(model); ok {
		*m = mm
	}
	return cmd
}

// handleMemoryPanelClick runs the memory panel's action — double-click
// expands or collapses the content in place. The cadence around it (single
// = deferred focus, this toggle on the double click without moving focus)
// lives in panelClickAction: handlers are pure actions.
func (m *model) handleMemoryPanelClick(lineIdx int) {
	_, ids := memoryPanelRows(m.mem, memoryPanelInner, m.currentSessionBricks(), m.memoryExpanded)
	if lineIdx < 0 || lineIdx >= len(ids) {
		return
	}
	id := ids[lineIdx]
	if id == "" {
		return
	}
	if m.memoryExpanded == id {
		m.memoryExpanded = ""
	} else {
		m.memoryExpanded = id
	}
}

// handleTasksPanelClick runs the tasks panel's action: double-click
// expands or collapses the task title in place (memory-panel semantics).
// Pure action — the cadence (single = deferred focus, double = toggle
// without moving focus) lives in panelClickAction.
func (m *model) handleTasksPanelClick(lineIdx int) {
	_, ids := taskPanelRows(m.taskStore, tasksPanelInner, m.taskExpanded)
	if lineIdx < 0 || lineIdx >= len(ids) {
		return
	}
	id := ids[lineIdx]
	if id == "" {
		return
	}
	if m.taskExpanded == id {
		m.taskExpanded = ""
	} else {
		m.taskExpanded = id
	}
}

// clearPanelSelection resets the side-panel selection state.
func (m *model) clearPanelSelection() {
	m.panelSelRegion = regionNone
	m.panelSelAnchorLine = -1
	m.panelSelAnchorCol = 0
	m.panelSelEndLine = -1
	m.panelSelEndCol = 0
	m.panelSelDragging = false
	m.panelSelText = ""
}

// orderedSelection returns a selection's endpoints ordered start before end,
// whichever way the user dragged.
func orderedSelection(anchorLine, anchorCol, endLine, endCol int) (loLine, loCol, hiLine, hiCol int) {
	if endLine < anchorLine || (endLine == anchorLine && endCol < anchorCol) {
		return endLine, endCol, anchorLine, anchorCol
	}
	return anchorLine, anchorCol, endLine, endCol
}

// selectionSpan returns the [start,end) range of row i's visible runes (n of
// them) covered by a selection between the ordered endpoints: the first row
// starts at loCol, the last ends at hiCol, rows in between are taken whole.
func selectionSpan(i, loLine, loCol, hiLine, hiCol, n int) (start, end int) {
	start, end = 0, n
	if i == loLine {
		start = min(loCol, n)
	}
	if i == hiLine && hiCol < n {
		end = hiCol
	}
	return min(start, end), end
}

// selectedText extracts the plain text between (anchorLine,anchorCol) and
// (endLine,endCol) from a slice of (possibly ANSI-styled) content lines,
// respecting column boundaries on the first and last line. Coordinates are
// visible-rune offsets, so the transcript, the side panels and the attach view
// all share it.
func selectedText(lines []string, anchorLine, anchorCol, endLine, endCol int) string {
	loLine, loCol, hiLine, hiCol := orderedSelection(anchorLine, anchorCol, endLine, endCol)
	loLine = max(loLine, 0)
	hiLine = min(hiLine, len(lines)-1)
	var sb strings.Builder
	for i := loLine; i <= hiLine; i++ {
		plain := []rune(ansi.Strip(lines[i]))
		start, end := selectionSpan(i, loLine, loCol, hiLine, hiCol, len(plain))
		sb.WriteString(string(plain[start:end]))
		if i < hiLine {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// Reverse video marks a selection; it is switched off again with 27 rather
// than a full reset so the row's own styling survives around it.
const (
	selectionOn  = "\x1b[7m"
	selectionOff = "\x1b[27m"
)

// highlightSelection returns a copy of lines with the selected range in
// reverse video, leaving every row's own styling (colours, dim, the think and
// diff backgrounds) intact underneath. Returns lines unchanged when no
// selection is active (anchorLine/endLine < 0).
func highlightSelection(lines []string, anchorLine, anchorCol, endLine, endCol int) []string {
	if anchorLine < 0 || endLine < 0 {
		return lines
	}
	loLine, loCol, hiLine, hiCol := orderedSelection(anchorLine, anchorCol, endLine, endCol)
	out := make([]string, len(lines))
	copy(out, lines)
	for i := max(loLine, 0); i <= hiLine && i < len(out); i++ {
		start, end := selectionSpan(i, loLine, loCol, hiLine, hiCol, len([]rune(ansi.Strip(out[i]))))
		out[i] = sgr.Highlight(out[i], start, end, selectionOn, selectionOff)
	}
	return out
}
