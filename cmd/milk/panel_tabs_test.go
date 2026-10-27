package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// Tabbed panel docking (ADR-0079): a theme that renders the tab strip docks
// every side panel into one shared slot — and tabs are exclusive: moving to
// a panel closes the one you moved away from, auto-open only takes a free
// slot, and multi-open residue collapses on the first interaction and at
// startup/theme-switch. These tests pin the shared slot, the exclusive
// F-key semantics, the tab-bar click, the instant tab swap (no animation),
// and that theme "off" keeps the side-by-side layout.

// withPanelTabs activates the tabbed (supermilk) theme for one test and
// restores whatever theme was active afterwards.
func withPanelTabs(t *testing.T) {
	t.Helper()
	old := th.name
	setTheme("supermilk")
	t.Cleanup(func() { setTheme(old) })
}

// TestTabbedTheme_PanelsShareOneSlot: only the front tab occupies layout
// columns — one panel slot wide, one panel in regionAt. The second panel is
// seeded directly (the activation paths never leave multi-open state
// behind); the front filter is what keeps such residue out of layout before
// enforceTabbedPanelExclusivity collapses it.
func TestTabbedTheme_PanelsShareOneSlot(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.width = 200
	m.panelMemory = true
	m.panelTasks = true
	m.activatePanelTab(regionMemory)

	if !m.panelVisible(regionMemory) {
		t.Fatal("expected the front tab (memory) to be visible")
	}
	if m.panelVisible(regionTasks) {
		t.Fatal("expected the seeded tasks panel to stay out of layout, not sit beside memory")
	}
	if !m.panelRawOpen(regionTasks) {
		t.Fatal("the seeded panel's open flag must be untouched by the layout filter")
	}
	if got := m.mainWidth(); got != 200-panelSlotWidth {
		t.Errorf("mainWidth=%d, want %d (one shared slot)", got, 200-panelSlotWidth)
	}
	region, _ := m.regionAt(m.mainWidth() + 1)
	if region != regionMemory {
		t.Errorf("panel columns map to %v, want the front tab regionMemory", region)
	}
}

// TestPanelTabsOff_PanelsStaySiblings: without tabs (theme "off") nothing
// about the shared slot applies — two open panels are two visible columns.
func TestPanelTabsOff_PanelsStaySiblings(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width = 200
	m.panelMemory = true
	m.panelTasks = true

	if !m.panelVisible(regionMemory) || !m.panelVisible(regionTasks) {
		t.Fatal("expected both open panels visible without tabs")
	}
	if got := m.mainWidth(); got != 200-2*panelSlotWidth {
		t.Errorf("mainWidth=%d, want %d (two sibling slots)", got, 200-2*panelSlotWidth)
	}
	if region, _ := m.regionAt(m.mainWidth() + panelSlotWidth + 1); region != regionTasks {
		t.Errorf("second slot maps to %v, want regionTasks", region)
	}
}

