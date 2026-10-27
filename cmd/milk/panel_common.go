package main

import (
	"hash/fnv"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/sgr"
	"github.com/scoutme/milk/internal/tasks"
)

// sidePanelLines returns the full (unwindowed) content lines and inner
// content width for the given side-panel region. This is the single place
// that knows how to build each panel's content; rendering, scroll clamping,
// and selection-text extraction all go through it so the three can never
// drift out of sync with each other.
func (m *model) sidePanelLines(region panelRegion) (lines []string, inner int) {
	switch region {
	case regionMemory:
		lines, _ := memoryPanelRows(m.mem, memoryPanelInner, m.currentSessionBricks(), m.memoryExpanded)
		return lines, memoryPanelInner
	case regionTasks:
		lines, _ := taskPanelRows(m.taskStore, tasksPanelInner, m.taskExpanded)
		return lines, tasksPanelInner
	case regionBackground:
		var jobs []local.Job
		if m.agents.backgroundMgr != nil {
			jobs = m.agents.backgroundMgr.Jobs()
		}
		return buildBackgroundPanelLines(jobs, local.BackgroundShells(), backgroundPanelInner), backgroundPanelInner
	case regionWorkflow:
		return buildWorkflowPanelLines(m.workflowState, workflowPanelInner), workflowPanelInner
	case regionProject:
		if m.projectLines == nil {
			// No data msg yet (restore-at-startup or the first frames after
			// toggle-open): render the builder's loading row — title included —
			// instead of an empty box. Empty panel space is unacceptable.
			lines, _ := buildProjectTreeLines(nil, false, m.projectExpanded, projectPanelInner, m.changesetFiles)
			return lines, projectPanelInner
		}
		return m.projectLines, projectPanelInner
	case regionGit:
		if m.gitLines == nil {
			// Same gap for git: population only rides the /panel toggle or a
			// focus-cycle refresh (sidePanelLines has no cwd to fetch with),
			// so pre-first-msg frames must show "(loading…)", not blank rows.
			lines, _ := buildGitPanelLines(nil, "", 0, "", gitPanelInner)
			return lines, gitPanelInner
		}
		return m.gitLines, gitPanelInner
	}
	return nil, 0
}

// panelShortcut returns the F-key label shown next to a panel's title,
// matching the F1-F6 global shortcuts wired into repl.go's key handler
// (mirrored by /panel <name>): memory, tasks, background, workflow, project,
// git — the same left-to-right order panels are joined in View().
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
	case regionProject:
		return "F5"
	case regionGit:
		return "F6"
	}
	return ""
}

