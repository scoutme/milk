package main

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/agent/local"
)

// Keyboard focus for the side panels (issue #412).
//
// The main area (transcript + input) and each visible side panel compete for
// one keyboard target: PgUp/PgDn (and the ctrl+u/ctrl+f aliases) scroll
// whichever panel holds focus, shift+pgup/shift+pgdown rotate focus through
// [main, visible panels…], a left click sets it — on every side panel only
// once the double-click window passes (single = focus, double = the panel's
// action, which never moves focus; see panelClickAction) — and focus renders as state —
// a focus tint on the panel's rows, the input area dimmed while focus is
// elsewhere (#367's render path). The focused panel also carries a row
// cursor: arrows walk it (renderSidePanel paints it as the selection
// highlight), Enter runs the cursor row's action — the double-click action,
// rows with items only (panelActivateRow) — and typing any text hands focus
// straight back to the input, keystroke included. Focus is transient view
// state: never persisted, never toasts, never moves layout.

// focusedPanel returns the side panel that currently owns keyboard
// navigation, or regionNone for the main area. It self-heals: a focused
// panel that has been closed or pushed out of the width budget no longer
// counts, so every consumer — the dimmed input, the tint, the scroll
// routing — falls back to the main area without needing a reset at each
// mutation site (closing, /new, budget changes, theme switches).
func (m *model) focusedPanel() panelRegion {
	if m.panelFocus != regionNone && m.panelVisible(m.panelFocus) {
		return m.panelFocus
	}
	return regionNone
}

// rotatePanelFocus moves keyboard focus one step through
// [main, visible panels…] in panelRenderOrder, wrapping at both ends.
// Main is deliberately part of the cycle: rotation is the only keyboard
// path back to the input area — and shift+pgup the way back out of it.
// Panels hidden by the width budget are not in the sequence (only what is
// actually rendered may be focused, ADR-0089), so with no visible panel the
// cycle is main→main: a no-op, not a panic.
func (m *model) rotatePanelFocus(dir int) {
	seq := []panelRegion{regionNone}
	for _, r := range panelRenderOrder {
		if m.panelVisible(r) {
			seq = append(seq, r)
		}
	}
	cur := m.focusedPanel()
	idx := 0
	for i, r := range seq {
		if r == cur {
			idx = i
			break
		}
	}
	m.panelFocus = seq[((idx+dir)%len(seq)+len(seq))%len(seq)]
	if r := m.panelFocus; r != regionNone {
		// The cursor lands on the top visible row — a rotation is a
		// glance, not a click; a click's own row lands via
		// flushPanelClickPending/panelClickFocusMsg instead.
		all, _ := m.sidePanelLines(r)
		m.panelCursorLine = min(m.panelScrollOffset(r), max(len(all)-1, 0))
	}
}

// scrollFocusedPanel pages the focused panel's own scroll offset — the same
// per-region offset the wheel and drag use — by half the panel height, the
// keyboard twin of the transcript's PgUp/PgDn. Clamped exactly like the
// wheel: [0, panelMaxOffset]. A main-focused caller reaches here only via
// focusedPanel()==regionNone guards, and this double-checks anyway.
func (m *model) scrollFocusedPanel(dir int) {
	region := m.focusedPanel()
	if region == regionNone {
		return
	}
	p := m.panelOffsetPtr(region)
	if p == nil {
		return
	}
	h := m.viewportHeight()
	step := max(h/2, 1)
	switch {
	case dir < 0:
		*p = max(*p-step, 0)
	case dir > 0:
		*p = min(*p+step, m.panelMaxOffset(region, h))
	}
	m.clampPanelCursorToView(region)
}

// --- keyboard cursor: the focused panel's row cursor (arrows, Enter) ---

// movePanelCursor moves the focused panel's row cursor by dir, clamped to
// the content, scrolling the panel just enough to keep the row visible —
// arrows walk rows, the keyboard twin of clicking one.
func (m *model) movePanelCursor(region panelRegion, dir int) {
	all, _ := m.sidePanelLines(region)
	if len(all) == 0 {
		return
	}
	m.panelCursorLine = min(max(m.panelCursorLine+dir, 0), len(all)-1)
	m.scrollCursorIntoView(region)
}