// TestTabbedTheme_MovingToAPanelClosesThePrevious: tabs are exclusive —
// opening or moving to a panel closes the one you moved away from, and
// closing the front panel leaves the slot empty (no hidden sibling waits to
// take it). Multi-open residue (flags set outside the activation paths)
// obeys the same rule on first interaction.
func TestTabbedTheme_MovingToAPanelClosesThePrevious(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.width = 200

	// Open memory, then move to tasks: memory closes — it must not sit
	// hidden behind its tab.
	updated, _ := m.handlePanelCmd("memory")
	m = updated.(model)
	if !m.panelRawOpen(regionMemory) || m.panelActive() != regionMemory {
		t.Fatalf("opening memory: open=%v front=%v", m.panelRawOpen(regionMemory), m.panelActive())
	}
	updated, _ = m.handlePanelCmd("tasks")
	m = updated.(model)
	if m.panelActive() != regionTasks {
		t.Fatalf("front tab = %v, want regionTasks", m.panelActive())
	}
	if m.panelRawOpen(regionMemory) {
		t.Fatal("moving to tasks must close the panel moved away from (memory)")
	}

	// Move back to memory: tasks closes, memory reopens.
	updated, _ = m.handlePanelCmd("memory")
	m = updated.(model)
	if m.panelActive() != regionMemory || !m.panelRawOpen(regionMemory) {
		t.Fatalf("front=%v open=%v after moving back", m.panelActive(), m.panelRawOpen(regionMemory))
	}
	if m.panelRawOpen(regionTasks) {
		t.Fatal("moving back to memory must close tasks")
	}

	// F-key on the front panel closes it: the slot empties — nothing waits
	// behind it (the old browser-tab fallback is gone with exclusivity).
	updated, _ = m.handlePanelCmd("memory")
	m = updated.(model)
	if m.panelActive() != regionNone {
		t.Errorf("front tab = %v after closing the only open panel, want regionNone", m.panelActive())
	}

	// Multi-open residue (seeded directly, as a sibling session or early
	// startup might leave it): moving to the hidden panel brings it forward
	// AND closes the rest — the invariant repairs itself on first use.
	m.panelTasks = true
	m.activatePanelTab(regionTasks)
	m.panelMemory = true
	stampedBefore := m.panelActivity[regionMemory]
	time.Sleep(2 * time.Millisecond) // distinct clock reading if the move stamps
	updated, _ = m.handlePanelCmd("memory")
	m = updated.(model)
	if m.panelActive() != regionMemory || !m.panelRawOpen(regionMemory) {
		t.Fatalf("front=%v open=%v after moving to the seeded memory tab", m.panelActive(), m.panelRawOpen(regionMemory))
	}
	if !m.panelActivity[regionMemory].After(stampedBefore) {
		t.Error("moving to a panel is user interest — it must stamp recency (ADR-0089)")
	}
	if m.panelRawOpen(regionTasks) {
		t.Fatal("moving to a hidden panel must close the front one")
	}
}

// TestTabbedTheme_TabBarClickSwitches: a click on a tab label does what that
// tab's F-key does — open the panel when its tab is closed (closing whatever
// was in the slot: tabs are exclusive), no-op on the front label.
func TestTabbedTheme_TabBarClickSwitches(t *testing.T) {
	withPanelTabs(t)
	m := layoutTestModel(t, 200, 40)
	m.panelMemory = true
	m.activatePanelTab(regionMemory)
	mw := m.mainWidth()

	clickTab := func(col int) {
		t.Helper()
		// The tab strip is the panel's first content row (content row 0),
		// whose screen row comes from the rendered chrome (panelRowScreen).
		// A click is press+release on the same cell.
		y := panelRowScreen(&m, 0)
		press := mouseEvent{X: mw + col, Y: y, Button: tea.MouseLeft, Action: mousePress}
		release := mouseEvent{X: mw + col, Y: y, Button: tea.MouseLeft, Action: mouseRelease}
		updated, _ := m.handleMouse(press.msg())
		m = *updated.(*model)
		updated, _ = m.handleMouse(release.msg())
		m = *updated.(*model)
	}

	clickTab(29) // "git" occupies columns 29-31 of the strip
	if !m.panelGit {
		t.Fatal("clicking a closed tab must open its panel")
	}
	if m.panelActive() != regionGit {
		t.Fatalf("front tab = %v, want regionGit after clicking its tab", m.panelActive())
	}
	if m.panelMemory {
		t.Fatal("moving to the git tab must close the memory panel")
	}

	clickTab(0) // "memory"
	if m.panelActive() != regionMemory {
		t.Fatalf("front tab = %v, want regionMemory after clicking its tab", m.panelActive())
	}
	if m.panelGit {
		t.Fatal("moving back to the memory tab must close git")
	}
}

