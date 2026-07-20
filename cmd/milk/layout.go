package main

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
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
// When the memory and/or workflow panels are open it is reduced accordingly,
// but only when the panel would actually be rendered (i.e. terminal is wide enough).
func (m *model) mainWidth() int {
	w := m.width
	if m.panelMemory {
		w -= memoryPanelWidth
	}
	if m.workflowPanelOpen {
		memW := 0
		if m.panelMemory {
			memW = memoryPanelWidth
		}
		if m.width >= memW+workflowPanelWidth+40 {
			w -= workflowPanelWidth
		}
	}
	if w < 20 {
		w = 20
	}
	return w
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
	vpH := m.viewportHeight()
	sep := m.renderSeparator(vpH)
	mainArea := lipgloss.JoinHorizontal(lipgloss.Top, m.vp.View(), sep)
	if m.panelMemory {
		panel := m.renderMemoryPanel(vpH)
		pbar := m.renderPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, panel, pbar)
	}
	if m.workflowPanelOpen {
		// Suppress the workflow panel when the terminal is too narrow to render it
		// alongside a usable main area (minimum 40 cols for the transcript).
		memW := 0
		if m.panelMemory {
			memW = memoryPanelWidth
		}
		tooNarrow := m.width < memW+workflowPanelWidth+40
		if !tooNarrow {
			wpanel := m.renderWorkflowPanel(vpH)
			mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, wpanel)
		}
	}
	if len(m.tabHints) > 0 {
		return m.headerBar() + "\n" + mainArea + "\n" + strings.Join(m.tabHints, "\n") + "\n" + m.statusBar()
	}
	return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
}