// scrollCursorIntoView adjusts the region's offset minimally so the cursor
// row sits inside the visible window (arrow navigation).
func (m *model) scrollCursorIntoView(region panelRegion) {
	p := m.panelOffsetPtr(region)
	if p == nil {
		return
	}
	h := m.viewportHeight()
	switch {
	case m.panelCursorLine < *p:
		*p = m.panelCursorLine
	case m.panelCursorLine >= *p+h:
		*p = m.panelCursorLine - h + 1
	}
	*p = min(*p, m.panelMaxOffset(region, h))
}

// clampPanelCursorToView is scrollCursorIntoView's reverse: after the
// window moved (wheel, paging) the cursor follows to the nearest edge —
// a cursor scrolled off-screen is invisible state.
func (m *model) clampPanelCursorToView(region panelRegion) {
	p := m.panelOffsetPtr(region)
	if p == nil {
		return
	}
	all, _ := m.sidePanelLines(region)
	if len(all) == 0 {
		return
	}
	h := m.viewportHeight()
	m.panelCursorLine = min(max(m.panelCursorLine, *p), min(*p+h-1, len(all)-1))
}

// panelRowBg is the row background shared by a side panel's body and its
// scrollbar: the focus tint wins over the alternating parity (a focused
// panel must read as focused wherever it landed), which wins over plain.
func (m *model) panelRowBg(region panelRegion) string {
	if m.focusedPanel() == region {
		return focusBgCode()
	}
	if m.panelAltBackground(region) {
		return panelAltBackgroundCode()
	}
	return ""
}

// --- click cadence: every side panel (single = deferred focus, double = the panel's action) ---

// panelDoubleClickWindow is the cadence shared by every side panel's click
// sequence: a second click within it runs the panel's action (and abandons
// the first click's deferred focus — a double click never moves focus), a
// third on a project file row inserts an @-mention — the gesture the double
// click used to own, pushed one click along. Same 400ms as the ctrl+t chord
// window: one cadence across the TUI.
const panelDoubleClickWindow = 400 * time.Millisecond

// panelClickFocusMsg fires when the double-click window passes with no
// second click: the deferred single click focuses its panel now.
type panelClickFocusMsg struct{ gen uint64 }

// pendingPanelClick is the deferred single click. gen orphans the timer
// whenever the click is superseded — a press in the main area, a double
// click, a key — so a stale timer can never move focus after the moment
// has passed. (A drag never arms a click at all, so it needs no orphaning.)
type pendingPanelClick struct {
	region panelRegion
	line   int
	gen    uint64
	at     time.Time
	valid  bool
}

// cancelPendingPanelClick abandons the deferred single click without
// applying focus.
func (m *model) cancelPendingPanelClick() {
	if m.panelClickPending.valid {
		m.panelClickPending.valid = false
		m.panelClickGen++
	}
}

// flushPanelClickPending applies the deferred focus now; key routing calls
// it first: any key means the user moved on, and the key must act on the
// panel they just clicked rather than the previous focus.
func (m *model) flushPanelClickPending() {
	if m.panelClickPending.valid {
		m.panelFocus = m.panelClickPending.region
		m.panelCursorLine = m.panelClickPending.line
		m.panelClickPending.valid = false
		m.panelClickGen++
	}
}

// projectRowID returns the project tree's row id ("" when out of range).
func (m *model) projectRowID(lineIdx int) string {
	if lineIdx < 0 || lineIdx >= len(m.projectIDs) {
		return ""
	}
	return m.projectIDs[lineIdx]
}

