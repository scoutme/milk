package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// Hybrid panel budget on every theme (ADR-0098 — the port of ADR-0079's
// docking): supermilk and theme "off" share one model — the positional
// free/grouped split (issue #373), the freshest grouped panel on top, the
// strip only on the group column, and the view stack (panelActive) for the
// persisted active panel. These tests pin the parity itself, what the port
// removed (exclusivity, the front-tab filter, the six-tab bar), and that
// navigation, auto-open and persistence behave theme-independently.

// withPanelTabs activates the supermilk theme for one test and restores
// whatever theme was active afterwards.
func withPanelTabs(t *testing.T) {
	t.Helper()
	old := th.name
	setTheme("supermilk")
	t.Cleanup(func() { setTheme(old) })
}

// TestPanelTabsOff_PanelsStaySiblings: baseline for the parity pin below —
// two open panels are two visible columns on theme "off".
func TestPanelTabsOff_PanelsStaySiblings(t *testing.T) {
	setTheme("off")
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

// seedHybrid lays out the shared parity scenario: five open panels at a
// width where four columns fit (git and project grouped, git fresher).
func seedHybrid(m *model) {
	m.width = 200 // floor((200-40)/33) = 4 columns
	m.panelMemory, m.panelBackground = true, true
	m.workflowPanelOpen, m.panelProject, m.panelGit = true, true, true
	m.panelActivity[regionGit] = time.Now().Add(-time.Minute)
	m.panelActivity[regionProject] = time.Now().Add(-2 * time.Minute)
}

// TestModernTheme_SplitsLikeClassic is the port's parity pin: the exact
// same seeds produce the exact same visible set and group on both themes —
// supermilk changes colours, never columns. It also pins that free columns
// carry the plain title row on supermilk (the six-tab bar retired) while
// only the group column wears the strip.
func TestModernTheme_SplitsLikeClassic(t *testing.T) {
	setTheme("off")
	t.Cleanup(func() { setTheme("off") })
	off := newTestModelForPanels(t)
	seedHybrid(&off)
	wantVisible := visiblePanels(&off)
	wantGroup := off.budgetTabGroupFor(off.budgetTop())
	if len(wantVisible) != 4 || wantVisible[3] != regionGit {
		t.Fatalf("theme-off baseline: visible = %v, want [memory background workflow git]", wantVisible)
	}
	if len(wantGroup) != 2 || wantGroup[0] != regionProject || wantGroup[1] != regionGit {
		t.Fatalf("theme-off baseline: group = %v, want [project git]", wantGroup)
	}

	withPanelTabs(t)
	m := newTestModelForPanels(t)
	seedHybrid(&m)
	got := visiblePanels(&m)
	for i := range wantVisible {
		if i >= len(got) || got[i] != wantVisible[i] {
			t.Fatalf("supermilk visible = %v, want the same as theme off: %v", got, wantVisible)
		}
	}
	group := m.budgetTabGroupFor(m.budgetTop())
	if len(group) != len(wantGroup) {
		t.Fatalf("supermilk group = %v, want %v", group, wantGroup)
	}
	for i := range group {
		if group[i] != wantGroup[i] {
			t.Fatalf("supermilk group = %v, want %v", group, wantGroup)
		}
	}

	// Title rows: the free column renders its plain title (name + F-hint,
	// no strip); only the group column's first row *is* the strip.
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })
	firstRow := func(r panelRegion) string {
		line := strings.Split(m.renderSidePanel(r, 8), "\n")[0]
		return strings.TrimRight(ansi.Strip(line), " ")
	}
	if row := firstRow(regionMemory); !strings.Contains(row, "memory") || strings.Contains(row, "git") {
		t.Errorf("free column title row = %q, want the plain title (no strip)", row)
	}
	if row := firstRow(regionGit); row != "files git" {
		t.Errorf("group column title row = %q, want the strip \"files git\" (short labels, project+git)", row)
	}
}

// TestModernTheme_NavigationNeverClosesSiblings: the port's removal of
// ADR-0079 exclusivity — on supermilk, an F-key / /panel press onto a
// grouped panel brings it to the top of its group (stamped and announced,
// like opening) WITHOUT closing anything; a press on a visible free panel
// is the plain close, exactly as on theme "off".
func TestModernTheme_NavigationNeverClosesSiblings(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.width = 130 // 2 columns: free = memory, grouped = [project git], top = project
	m.panelMemory = true
	m.panelProject = true
	m.panelGit = true
	m.activatePanelTab(regionMemory)

	// Grouped panel → brought to front: git tops the group, everything
	// stays open, the move stamps and announces like an open does.
	stampedBefore := m.panelActivity[regionGit]
	time.Sleep(2 * time.Millisecond)
	updated, _ := m.handlePanelCmd("git")
	m = updated.(model)
	if !m.panelRawOpen(regionGit) || m.budgetTop() != regionGit {
		t.Fatalf("bring-to-front: open=%v groupTop=%v, want open and top", m.panelRawOpen(regionGit), m.budgetTop())
	}
	if !m.panelActivity[regionGit].After(stampedBefore) {
		t.Error("moving to a grouped panel must stamp recency (ADR-0089)")
	}
	if !m.panelRawOpen(regionMemory) || !m.panelRawOpen(regionProject) {
		t.Error("the port forbids closing siblings on a move: all three panels must stay open")
	}
	found := false
	for _, ev := range m.toastVisible {
		if ev.text == "git panel: brought to front" {
			found = true
		}
	}
	if !found {
		t.Errorf("the move must announce itself like opening, toasts=%v", m.toastVisible)
	}

	// A press on the visible free panel is the plain close (theme rules
	// identical: only a visible panel toggles shut).
	updated, _ = m.handlePanelCmd("memory")
	m = updated.(model)
	if m.panelRawOpen(regionMemory) {
		t.Error("a press on a visible panel closes it, on every theme")
	}
	if !m.panelRawOpen(regionGit) || !m.panelRawOpen(regionProject) {
		t.Error("closing the free panel must not cascade into the grouped ones")
	}
}

