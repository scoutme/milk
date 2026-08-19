package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
)

// viewportHeight is the full terminal height minus the chrome lines.
// View() layout: headerBar + "\n" + mainArea + "\n" + statusBar; the "\n" separators don't add lines.
// Chrome heights are measured from the rendered output so growth in either bar automatically reduces
// the viewport rather than pushing the header off-screen.
func (m *model) viewportHeight() int {
	header := strings.Count(m.headerBar(), "\n") + 1
	status := strings.Count(m.statusBar(), "\n") + 1
	h := m.height - header - status - len(m.tabHints)
	return max(h, 3)
}

// mainWidth returns the width available for the transcript+input area.
// When the memory, tasks, and/or workflow panels are open it is reduced accordingly,
// but only when the panel would actually be rendered (i.e. terminal is wide enough).
func (m *model) mainWidth() int {
	w := m.width
	if m.panelMemory {
		w -= memoryPanelWidth
	}
	if m.panelTasks {
		w -= tasksPanelWidth
	}
	if m.workflowPanelVisible() {
		w -= workflowPanelWidth
	}
	if w < 20 {
		w = 20
	}
	return w
}

// workflowPanelVisible reports whether the workflow panel is open and the
// terminal is wide enough to render it alongside a usable main area (minimum
// 40 cols for the transcript) and any other open panels.
func (m *model) workflowPanelVisible() bool {
	if !m.workflowPanelOpen {
		return false
	}
	memW := 0
	if m.panelMemory {
		memW = memoryPanelWidth
	}
	if m.panelTasks {
		memW += tasksPanelWidth
	}
	return m.width >= memW+workflowPanelWidth+40
}

// vpWidth is the viewport content width: mainWidth minus 1 column reserved for the scrollbar.
func (m *model) vpWidth() int {
	return m.mainWidth() - 1
}

// syncLayout rebuilds viewport content after textarea size changes.
// Sticky-bottom: scrolls to bottom only when already there.
func (m *model) syncLayout() {
	if !m.ready {
		return
	}
	// PTY pane owns the viewport area; no transcript layout needed.
	if m.ptyPane != nil {
		return
	}
	vw := m.vpWidth()
	vpH := m.viewportHeight()
	atBottom := m.vp.AtBottom()
	if m.vp.Width != vw {
		m.vp.Width = vw
		m.colorizeForce = true // width changed — rewrap and re-colorize
	}
	if m.vp.Height != vpH {
		m.vp.Height = vpH
	}
	// The textarea is rendered as content inside the viewport (see
	// setViewportContent), so it must wrap at the same width the viewport
	// itself uses (vw), not mainWidth() — otherwise input lines overflow the
	// viewport's own column budget by exactly the scrollbar column reserved
	// by vpWidth(). This also re-syncs the width whenever a panel opens or
	// closes, even at call sites that only call syncLayout and not
	// refreshPrompt (e.g. auto-opening the workflow panel).
	if m.ta.Width() != vw {
		m.ta.SetWidth(vw)
	}
	m.setViewportContent()
	if atBottom {
		m.vp.GotoBottom()
	}
}

// setViewportContent rebuilds the full viewport content:
// transcript + separator + input area. The input area scrolls with the transcript.
func (m *model) setViewportContent() {
	rows := m.taRows()
	if m.ta.Height() != rows {
		m.ta.SetHeight(rows)
	}
	vw := m.vpWidth()
	sep := styleBorder.Width(vw).Render("")
	transcript := m.wrappedTranscript()
	content := transcript + "\n" + sep + "\n" + m.colorizeInput(m.ta.View())
	m.vp.SetContent(content)
}

func (m model) handleResize(msg tea.WindowSizeMsg) (tea.Model, tea.Cmd) {
	m.width = msg.Width
	m.height = msg.Height

	// Propagate terminal width to local agent for tool hint truncation.
	if m.agents.local != nil {
		m.agents.local.SetTermWidth(msg.Width)
	}

	vw := m.vpWidth()
	vpH := m.viewportHeight()
	if !m.ready {
		m.vp = viewport.New(vw, vpH)
		m.ready = true
		for _, w := range m.startupWarnings {
			m.appendTranscript(yellow("config warning: ") + w + "\n")
		}
		m.startupWarnings = nil
		m.refreshPrompt()
		m.setViewportContent()
		m.vp.GotoBottom()
	} else {
		atBottom := m.vp.AtBottom()
		m.vp.Width = vw
		m.vp.Height = vpH
		m.refreshPrompt()
		m.setViewportContent()
		if atBottom {
			m.vp.GotoBottom()
		}
	}
	// Resize the PTY to match the new terminal dimensions.
	if m.ptyPane != nil {
		newCols := m.mainWidth() - 1
		if newCols < 10 {
			newCols = 10
		}
		pty.Setsize(m.ptyPane.ptm, &pty.Winsize{ //nolint:errcheck
			Rows: uint16(vpH),
			Cols: uint16(newCols),
		})
		m.ptyPane.vtMu.Lock()
		m.ptyPane.vt.Resize(vpH, newCols)
		m.ptyPane.vtMu.Unlock()
	}
	return m, nil
}

// renderSeparator renders the vertical scrollbar / panel divider column.
// Rules:
//   - panel open + scrollable: thumb at proportional position
//   - panel open + not scrollable: full column of │
//   - panel closed + scrollable: thumb at proportional position
//   - panel closed + fits: blank column (no visual noise)
func (m *model) renderSeparator(h int) string {
	total := m.vp.TotalLineCount()
	scrollable := total > h
	visible := m.panelMemory || scrollable

	var rows []string
	if !visible {
		for range h {
			rows = append(rows, " ")
		}
		return strings.Join(rows, "\n")
	}

	var thumbTop, thumbBot int
	if scrollable {
		thumbTop, thumbBot = scrollThumb(h, total, m.vp.YOffset)
	}
	for i := range h {
		if scrollable && i >= thumbTop && i <= thumbBot {
			rows = append(rows, dim("▌"))
		} else {
			rows = append(rows, dim("│"))
		}
	}
	return strings.Join(rows, "\n")
}

func (m model) View() string {
	if !m.ready {
		return ""
	}
	// PTY pane: replace the transcript viewport with the VT terminal screen.
	if m.ptyPane != nil {
		vpH := m.viewportHeight()
		paneCols := m.mainWidth()
		ptyContent := m.renderPTYPane(vpH, paneCols)
		sep := m.renderSeparator(vpH)
		mainArea := lipgloss.JoinHorizontal(lipgloss.Top, ptyContent, sep)
		return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
	}
	vpH := m.viewportHeight()
	sep := m.renderSeparator(vpH)
	mainArea := lipgloss.JoinHorizontal(lipgloss.Top, m.vp.View(), sep)
	if m.panelMemory {
		panel := m.renderMemoryPanel(vpH)
		pbar := m.renderPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, panel, pbar)
	}
	if m.panelTasks {
		tpanel := m.renderTasksPanel(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, tpanel)
	}
	if m.workflowPanelVisible() {
		wpanel := m.renderWorkflowPanel(vpH)
		wbar := m.renderWorkflowPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, wpanel, wbar)
	}
	if len(m.tabHints) > 0 {
		return m.headerBar() + "\n" + mainArea + "\n" + strings.Join(m.tabHints, "\n") + "\n" + m.statusBar()
	}
	return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
}
