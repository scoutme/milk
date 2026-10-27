package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/creack/pty"
	viewport "github.com/scoutme/milk/internal/vp"
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
// handleAttachKey (see Update's tea.KeyPressMsg case); this keeps what's
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
// tea.KeyPressMsg case checks, in the same "needs a real decision" spirit, kept
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
		m.pendingTool != nil ||
		m.pendingBgAgent != nil ||
		m.pendingTelegramSetup != nil ||
		m.pendingInit != nil ||
		m.pendingWorkflowWizard != nil ||
		m.pendingGenericWorkflowExtend != nil
}

// hintRegion returns the hint lines rendered between the main area and the
// status bar — one list both View and viewportHeight derive the layout
// from, so the line counts can never disagree. Completion hints win when
// they are up (they are the user's active task); otherwise the idle tip
// line (ADR-0061) fills the slot when enabled. Nothing while the PTY pane
// owns the screen: its layout branch renders no hint rows at all.
//
// The tip line renders at the full window width (m.width, the same budget
// the header/status bars use) — not vpWidth(): the hint rows sit below the
// whole main area, and a side panel block is only viewportHeight() rows
// tall, ending on the row above. Open panels never cover the tip row, so
// they must not narrow it either.
func (m *model) hintRegion() []string {
	if m.ptyPane != nil {
		return nil
	}
	if len(m.tabHints) > 0 {
		return m.tabHints
	}
	if !m.showTips {
		return nil
	}
	return []string{renderTip(m.currentTip(), m.width)}
}

// viewportHeight is the full terminal height minus the chrome lines.
// View() layout: headerBar + "\n" + mainArea + "\n" + statusBar; the "\n" separators don't add lines.
// Chrome heights are measured from the rendered output so growth in either bar automatically reduces
// the viewport rather than pushing the header off-screen.
func (m *model) viewportHeight() int {
	header := strings.Count(m.headerBar(), "\n") + 1
	status := strings.Count(m.statusBar(), "\n") + 1
	h := m.height - header - status - len(m.hintRegion())
	return max(h, 3)
}

// viewportRows returns the screen rows the main viewport occupies: top is the
// first viewport row, bottom the first row below it. Every mouse row→content
// line mapping derives from this one layout measurement — the same one
// viewportHeight() and View() build the layout from — so a header bar that
// wraps (narrow terminal, long session label) or a growing hint region can
// never skew click hit-testing. It used to be hardcoded as "rows 2..height-2",
// which silently mapped every click one row low per extra chrome row: a fold
// header click landed on a later think block (or past the last one, a no-op).
func (m *model) viewportRows() (top, bottom int) {
	top = strings.Count(m.headerBar(), "\n") + 1
	status := strings.Count(m.statusBar(), "\n") + 1
	bottom = m.height - len(m.hintRegion()) - status
	if bottom < top {
		bottom = top
	}
	return top, bottom
}

// mainWidth returns the width available for the transcript+input area.
// When side panels are in the layout it is reduced accordingly, but only when
// the panel would actually be rendered (i.e. terminal is wide enough). On a
// tabbed theme (ADR-0079) at most one panel — the front tab — is in the
// layout, so the shared slot costs one panel width however many are open.
func (m *model) mainWidth() int {
	w := m.width
	for _, r := range panelRenderOrder {
		if m.panelVisible(r) {
			w -= panelWidth(r)
		}
	}
	if w < 20 {
		w = 20
	}
	return w
}

// sidePanelsWidthExcept sums the widths of the side panels sharing the
// layout, excluding exclude — raw open flags with the tabbed front-tab
// filter (panelInLayout), never the width-guarded visibility helpers (those
// call this, so consulting them here would recurse). Used by the
// project/git/workflow visibility guards so a panel is only rendered when a
// usable main area (40 cols) remains.
func (m *model) sidePanelsWidthExcept(exclude panelRegion) int {
	w := 0
	for _, r := range panelRenderOrder {
		if r != exclude && m.panelInLayout(r) {
			w += panelWidth(r)
		}
	}
	return w
}

// projectPanelVisible reports whether the project panel is open and the
// terminal is wide enough to render it alongside a usable main area
// (minimum 40 cols), like workflowPanelVisible.
func (m *model) projectPanelVisible() bool {
	if !m.panelProject {
		return false
	}
	return m.width >= m.sidePanelsWidthExcept(regionProject)+projectPanelWidth+40
}