// TestPanelTabAt_CoversEveryTabLabel: the click hit-test walks the same
// single-space join the strip renders with — every label maps to its own
// region and the gaps between labels map to none.
func TestPanelTabAt_CoversEveryTabLabel(t *testing.T) {
	x := 0
	for _, tab := range panelTabs {
		if got := panelTabAt(x); got != tab.region {
			t.Errorf("column %d (%s): got %v, want %v", x, tab.name, got, tab.region)
		}
		w := len([]rune(tab.name))
		if got := panelTabAt(x + w - 1); got != tab.region {
			t.Errorf("column %d (last of %s): got %v, want %v", x+w-1, tab.name, got, tab.region)
		}
		if got := panelTabAt(x + w); got != regionNone {
			t.Errorf("column %d (gap after %s): got %v, want regionNone", x+w, tab.name, got)
		}
		x += w + 1
	}
	if got := panelTabAt(x); got != regionNone {
		t.Errorf("column %d (past the strip): got %v, want regionNone", x, got)
	}
}

// TestTabbedTheme_TabSwitchAnimatesNothing: switching tabs swaps content
// inside the occupied slot — no slide, no ghost. The slot itself still
// animates: appearing slides in, the last close shrinks out.
func TestTabbedTheme_TabSwitchAnimatesNothing(t *testing.T) {
	withPanelTabs(t)
	oldAnim := animationsEnabled
	animationsEnabled = true
	t.Cleanup(func() { animationsEnabled = oldAnim })

	m := newTestModelForPanels(t)
	m.width, m.height = 200, 40

	// Each step gets a fresh animation state: trackAnimations hands its
	// command out only when it arms the shared tick, and a tick left armed
	// by a previous step would mask every later one.
	step := func(prev model) tea.Cmd {
		m.anim = newAnimState()
		return m.trackAnimations(&prev)
	}

	// First panel: the slot appears — a slide-in must run.
	prev := m
	updated, _ := m.handlePanelCmd("memory")
	m = updated.(model)
	if cmd := step(prev); cmd == nil {
		t.Fatal("opening the first panel must animate (slot appears)")
	}

	// Tab switch: the F-key moves to tasks, closing memory — the slot stays
	// occupied, so the swap is instant: no slide, no ghost.
	prev = m
	updated, _ = m.handlePanelCmd("tasks")
	m = updated.(model)
	if cmd := step(prev); cmd != nil {
		t.Fatal("a tab switch must swap in place, not animate")
	}

	// Closing the last panel: the slot disappears — the exit must run.
	prev = m
	updated, _ = m.handlePanelCmd("tasks")
	m = updated.(model)
	if cmd := step(prev); cmd == nil {
		t.Fatal("closing the last panel must animate (slot disappears)")
	}
}

// TestTabbedTheme_AutoOpenOnlyTakesAFreeSlot: content-driven auto-open is
// not allowed to move the user — it fires only while the shared slot is
// free (or the panel already holds it). Content for a closed panel while
// another panel is shown stays a notification, and content for the shown
// panel keeps it exactly where it is.
func TestTabbedTheme_AutoOpenOnlyTakesAFreeSlot(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.width = 200

	// Slot free: the first auto-open takes it.
	m.autoOpenPanel(regionTasks)
	if !m.panelRawOpen(regionTasks) || m.panelActive() != regionTasks {
		t.Fatalf("auto-open with a free slot: open=%v front=%v", m.panelRawOpen(regionTasks), m.panelActive())
	}

	// The user moves to memory (closing tasks), then task content arrives:
	// the view must not move and tasks must not reopen.
	updated, _ := m.handlePanelCmd("memory")
	m = updated.(model)
	m.autoOpenPanel(regionTasks)
	if m.panelActive() != regionMemory {
		t.Fatalf("front tab = %v, want regionMemory (content must not move the user)", m.panelActive())
	}
	if m.panelRawOpen(regionTasks) {
		t.Fatal("auto-open must not reopen a panel while the shared slot is taken")
	}

	// Content for the panel already in the slot leaves it in place.
	m.autoOpenPanel(regionMemory)
	if m.panelActive() != regionMemory || !m.panelRawOpen(regionMemory) {
		t.Fatalf("front=%v open=%v, want memory untouched", m.panelActive(), m.panelRawOpen(regionMemory))
	}
}