// TestModernTheme_StripClickMovesToTop: the group column's strip is the
// navigation on supermilk too — a full mouse press+release on a grouped
// label moves that panel to the top of its group (never closes anything),
// and clicking the top's own label does nothing.
func TestModernTheme_StripClickMovesToTop(t *testing.T) {
	withPanelTabs(t)
	oldTTY := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = oldTTY })

	m := layoutTestModel(t, 200, 40)
	m.panelMemory, m.panelBackground = true, true
	m.workflowPanelOpen, m.panelProject, m.panelGit = true, true, true
	m.panelActivity[regionProject] = time.Now() // project tops the group initially
	m.panelActivity[regionGit] = time.Now().Add(-time.Minute)
	if m.budgetTop() != regionProject {
		t.Fatalf("baseline group top = %v, want regionProject", m.budgetTop())
	}

	// The group column starts after the three free columns; its strip
	// renders labels "project git" from column 0 of that title row.
	groupX := m.mainWidth() + 3*panelSlotWidth
	y := panelRowScreen(&m, 0)
	click := func(col int) {
		t.Helper()
		press := mouseEvent{X: groupX + col, Y: y, Button: tea.MouseLeft, Action: mousePress}
		release := mouseEvent{X: groupX + col, Y: y, Button: tea.MouseLeft, Action: mouseRelease}
		updated, _ := m.handleMouse(press.msg())
		m = *updated.(*model)
		updated, _ = m.handleMouse(release.msg())
		m = *updated.(*model)
	}

	before := visiblePanels(&m)
	click(0) // "project" — the top's own label: inert
	if m.budgetTop() != regionProject {
		t.Errorf("clicking the top's own tab changed the top: %v", m.budgetTop())
	}
	if got := visiblePanels(&m); len(got) != len(before) {
		t.Errorf("the inert click moved panels: %v, want %v", got, before)
	}

	click(7) // "git" — labels "project git": git at cols 7-9
	if m.budgetTop() != regionGit {
		t.Fatalf("clicking git's label: top = %v, want regionGit", m.budgetTop())
	}
	if !m.panelRawOpen(regionProject) || !m.panelRawOpen(regionMemory) {
		t.Error("a strip click moves the top; it never closes a panel")
	}
}

// TestModernTheme_AutoOpenParity: content-driven auto-open behaves on
// supermilk exactly as it does on theme "off" (TestAutoOpenPanel_BecomesTheGroupTop)
// — the stamp lifts the fresh panel to the top of the group and shows it
// there; the free column never moves.
func TestModernTheme_AutoOpenParity(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.width = 120 // 2 columns
	m.panelMemory = true
	m.panelTasks = true

	m.autoOpenPanel(regionBackground)
	if !m.panelRawOpen(regionBackground) {
		t.Fatal("auto-open must arm the panel's raw flag")
	}
	if !m.panelVisible(regionBackground) {
		t.Error("fresh content must show its panel even when every column is taken")
	}
	if !m.panelVisible(regionMemory) {
		t.Error("memory is free (the leftmost open panel) and keeps its column")
	}
	if m.panelVisible(regionTasks) {
		t.Error("tasks — grouped with background, not the top — must wait behind its tab")
	}
}

// TestViewStack_FrontTabPersists: the view stack (panelActive) follows the
// user's own opens on the ported theme and round-trips the persisted
// active panel — both opened panels restore open (no exclusivity), with
// the last-viewed one active and visible.
func TestViewStack_FrontTabPersists(t *testing.T) {
	withPanelTabs(t)
	m := newTestModelForPanels(t)
	m.panelStatePath = filepath.Join(t.TempDir(), "panel-state.json")

	updated, _ := m.handlePanelCmd("memory")
	m = updated.(model)
	updated, _ = m.handlePanelCmd("git")
	m = updated.(model)
	if m.panelActive() != regionGit {
		t.Fatalf("view-stack front = %v, want regionGit (the panel opened last)", m.panelActive())
	}
	if !m.panelRawOpen(regionMemory) {
		t.Fatal("opening git must not close memory — exclusivity is gone")
	}
	if err := m.savePanelState(); err != nil {
		t.Fatalf("save: %v", err)
	}
	data, err := os.ReadFile(m.panelStatePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"active":"git"`) {
		t.Errorf("expected the front panel in the state file, got %s", data)
	}

	m2 := newTestModelForPanels(t)
	m2.width = 200
	m2.panelStatePath = m.panelStatePath
	m2.loadPanelState(m2.panelStatePath)
	if m2.panelActive() != regionGit {
		t.Errorf("front after restore = %v, want regionGit", m2.panelActive())
	}
	if !m2.panelRawOpen(regionMemory) || !m2.panelRawOpen(regionGit) {
		t.Errorf("restore must bring both panels back: memory=%v git=%v",
			m2.panelRawOpen(regionMemory), m2.panelRawOpen(regionGit))
	}
	if !m2.panelVisible(regionGit) {
		t.Error("the restored front panel must be visible (wide enough: all free)")
	}
}
