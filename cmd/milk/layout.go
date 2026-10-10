package main

import (
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/creack/pty"
)

// viewportRebuildThrottle caps how often a streamed chunk triggers a full
// viewport rebuild (~30fps) — see viewportDirty's doc comment on model.
const viewportRebuildThrottle = 33 * time.Millisecond

// activeViewport returns the viewport currently being displayed in the main
// area: the attach view's own viewport while attached (ADR-0047) and no
// pending prompt needs the user's attention, otherwise the main transcript
// viewport. Used everywhere View()/handleResize()/renderSeparator need to
// act on "whichever viewport is on screen" without each caller re-deriving
// the same check.
//
// The hasPendingPrompt() guard matters live, not just in theory: a
// permission prompt (or any other pending wizard) is printed into the main
// transcript, which the attach view would otherwise be covering — without
// this, the user would see only the attach buffer and the status bar's
// "[allow?]" hint, with no way to read what they're actually being asked.
// Key routing already gives every pending-prompt handler priority over
// handleAttachKey (see Update's tea.KeyMsg case); this keeps what's
// *rendered* consistent with what keys actually do. m.attached itself is
// left untouched — once the prompt resolves, the attach view reappears
// exactly where it was, since nothing here ever calls detachAttach.
func (m *model) activeViewport() *viewport.Model {
	if m.attachActive() {
		return &m.attached.vp
	}
	return &m.vp
}

// attachActive reports whether the attach view currently owns the main area —
// attached and no pending prompt needs the screen (the exact condition
// activeViewport() uses to pick the attach viewport). Kept as its own helper
// so mouse selection can ask "which view is under the cursor?" with the same
// answer rendering gives, without dereferencing a nil attachState.
func (m *model) attachActive() bool {
	return m.attached != nil && !m.hasPendingPrompt()
}

// hasPendingPrompt reports whether some pending prompt/wizard needs the
// user's direct attention right now — the same set of fields Update's
// tea.KeyMsg case checks, in the same "needs a real decision" spirit, kept
// as one place both key routing's implicit precedence (each check simply
// sits before the attach check) and rendering (activeViewport) can agree
// on what counts as "something more urgent than attach is going on."
func (m *model) hasPendingPrompt() bool {
	return m.pendingDirectBash != nil ||
		m.pendingPerm != nil ||
		m.pendingPathPaste != "" ||
		m.pendingForget != nil ||
		m.pendingAdd != nil ||
		m.pendingMCPAdd != nil ||
		m.pendingSwitch != nil ||
		m.pendingTelegramSetup != nil ||
		m.pendingInit != nil ||
		m.pendingWorkflowWizard != nil ||
		m.pendingGenericWorkflowExtend != nil
}

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
	if m.panelBackground {
		w -= backgroundPanelWidth
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
	if m.panelBackground {
		memW += backgroundPanelWidth
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
	m.viewportDirty = false
	m.lastViewportRebuild = time.Now()
}

// syncViewportThrottled is the streaming-chunk path into the viewport:
// appendTranscript/appendThinking call this instead of setViewportContent
// directly. It rebuilds immediately (sticky-bottom preserved) at most once
// per viewportRebuildThrottle window; a call inside that window just marks
// the viewport dirty for the next flushViewportIfDirty (spinnerTickMsg, or
// any other setViewportContent/syncLayout call elsewhere in Update()) to pick
// up — see viewportDirty's doc comment on model for why this never leaves
// streamed text stuck off-screen.
func (m *model) syncViewportThrottled() {
	if !m.ready {
		return
	}
	if time.Since(m.lastViewportRebuild) < viewportRebuildThrottle {
		m.viewportDirty = true
		return
	}
	atBottom := m.vp.AtBottom()
	m.setViewportContent()
	if atBottom {
		m.vp.GotoBottom()
	}
}

// flushViewportIfDirty performs the deferred rebuild syncViewportThrottled
// skipped, if one is still pending. Cheap no-op otherwise.
func (m *model) flushViewportIfDirty() {
	if !m.viewportDirty {
		return
	}
	atBottom := m.vp.AtBottom()
	m.setViewportContent()
	if atBottom {
		m.vp.GotoBottom()
	}
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
	if m.attached != nil {
		m.syncAttachedContent()
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
	vp := m.activeViewport()
	total := vp.TotalLineCount()
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
		thumbTop, thumbBot = scrollThumb(h, total, vp.YOffset)
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
		// Toasts float over the shell too (render-time overlay, no resize).
		mainArea = m.overlayToasts(mainArea)
		return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
	}
	vpH := m.viewportHeight()
	sep := m.renderSeparator(vpH)
	// Attached: show the background job's/workflow's live buffer in place of
	// the main transcript (ADR-0047) — side panels keep rendering normally so
	// the background/workflow panel that triggered the attach stays visible,
	// unlike the PTY-pane branch above, which is a full-screen takeover.
	mainArea := lipgloss.JoinHorizontal(lipgloss.Top, m.activeViewport().View(), sep)
	if m.panelMemory {
		panel := m.renderMemoryPanel(vpH)
		pbar := m.renderPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, panel, pbar)
	}
	if m.panelTasks {
		tpanel := m.renderTasksPanel(vpH)
		tbar := m.renderTasksPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, tpanel, tbar)
	}
	if m.panelBackground {
		bgpanel := m.renderBackgroundPanel(vpH)
		bgbar := m.renderBackgroundPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, bgpanel, bgbar)
	}
	if m.workflowPanelVisible() {
		wpanel := m.renderWorkflowPanel(vpH)
		wbar := m.renderWorkflowPanelScrollbar(vpH)
		mainArea = lipgloss.JoinHorizontal(lipgloss.Top, mainArea, wpanel, wbar)
	}
	// Notification toasts (issue #162) float over the top-right of the whole
	// main area — pure render-time overlay, no layout rows consumed.
	mainArea = m.overlayToasts(mainArea)
	if len(m.tabHints) > 0 {
		return m.headerBar() + "\n" + mainArea + "\n" + strings.Join(m.tabHints, "\n") + "\n" + m.statusBar()
	}
	return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
}