// panelTitleLine renders a side panel's title row: the title left-aligned
// (styled via stylePanelTitle) and its F1-F6 shortcut right-aligned within
// inner columns, dimmed — so the toggle key is visible without opening
// /panel or memorizing it. Title is truncated, not the hint, if both
// together would overflow inner.
func panelTitleLine(title string, region panelRegion, inner int) string {
	if th.modern {
		return panelTabStrip(region, inner)
	}
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
// via /panel or its F1-F6 shortcut this session (see panelManualOverride),
// in which case their choice sticks and this is a no-op. The override is
// ADR-0059-scoped: it is dropped again as soon as the panel's content
// finalizes (see resetManualOverrideIfFinalized), so the *next* active
// content auto-opens the panel once more — a restored panel-state
// preference never vetoes auto-open (ADR-0087, it is startup layout only).
//
// A silent auto-open is exactly the kind of surprise event that should
// explain itself: when the panel *transitions* closed→open (issue #162 /
// ADR-0044's panel state changes), post a notification toast with the
// command that closes it again. Repeated calls while already open — e.g.
// every workflow chunk — stay silent.
//
// On a tabbed theme (ADR-0079) content never moves the user: an auto-open
// only fires while the shared slot is free (see the gate below), so live
// content can never yank the view off the panel the user is looking at.
func (m *model) autoOpenPanel(region panelRegion) {
	// Content event → recency stamp (ADR-0089), ahead of every gate: the
	// panel's content changed whether or not the panel is allowed to open
	// right now (master switch off, manual override, tab slot busy), and a
	// budget-hidden panel must still become the freshest candidate so the
	// next layout pass swaps it in.
	m.stampPanelActivity(region)
	// Master switch (ADR-0087, /panel autoopen): off means content never
	// opens a panel on its own — not even once, not even silently.
	if !m.panelAutoOpen {
		return
	}
	if m.panelManualOverride[region] {
		return
	}
	wasOpen := m.panelVisible(region)
	// Tabbed themes (ADR-0079): while another panel occupies the shared
	// slot, content for a closed panel stays a notification instead of
	// switching the user's tab — auto-open takes a free slot or none.
	if panelTabsActive() && !wasOpen && m.anyPanelVisible() {
		return
	}
	if ptr := m.panelOpenPtr(region); ptr != nil {
		*ptr = true
	}
	if !wasOpen && m.panelVisible(region) {
		// Tabbed themes (ADR-0079): the panel that just opened takes the
		// shared slot (the gate above means the slot was free) — once.
		// Later content for the same already-open panel (every workflow
		// chunk, every task update) never yanks the tab back from wherever
		// the user has moved on to.
		m.activatePanelTab(region)
		m.notify(panelDisplayName(region)+" panel opened", "/panel "+panelSubName(region))
	}
}

// panelContentFinalized reports whether the given panel's content has
// settled into a terminal state — emptied, completed, or done — with
// nothing in flight. It drives the manual-override reset (see
// resetManualOverrideIfFinalized): a user's explicit close only needs to
// stick while there is live content it was dismissed over.
//
// Per panel, mirroring what each build*PanelLines shows as its "active"
// content:
//   - memory: no percepts left in either scope. The context bricks
//     (need/summaries/brief) are session-summary state, not activity, so
//     they don't keep the content "live".
//   - tasks: every session + global task is done, or there are none
//     (blocked/in-progress/pending count as active).
//   - background: no job and no background shell is still running (ADR-0064
//     merged both into the one panel) — an empty list (all spawned
//     jobs finished and drained) is finalized too.
//   - workflow: no run in flight and the loaded state is nil, nameless
//     (panel shows "no active workflow"), or already at Role "done".
//     "interrupted"/"waiting for your answers" states stay active.
func (m *model) panelContentFinalized(region panelRegion) bool {
	switch region {
	case regionMemory:
		if m.mem == nil {
			return true
		}
		if len(m.mem.List(memory.ListOpts{Scope: "session"})) > 0 {
			return false
		}
		return len(m.mem.List(memory.ListOpts{Scope: "global"})) == 0
	case regionTasks:
		if m.taskStore == nil {
			return true
		}
		all, err := m.taskStore.List(tasks.ListOpts{IncludeGlobal: true})
		if err != nil {
			return true
		}
		for _, t := range all {
			if t.Status != tasks.StatusDone {
				return false
			}
		}
		return true
	case regionBackground:
		for _, s := range local.BackgroundShells() {
			if s.Running {
				return false
			}
		}
		if m.agents.backgroundMgr == nil {
			return true
		}
		for _, j := range m.agents.backgroundMgr.Jobs() {
			if j.Status == local.JobRunning {
				return false
			}
		}
		return true
	case regionWorkflow:
		if m.workflowRunning {
			return false
		}
		st := m.workflowState
		return st == nil || st.WorkflowName == "" || st.Role == "done"
	case regionProject, regionGit:
		// No live content lifecycle — always finalized, so a manual close
		// never blocks the next open (see resetManualOverrideIfFinalized).
		return true
	}
	return true
}

// resetManualOverrideIfFinalized drops a panel's sticky manual override
// (panelManualOverride) once its content has finalized — emptied,
// completed, or done (see panelContentFinalized). From that point on, the
// *next* active content auto-opens the panel again: the user's close stays
// respected for the live content it was dismissed over, but no longer
// blocks genuinely new activity later in the session (ADR-0059).
//
// With "remember_panel_state" (ADR-0078, default on) the toggle's
// durable half is panelPrefs — the restored startup layout (ADR-0087
// amendment). The veto itself is always ADR-0059-scoped: it covers
// exactly the live content the user dismissed, and expires when that
// content finalizes, so new activity is never locked out of its panel.
//
// Called from two kinds of sites:
//   - handlePanelCmd, when a toggle *closes* the panel — closing a panel
//     whose content has already settled must not lock it out of its next
//     content either;
//   - the content-finalization events the TUI observes: task-store
//     mutations (taskStoreChangedMsg), the last background job finishing
//     (backgroundJobDoneMsg), the last background shell exiting
//     (backgroundShellDoneMsg), a workflow ending or being cleared
//     (workflow.WorkflowDoneMsg, /workflow clear), and /forget emptying
//     the percept store (resolveForget).
//
// No-op when there is no override for the region or the content is still
// active. Order matters at the event sites: call autoOpenPanel *first*
// (so the finalizing event itself never force-opens a user-closed panel),
// then this reset — only future content re-opens it.
func (m *model) resetManualOverrideIfFinalized(region panelRegion) {
	if m.panelManualOverride[region] && m.panelContentFinalized(region) {
		delete(m.panelManualOverride, region)
	}
}

// panelSubName returns the /panel <name> argument for a region.
func panelSubName(region panelRegion) string {
	switch region {
	case regionMemory:
		return "memory"
	case regionTasks:
		return "tasks"
	case regionBackground:
		return "background"
	case regionWorkflow:
		return "workflow"
	case regionProject:
		return "project"
	case regionGit:
		return "git"
	}
	return ""
}

// regionForPanelSub is the inverse of panelSubName (unknown → regionNone).
// The /panel <name> toggle uses it to record the recency stamp of the panel
// it is about to open (ADR-0089) in one place instead of per case.
func regionForPanelSub(sub string) panelRegion {
	switch sub {
	case "memory":
		return regionMemory
	case "tasks":
		return regionTasks
	case "background":
		return regionBackground
	case "workflow":
		return regionWorkflow
	case "project":
		return regionProject
	case "git":
		return regionGit
	}
	return regionNone
}

// stampPanelActivity records that region's content just changed — or that
// the user just paid attention to it (click/wheel, open toggle) — making it
// the most relevant panel for the width budget to show (ADR-0089). Called
// from every content event (centrally in autoOpenPanel, plus the events that
// finalize rather than auto-open) and from attention events; cosmetic
// refreshes (elapsed counters, no-change polls) must NOT stamp.
func (m *model) stampPanelActivity(region panelRegion) {
	if region == regionNone || int(region) >= len(m.panelActivity) {
		return
	}
	m.panelActivity[region] = time.Now()
}

// panelRankByActivity returns the in-layout panels ordered by relevance for
// the width budget: most recently stamped first, ties keeping
// panelRenderOrder (so a fresh session — every timestamp zero — lays out
// exactly left-to-right, the deterministic startup baseline). Insertion
// sort into a stack array: at most six elements, no allocation on the
// per-render panelVisible path (see ADR-0088's render-loop budget).
func (m *model) panelRankByActivity() (ranked [regionGit + 1]panelRegion, n int) {
	for _, r := range panelRenderOrder {
		if !m.panelInLayout(r) {
			continue
		}
		pos := n // strictly-newer wins the slot; equal timestamps keep order
		for i := 0; i < n; i++ {
			if m.panelActivity[r].After(m.panelActivity[ranked[i]]) {
				pos = i
				break
			}
		}
		copy(ranked[pos+1:n+1], ranked[pos:n])
		ranked[pos] = r
		n++
	}
	return ranked, n
}

// memoryContentSig fingerprints what the memory panel would render for the
// given store (percept rows, expansion-independent), so the 5s refresh tick
// can tell real content change from a no-op redraw when deciding whether to
// stamp recency (ADR-0089). Empty string = no store; the first non-empty
// read only establishes the baseline (callers compare against a non-empty
// previous value before stamping).
func (m *model) memoryContentSig() string {
	if m.mem == nil {
		return ""
	}
	lines, _ := memoryPanelRows(m.mem, memoryPanelInner, m.currentSessionBricks(), "")
	h := fnv.New64a()
	for _, l := range lines {
		_, _ = h.Write([]byte(l))
		_, _ = h.Write([]byte{0}) // row separator: "ab"+"c" can't hash like "a"+"bc"
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

// panelDisplayName returns the human-readable panel name used in
// notifications and confirmations ("background panel", ...).
func panelDisplayName(region panelRegion) string {
	switch region {
	case regionBackground:
		return "background"
	case regionMemory, regionTasks, regionWorkflow, regionProject, regionGit:
		return panelSubName(region)
	}
	return "side"
}

// panelRenderOrder is the left-to-right order side panels are joined in by
// View() — memory, tasks, background, workflow, project, git.
var panelRenderOrder = []panelRegion{regionMemory, regionTasks, regionBackground, regionWorkflow, regionProject, regionGit}

// panelAltBackground reports whether region should render with the subtle
// alternating background tint (see withPanelBackground): true for every
// other panel among those *currently in the layout*, counted left-to-right in
// panelRenderOrder — not a fixed per-region parity. The count starts with
// the panel adjacent to the transcript, so the chat/panel boundary is
// always a tint change (the first panel must differ from the chat).
// Counting only visible panels means two panels that end up visually adjacent
// because something between them is closed never land on the same
// background by coincidence — and on a tabbed theme (ADR-0079) the one panel
// in the shared slot is always the first, so it always differs from the chat.
func (m *model) panelAltBackground(region panelRegion) bool {
	idx := 0
	for _, r := range panelRenderOrder {
		if !m.panelVisible(r) {
			continue
		}
		if r == region {
			return idx%2 == 0
		}
		idx++
	}
	return false
}

// panelOpenPtr returns a pointer to the model's raw open flag for the given
// side-panel region (no layout/width gating — panelVisible's width budget is
// render-time only), or nil for regionNone/an unknown region. Toggling
// (/panel, F1-F6), auto-open and panel-state persistence share it, so the
// setters can never drift out of sync with the readers.
func (m *model) panelOpenPtr(region panelRegion) *bool {
	switch region {
	case regionMemory:
		return &m.panelMemory
	case regionTasks:
		return &m.panelTasks
	case regionBackground:
		return &m.panelBackground
	case regionWorkflow:
		return &m.workflowPanelOpen
	case regionProject:
		return &m.panelProject
	case regionGit:
		return &m.panelGit
	}
	return nil
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
	case regionProject:
		return &m.projectOffset
	case regionGit:
		return &m.gitOffset
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
		all = highlightSelection(all, m.panelSelAnchorLine, m.panelSelAnchorCol, m.panelSelEndLine, m.panelSelEndCol)
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
		// Never let a line outgrow the panel (issue #212): an over-wide row
		// would push the scrollbar column out of the visible area. Builders
		// wrap/truncate their own content — this clamp is the guarantee that
		// the frame holds even if one of them ever doesn't.
		line = ansi.Truncate(line, inner, "…")
		if lineW := ansi.StringWidth(ansi.Strip(line)); lineW < inner {
			line += strings.Repeat(" ", inner-lineW)
		}
		rows = append(rows, sgr.WithBackground(line, bg))
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
			rows = append(rows, sgr.WithBackground(" ", bg))
		}
		return strings.Join(rows, "\n")
	}

	thumbTop, thumbBot := scrollThumb(h, total, *m.panelOffsetPtr(region))
	for i := range h {
		if i >= thumbTop && i <= thumbBot {
			rows = append(rows, sgr.WithBackground(scrollThumbGlyph(), bg))
		} else {
			rows = append(rows, sgr.WithBackground(scrollTrackGlyph(), bg))
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
