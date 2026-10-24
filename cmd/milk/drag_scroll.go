package main

import (
	"time"

	"charm.land/bubbles/v2/viewport"
	tea "charm.land/bubbletea/v2"
)

const (
	dragScrollInterval = 50 * time.Millisecond
	// dragScrollMaxIdle bounds how long the tick loop keeps a drag alive at a
	// scroll limit; past it the ordinary dropped-release timeout takes over.
	dragScrollMaxIdle = 60
)

// dragScrollMsg is one autoscroll tick; gen discards ticks from an earlier arming.
type dragScrollMsg struct{ gen uint64 }

func dragScrollCmd(gen uint64) tea.Cmd {
	return tea.Tick(dragScrollInterval, func(time.Time) tea.Msg { return dragScrollMsg{gen: gen} })
}

type dragOwner int

const (
	dragNone dragOwner = iota
	dragAttach
	dragPanel
	dragTranscript
)

// liveDragOwner reports which view owns the mouse interaction that has seen a
// press but no release yet. Presses clear every other view's selection, so at
// most one of panel/transcript holds an anchor (the hidden transcript is
// ignored while the attach view is on screen).
func (m *model) liveDragOwner() dragOwner {
	if !m.dragResetPending {
		return dragNone
	}
	switch {
	case m.attachActive() && m.attachSelAnchorLine >= 0:
		return dragAttach
	case m.panelSelAnchorLine >= 0 && m.panelSelRegion != regionNone:
		return dragPanel
	case !m.attachActive() && m.selAnchorLine >= 0:
		return dragTranscript
	}
	return dragNone
}

// dragEdgeDir returns -1 when screen row y is above the scrollable rows, +1
// when below, 0 when inside.
func (m *model) dragEdgeDir(y int) int {
	const top = 2
	switch {
	case y < top:
		return -1
	case y >= top+m.vp.Height():
		return 1
	}
	return 0
}

func (m *model) stopDragScroll() {
	m.dragScrollDir = 0
	m.dragScrollArmed = false
	m.dragScrollIdle = 0
	m.dragScrollGen++
}

// handleDragEdgeMouse implements autoscroll for a live drag selection: while the
// pointer is above or below the scrollable rows, the owning view scrolls and
// the selection end follows the edge line. Terminals send no repeat events for
// a stationary pointer, so the first outside motion arms a tick loop. A release
// outside the rows (which the per-view handlers ignore) finalizes the drag here.
func (m *model) handleDragEdgeMouse(ev mouseEvent, region panelRegion, regionX int) (tea.Cmd, bool) {
	if ev.Button != tea.MouseLeft {
		return nil, false
	}
	if ev.Action == mousePress {
		m.stopDragScroll()
		return nil, false
	}
	owner := m.liveDragOwner()
	if owner == dragNone {
		m.stopDragScroll()
		return nil, false
	}
	dir := m.dragEdgeDir(ev.Y)
	if ev.Action == mouseRelease {
		m.stopDragScroll()
		if dir == 0 {
			return nil, false
		}
		return m.finalizeDroppedDrag(), true
	}
	if dir == 0 {
		m.dragScrollDir = 0
		return nil, false
	}
	m.dragScrollDir = dir
	switch {
	case owner != dragPanel:
		m.dragScrollX = max(ev.X, 0)
	case region == m.panelSelRegion:
		m.dragScrollX = panelContentCol(region, regionX)
	}
	if m.dragScrollArmed {
		return nil, true
	}
	m.dragScrollArmed = true
	m.dragScrollIdle = 0
	m.dragScrollGen++
	m.dragAutoScrollStep()
	return dragScrollCmd(m.dragScrollGen), true
}

