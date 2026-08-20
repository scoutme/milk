package main

import (
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
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
	regionWorkflow
)

// regionAt maps an absolute terminal column to the panel region it falls in
// (regionNone for the main viewport) and the column offset within that
// region's own rendered width, in the same left-to-right order the panels are
// joined in View(): viewport, memory, tasks, workflow.
func (m *model) regionAt(x int) (panelRegion, int) {
	off := m.mainWidth()
	if x < off {
		return regionNone, x
	}
	if m.panelMemory {
		if x < off+memoryPanelWidth {
			return regionMemory, x - off
		}
		off += memoryPanelWidth
	}
	if m.panelTasks {
		if x < off+tasksPanelWidth {
			return regionTasks, x - off
		}
		off += tasksPanelWidth
	}
	if m.workflowPanelVisible() {
		if x < off+workflowPanelWidth {
			return regionWorkflow, x - off
		}
	}
	return regionNone, x
}

// panelScrollOffset returns the current scroll offset for a panel region.
func (m *model) panelScrollOffset(region panelRegion) int {
	switch region {
	case regionMemory:
		return m.panelOffset
	case regionWorkflow:
		return m.workflowPanelOffset
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
	var total int
	switch region {
	case regionMemory:
		total = len(buildPanelLines(m.mem, memoryPanelInner, m.currentSessionBricks()))
	case regionTasks:
		total = len(buildTasksPanelLines(m.taskStore, tasksPanelInner))
	case regionWorkflow:
		total = len(buildWorkflowPanelLines(m.workflowState, workflowPanelContentWidth-2))
	default:
		return 0
	}
	return max(total-h, 0)
}

// panelContentCol maps a column offset within a panel's rendered width
// (regionX, 0-based from the panel's leftmost screen column) to the column
// index into that panel's plain content lines. The workflow panel reserves
// its first 2 columns for a left border + padding; the memory panel has none.
func panelContentCol(region panelRegion, regionX int) int {
	if region == regionWorkflow {
		regionX -= 2
	}
	if regionX < 0 {
		regionX = 0
	}
	return regionX
}

// panelSelLines returns the full (unwindowed) content lines for the panel
// region that currently owns the active panel selection, matching the line
// indices used by panelSelAnchorLine/panelSelEndLine.
func (m *model) panelSelLines() []string {
	switch m.panelSelRegion {
	case regionMemory:
		return buildPanelLines(m.mem, memoryPanelInner, m.currentSessionBricks())
	case regionWorkflow:
		return buildWorkflowPanelLines(m.workflowState, workflowPanelContentWidth-2)
	}
	return nil
}

// handlePanelMouse handles left-button mouse events over the memory or
// workflow side panels: a click that starts and ends on the same cell runs
// the memory panel's percept/brick detail lookup (double-click for details);
// any drag selects panel text for copying, scoped to that panel's own lines
// so the selection never crosses into the transcript or another panel.
func (m *model) handlePanelMouse(region panelRegion, regionX int, ev tea.MouseEvent) (tea.Model, tea.Cmd) {
	const panelRowStart = 2 // same header offset as the main viewport
	lineIdx := m.panelScrollOffset(region) + (ev.Y - panelRowStart)
	col := panelContentCol(region, regionX)

	switch ev.Action {
	case tea.MouseActionPress:
		if ev.Ctrl && m.panelSelRegion == region && m.panelSelAnchorLine >= 0 {
			// Extend (or shrink) the existing selection to the clicked position,
			// mirroring the transcript viewport's ctrl+click behavior.
			m.panelSelEndLine = lineIdx
			m.panelSelEndCol = col
			m.panelSelDragging = true
			m.panelSelText = panelSelectionText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
			setMouseDragMode(true)
			break
		}
		m.clearSelection()
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
	case tea.MouseActionMotion:
		if m.panelSelRegion == region && m.panelSelAnchorLine >= 0 {
			m.panelSelDragging = true
			m.panelSelEndLine = lineIdx
			m.panelSelEndCol = col
		}
	case tea.MouseActionRelease:
		setMouseDragMode(false)
		if m.panelSelRegion != region || m.panelSelAnchorLine < 0 {
			break
		}
		if !m.panelSelDragging || (lineIdx == m.panelSelAnchorLine && col == m.panelSelAnchorCol) {
			// A click, not a drag: clear the zero-length selection and, for the
			// memory panel, run the existing percept/brick detail lookup.
			m.clearPanelSelection()
			if region == regionMemory {
				m.handleMemoryPanelClick(lineIdx)
			}
			break
		}
		m.panelSelEndLine = lineIdx
		m.panelSelEndCol = col
		m.panelSelText = panelSelectionText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
	}
	return m, nil
}

// handleMemoryPanelClick runs the memory panel's existing click-for-detail
// behavior: a single click arms the line's ID, and a second click on the same
// ID within 400ms prints the full percept/brick content to the transcript.
func (m *model) handleMemoryPanelClick(lineIdx int) {
	ids := buildPanelLineIDs(m.mem, m.currentSessionBricks())
	if lineIdx < 0 || lineIdx >= len(ids) {
		return
	}
	id := ids[lineIdx]
	if id == "" {
		return
	}
	now := time.Now()
	if id == m.lastPanelClickID && now.Sub(m.lastPanelClickTime) <= 400*time.Millisecond {
		bricks := m.currentSessionBricks()
		var result string
		if content := brickContent(id, bricks); content != "" {
			result = milkTag() + " [" + id + "]\n" + content
		} else {
			result = execMemoryShow("#"+id[:min(6, len(id))], m.st)
		}
		m.appendTranscript(result + "\n")
		m.vp.GotoBottom()
		m.lastPanelClickID = ""
	} else {
		m.lastPanelClickID = id
		m.lastPanelClickTime = now
	}
}

// clearPanelSelection resets the memory/workflow panel selection state.
func (m *model) clearPanelSelection() {
	m.panelSelRegion = regionNone
	m.panelSelAnchorLine = -1
	m.panelSelAnchorCol = 0
	m.panelSelEndLine = -1
	m.panelSelEndCol = 0
	m.panelSelDragging = false
	m.panelSelText = ""
}

// panelSelectionText extracts the plain text between (anchorLine,anchorCol)
// and (endLine,endCol) from a pre-built slice of (possibly ANSI-colored)
// panel content lines, respecting column boundaries on the first and last
// line. Mirrors transcript.go's selectionText but for panel line slices
// rather than the transcript viewport.
func panelSelectionText(lines []string, anchorLine, anchorCol, endLine, endCol int) string {
	loLine, loCol := anchorLine, anchorCol
	hiLine, hiCol := endLine, endCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	if loLine < 0 {
		loLine = 0
	}
	if hiLine >= len(lines) {
		hiLine = len(lines) - 1
	}
	if hiLine < loLine {
		return ""
	}
	var sb strings.Builder
	for i := loLine; i <= hiLine; i++ {
		plain := []rune(ansi.Strip(lines[i]))
		start, end := 0, len(plain)
		if i == loLine {
			if loCol < len(plain) {
				start = loCol
			} else {
				start = len(plain)
			}
		}
		if i == hiLine {
			if hiCol < len(plain) {
				end = hiCol
			}
		}
		if start > end {
			start = end
		}
		sb.WriteString(string(plain[start:end]))
		if i < hiLine {
			sb.WriteByte('\n')
		}
	}
	return sb.String()
}

// applyPanelSelectionHighlight returns a copy of lines with reverse-video
// applied to the selected range, mirroring transcript.go's
// applySelectionHighlight but operating on a []string of panel content lines.
// Returns lines unchanged when no selection is active (anchorLine/endLine < 0).
func applyPanelSelectionHighlight(lines []string, anchorLine, anchorCol, endLine, endCol int) []string {
	if anchorLine < 0 || endLine < 0 {
		return lines
	}
	loLine, loCol := anchorLine, anchorCol
	hiLine, hiCol := endLine, endCol
	if hiLine < loLine || (hiLine == loLine && hiCol < loCol) {
		loLine, loCol, hiLine, hiCol = hiLine, hiCol, loLine, loCol
	}
	out := make([]string, len(lines))
	copy(out, lines)
	selStyle := lipgloss.NewStyle().Reverse(true)
	for i := range out {
		if i < loLine || i > hiLine {
			continue
		}
		plain := []rune(ansi.Strip(out[i]))
		start, end := 0, len(plain)
		if i == loLine {
			if loCol < len(plain) {
				start = loCol
			} else {
				start = len(plain)
			}
		}
		if i == hiLine {
			if hiCol < len(plain) {
				end = hiCol
			}
		}
		if start > end {
			start = end
		}
		before := string(plain[:start])
		sel := selStyle.Render(string(plain[start:end]))
		after := string(plain[end:])
		out[i] = before + sel + after
	}
	return out
}