// TestTabbedTheme_PanelStateRoundTripsFrontTab: the front tab of the shared
// slot is part of the panel preference and comes back across sessions — but
// only for manually chosen panels, never for auto-opened ones (pinned
// together with the open-flag rule by TestPanelState_OnlyManualTogglesPersisted).
func TestTabbedTheme_PanelStateRoundTripsFrontTab(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")

	updated, _ := m.handlePanelCmd("memory")
	m = updated.(model)
	updated, _ = m.handlePanelCmd("git")
	m = updated.(model)
	if m.panelActive() != regionGit {
		t.Fatalf("front tab = %v, want regionGit", m.panelActive())
	}
	if err := m.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(m.panelStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"active":"git"`) {
		t.Errorf("expected the front tab in the state file, got %s", data)
	}

	m2 := newTestModelForPanels(t)
	m2.width = 200 // wide enough for the git panel's layout guard
	m2.panelStatePath = m.panelStatePath
	m2.loadPanelState(m2.panelStatePath)
	if m2.panelActive() != regionGit {
		t.Errorf("front tab after restore = %v, want regionGit", m2.panelActive())
	}
	if !m2.panelVisible(regionGit) {
		t.Error("expected the restored front tab's panel to occupy the shared slot")
	}
	if m2.panelRawOpen(regionMemory) {
		t.Error("the panel moved away from must stay closed on restore, not reappear behind a tab")
	}
}

// TestTabbedTheme_ExclusivityEnforcedOutsideActivationPaths: multi-open
// panel states set outside the activation paths — sibling-session state
// restored at startup, or /theme switching from siblings to tabs — collapse
// to the single front panel instead of lingering as hidden-open residue.
func TestTabbedTheme_ExclusivityEnforcedOutsideActivationPaths(t *testing.T) {
	withPanelTabs(t)
	openCount := func(mm model) int {
		n := 0
		for _, r := range panelRenderOrder {
			if mm.panelRawOpen(r) {
				n++
			}
		}
		return n
	}

	// Directly seeded multi-open (what restore can hand startup): the helper
	// keeps the front panel, closes the rest, and persists the collapse —
	// the state file must reflect it without waiting for a panel action.
	m := newTestModelForPanels(t)
	m.width = 200
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")
	m.panelMemory = true
	m.panelTasks = true
	m.panelManualOverride = map[panelRegion]bool{regionMemory: true, regionTasks: true}
	m.activatePanelTab(regionTasks)
	m.enforceTabbedPanelExclusivity()
	if !m.panelRawOpen(regionTasks) || m.panelActive() != regionTasks {
		t.Fatalf("front = %v open(tasks) = %v, want tasks kept", m.panelActive(), m.panelRawOpen(regionTasks))
	}
	if m.panelRawOpen(regionMemory) {
		t.Fatal("enforcement must close the panel that is not in front")
	}
	data, err := os.ReadFile(m.panelStatePath)
	if err != nil {
		t.Fatalf("expected the collapse to be persisted: %v", err)
	}
	if !strings.Contains(string(data), `"memory":false`) || !strings.Contains(string(data), `"tasks":true`) {
		t.Errorf("persisted state must show the collapse, got %s", data)
	}

	// Theme switch from siblings to tabs, through the /theme command the
	// user would actually run: same collapse.
	setTheme("off")
	t.Cleanup(func() { setTheme("supermilk") })
	m2 := newTestModelForPanels(t)
	m2.width = 200
	m2.panelMemory = true
	m2.panelTasks = true
	updated, _ := m2.handleThemeCmd("supermilk")
	m2 = updated.(model)
	if got := openCount(m2); got != 1 {
		t.Errorf("%d panels open after /theme supermilk, want exactly 1", got)
	}
	if m2.panelActive() == regionNone || !m2.panelVisible(m2.panelActive()) {
		t.Error("the surviving panel must occupy the shared slot")
	}
}