// panelClickAction is THE click state machine for every side panel, run on
// a click's release — no panel's press sets focus (repl.go's mouse branch),
// so this decides it, uniformly:
//
//   - single: the panel's focus lands deferred — when the window passes
//     (the tick) or immediately when a key flushes it. No action fires:
//     one click is focus, never content — browsing must not dump previews
//     or expand rows off a stray click.
//   - double: the panel's own action, and the deferred focus is abandoned —
//     a double click never moves focus. Actions: memory/tasks expand in
//     place, background/workflow attach their live buffer, project toggles
//     a directory row or previews a file (diff/contents), git previews the
//     commit or the working-tree header. The row index a pending click
//     recorded is stable across every action (attach views don't rebuild
//     the trees; a dir's toggled row keeps its index — only rows below
//     move).
//   - triple (project file rows): the @-mention.
//
// The main area (regionNone) focuses immediately at press (repl.go) — it
// has no double-click action to protect. The click it judges must BE one:
// press and release close together (panel_select.go gates the branch on
// panelPressAt — holding the button down is not a normal single click),
// and a drag (text selection) never reaches this machine at all — movement
// is not a click, so selecting text never changes focus.
func (m *model) panelClickAction(region panelRegion, lineIdx int) tea.Cmd {
	now := time.Now()

	// click3: an @-mention, within the window after the previewing click2.
	if region == regionProject && m.lastTreeClickPath != "" &&
		now.Sub(m.lastTreeClickTime) <= panelDoubleClickWindow {
		if id := m.projectRowID(lineIdx); id != "" && id == m.lastTreeClickPath &&
			!strings.HasPrefix(id, "d:") {
			m.lastTreeClickPath = ""
			m.cancelPendingPanelClick()
			m.ta.InsertString("@" + id)
			m.syncLayout()
			return nil
		}
	}

	// click2: same region + line inside the window — the acting click.
	if p := &m.panelClickPending; p.valid && p.region == region && p.line == lineIdx &&
		now.Sub(p.at) <= panelDoubleClickWindow {
		m.cancelPendingPanelClick() // abandons the deferred focus
		// A project file row's double click arms the triple-click
		// @-mention — a click-only gesture (keyboard activation never
		// arms it).
		if region == regionProject {
			// The changes-summary row is a preview target, not a path — it
			// must never be recorded for the triple-click @-mention dance.
			if id := m.projectRowID(lineIdx); id != "" && !strings.HasPrefix(id, "d:") && id != changesRowID {
				m.lastTreeClickPath = id
				m.lastTreeClickTime = now
			}
		}
		cmd, _ := m.panelActivateRow(region, lineIdx)
		return cmd
	}

	// click1: defer focus to the window; nothing else — every panel's
	// action belongs to the double click.
	m.cancelPendingPanelClick() // superseded: another row/region, or a stale window
	m.panelClickGen++
	m.panelClickPending = pendingPanelClick{
		region: region, line: lineIdx, gen: m.panelClickGen, at: now, valid: true,
	}
	gen := m.panelClickGen
	return tea.Tick(panelDoubleClickWindow, func(time.Time) tea.Msg {
		return panelClickFocusMsg{gen: gen}
	})
}

// panelActivateRow runs a row's action — exactly what a double click on it
// runs; Enter is the keyboard twin, on the focused panel's cursor row. The
// bool is the "rows with items" gate: false for header/blank/out-of-range
// rows, where nothing ran and Enter must do nothing.
func (m *model) panelActivateRow(region panelRegion, lineIdx int) (tea.Cmd, bool) {
	switch region {
	case regionMemory:
		_, ids := memoryPanelRows(m.mem, memoryPanelInner, m.currentSessionBricks(), m.memoryExpanded)
		if lineIdx < 0 || lineIdx >= len(ids) || ids[lineIdx] == "" {
			return nil, false
		}
		m.handleMemoryPanelClick(lineIdx)
		return nil, true
	case regionTasks:
		_, ids := taskPanelRows(m.taskStore, tasksPanelInner, m.taskExpanded)
		if lineIdx < 0 || lineIdx >= len(ids) || ids[lineIdx] == "" {
			return nil, false
		}
		m.handleTasksPanelClick(lineIdx)
		return nil, true
	case regionBackground:
		var jobs []local.Job
		if m.agents.backgroundMgr != nil {
			jobs = m.agents.backgroundMgr.Jobs()
		}
		rows := backgroundPanelLayout(jobs, local.BackgroundShells(), backgroundPanelInner)
		if lineIdx < 0 || lineIdx >= len(rows) {
			return nil, false
		}
		if rows[lineIdx].kind != bgRowJob && rows[lineIdx].kind != bgRowShell {
			return nil, false
		}
		return m.handleBackgroundPanelClick(lineIdx), true
	case regionWorkflow:
		if m.workflowState == nil {
			return nil, false
		}
		return m.handleWorkflowPanelClick(), true
	case regionProject:
		if m.projectRowID(lineIdx) == "" {
			return nil, false
		}
		return m.handleProjectPanelClick(lineIdx), true
	case regionGit:
		if lineIdx < 0 || lineIdx >= len(m.gitIDs) || m.gitIDs[lineIdx] == "" {
			return nil, false
		}
		return m.handleGitPanelClick(lineIdx), true
	}
	return nil, false
}