// handleDragScrollTick advances one autoscroll step and re-arms while the
// pointer is still outside the rows.
func (m *model) handleDragScrollTick(gen uint64) tea.Cmd {
	if gen != m.dragScrollGen {
		return nil
	}
	if m.dragScrollDir == 0 || m.liveDragOwner() == dragNone {
		m.dragScrollArmed = false
		return nil
	}
	if m.dragAutoScrollStep() {
		m.dragScrollIdle = 0
	} else if m.dragScrollIdle++; m.dragScrollIdle > dragScrollMaxIdle {
		m.dragScrollArmed = false
		return nil
	}
	return dragScrollCmd(gen)
}

func scrollViewportBy(vp *viewport.Model, dir int) bool {
	before := vp.YOffset()
	if dir < 0 {
		vp.ScrollUp(1)
	} else {
		vp.ScrollDown(1)
	}
	return vp.YOffset() != before
}

// dragAutoScrollStep scrolls the owning view one line toward dragScrollDir and
// moves the selection end to the edge line at the pointer's column. It reports
// whether the view actually scrolled (false at a scroll limit). Like the
// motion handlers, it leaves the selection text to be computed on release.
func (m *model) dragAutoScrollStep() bool {
	dir, col := m.dragScrollDir, max(m.dragScrollX, 0)
	if dir == 0 {
		return false
	}
	edgeLine := func(offset, height, total int) int {
		line := offset
		if dir > 0 {
			line += height - 1
		}
		return max(min(line, total-1), 0)
	}
	switch m.liveDragOwner() {
	case dragAttach:
		vp := &m.attached.vp
		moved := scrollViewportBy(vp, dir)
		m.attachSelDragging = true
		m.attachSelEndLine = edgeLine(vp.YOffset(), vp.Height(), vp.TotalLineCount())
		m.attachSelEndCol = col
		m.syncAttachedContent()
		return moved
	case dragTranscript:
		moved := scrollViewportBy(&m.vp, dir)
		m.selDragging = true
		m.selEndLine = edgeLine(m.vp.YOffset(), m.vp.Height(), m.vp.TotalLineCount())
		m.selEndCol = col
		m.setViewportContent()
		return moved
	case dragPanel:
		region := m.panelSelRegion
		h := m.vp.Height()
		p := m.panelOffsetPtr(region)
		before := *p
		if dir < 0 {
			*p = max(*p-1, 0)
		} else {
			*p = min(*p+1, m.panelMaxOffset(region, h))
		}
		m.panelSelDragging = true
		m.panelSelEndLine = edgeLine(*p, h, len(m.panelSelLines()))
		m.panelSelEndCol = col
		return *p != before
	}
	return false
}

// finalizeDroppedDrag completes a drag whose release never reached its view
// (dropped, or landed outside the scrollable rows): it settles the selection
// text, resets terminal mouse mode to basic tracking (1000), and — with
// copy_on_select — copies.
func (m *model) finalizeDroppedDrag() tea.Cmd {
	m.dragResetPending = false
	m.dragSawOutside = false
	m.stopDragScroll()
	var copyCmd tea.Cmd
	if m.selAnchorLine >= 0 && m.selDragging && m.selText == "" {
		m.selText = m.selectionText()
		if copyCmd = m.copyOnSelectRelease(m.selText); copyCmd != nil {
			m.clearSelection()
		}
	}
	if m.panelSelAnchorLine >= 0 && m.panelSelDragging && m.panelSelText == "" {
		m.panelSelText = selectedText(m.panelSelLines(), m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
		if copyCmd = m.copyOnSelectRelease(m.panelSelText); copyCmd != nil {
			m.clearPanelSelection()
		}
	}
	if m.attachSelAnchorLine >= 0 && m.attachSelDragging && m.attachSelText == "" && m.attached != nil {
		m.attachSelText = m.attachSelectionText()
		if copyCmd = m.copyOnSelectRelease(m.attachSelText); copyCmd != nil {
			m.clearAttachSelection()
			m.syncAttachedContent()
		}
	}
	setMouseDragMode(false)
	m.setViewportContent()
	return copyCmd
}