// gitPanelVisible — the same guard for the git panel.
func (m *model) gitPanelVisible() bool {
	if !m.panelGit {
		return false
	}
	return m.width >= m.sidePanelsWidthExcept(regionGit)+gitPanelWidth+40
}

// workflowPanelVisible reports whether the workflow panel is open and the
// terminal is wide enough to render it alongside a usable main area (minimum
// 40 cols for the transcript) and any other open panels.
func (m *model) workflowPanelVisible() bool {
	if !m.workflowPanelOpen {
		return false
	}
	memW := 0
	if m.panelInLayout(regionMemory) {
		memW = memoryPanelWidth
	}
	if m.panelInLayout(regionTasks) {
		memW += tasksPanelWidth
	}
	if m.panelInLayout(regionBackground) {
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
	if m.vp.Width() != vw {
		m.vp.SetWidth(vw)
		m.colorizeForce = true // width changed — rewrap and re-colorize
	}
	if m.vp.Height() != vpH {
		m.vp.SetHeight(vpH)
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
	// Perf (ADR-0088): the transcript part changes far less often than this
	// runs — it rebuilds on every keystroke — so its rows are cached as a
	// line slice, content-addressed against the wrapped transcript. Typing
	// and cursor movement then only rebuild the small input-area tail and
	// hand the assembled lines to the viewport's split-free SetLines path.
	if transcript != m.txPartCache || sep != m.txSepCache {
		m.txPartCache = transcript
		m.txSepCache = sep
		// Normalize once per rebuild, the way viewport.SetContentLines
		// would — SetLines (the per-keystroke path) trusts its lines.
		m.txLinesCache = viewport.NormalizeLines(strings.Split(transcript+"\n"+sep, "\n"))
	}
	inputLines := viewport.NormalizeLines(strings.Split(m.colorizeInput(m.ta.View()), "\n"))
	lines := make([]string, 0, len(m.txLinesCache)+len(inputLines))
	lines = append(lines, m.txLinesCache...)
	lines = append(lines, inputLines...)
	m.vp.SetLines(lines)
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
		m.vp = viewport.New(viewport.WithWidth(vw), viewport.WithHeight(vpH))
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
		m.vp.SetWidth(vw)
		m.vp.SetHeight(vpH)
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
		thumbTop, thumbBot = scrollThumb(h, total, vp.YOffset())
	}
	for i := range h {
		if scrollable && i >= thumbTop && i <= thumbBot {
			rows = append(rows, scrollThumbGlyph())
		} else {
			rows = append(rows, scrollTrackGlyph())
		}
	}
	return strings.Join(rows, "\n")
}

// View renders the screen. Terminal modes (alt screen, focus reporting, title) are declared here, not
// requested with commands, so the renderer owns their lifecycle.
func (m model) View() tea.View {
	v := tea.NewView(m.viewString())
	v.AltScreen = true
	v.ReportFocus = true
	v.WindowTitle = m.windowTitle()
	return v
}

func (m model) viewString() string {
	if !m.ready {
		return ""
	}
	if th.modern {
		if bg := tkBase.sgrBg(); bg != "" {
			return tintBlock(m.viewBody(), m.width-1, bg) // last column left to the terminal default (same colour via OSC 11): a full-width row can double-advance on ConPTY
		}
	}
	return m.viewBody()
}

func (m model) viewBody() string {
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
	for _, region := range panelRenderOrder {
		if m.panelVisible(region) {
			mainArea = m.joinPanel(mainArea, region, vpH)
		}
	}
	mainArea = m.overlayPanelTransitions(mainArea)
	// Notification toasts (issue #162) float over the top-right of the whole
	// main area — pure render-time overlay, no layout rows consumed.
	mainArea = m.overlayToasts(mainArea)
	mainArea = m.overlayPalette(mainArea)
	if hints := m.hintRegion(); len(hints) > 0 {
		return m.headerBar() + "\n" + mainArea + "\n" + strings.Join(hints, "\n") + "\n" + m.statusBar()
	}
	return m.headerBar() + "\n" + mainArea + "\n" + m.statusBar()
}
