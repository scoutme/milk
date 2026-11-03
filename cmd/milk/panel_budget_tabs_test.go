package main

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// budgetTabsModel builds a sibling-layout model (theme off) with the memory
// and git panels open at a width where only one 33-col column fits; both are
// grouped (2 open > 1 column) and stamping git makes it the freshest — the
// group's top: its content fills the column and its label is the strip's
// active one (the labels themselves stay in fixed f1–f6 order).
func budgetTabsModel(t *testing.T) model {
	t.Helper()
	setTheme("off")
	t.Cleanup(func() { setTheme("off") })
	m := newTestModelForPanels(t)
	m.width = 100 // floor((100-40)/33) = 1 slot
	m.panelMemory = true
	m.panelGit = true
	m.stampPanelActivity(regionGit)
	return m
}

// TestBudgetGroup_PositionalSplit pins issue #373's model end-to-end in the
// spec's own scenario: 4 columns (width 200), every panel open except tasks.
// The open panels split by *position* — "the grouped ones are always the
// rightmost ones between the opened ones": the first three slots hold f1 f3
// f4 (memory background workflow) in order, the grouped column holds f5 f6
// (project git). Between the grouped ones the fresher one is on top; a tab
// click moves a grouped panel to the top of its group and nothing else — the
// two sets never change, and the strip's labels keep their fixed f1–f6 order
// throughout (selection moves only the styling and the content).
func TestBudgetGroup_PositionalSplit(t *testing.T) {
	setTheme("off")
	t.Cleanup(func() { setTheme("off") })
	m := newTestModelForPanels(t)
	m.width = 200                                 // floor((200-40)/33) = 4 columns
	m.panelMemory, m.panelBackground = true, true // tasks (f2) closed
	m.workflowPanelOpen, m.panelProject, m.panelGit = true, true, true
	// Between the grouped ones (project, git), git is the fresher one.
	m.panelActivity[regionGit] = time.Now().Add(-time.Minute)
	m.panelActivity[regionProject] = time.Now().Add(-2 * time.Minute)

	// The first three slots in order: f1 f3 f4. The grouped column: f5 f6,
	// with the fresher one (git) on top and its content on screen.
	if got, want := visiblePanels(&m), []panelRegion{regionMemory, regionBackground, regionWorkflow, regionGit}; !slices.Equal(got, want) {
		t.Fatalf("visible = %v, want %v (free in order + the freshest grouped one)", got, want)
	}
	if group := m.budgetTabGroupFor(regionGit); !slices.Equal(group, []panelRegion{regionProject, regionGit}) {
		t.Fatalf("group strip = %v, want [project git] (fixed f1–f6 labels; git is the top — styled, not first)", group)
	}
	for _, r := range []panelRegion{regionMemory, regionBackground, regionWorkflow, regionProject} {
		if g := m.budgetTabGroupFor(r); g != nil {
			t.Fatalf("only the group's top carries the strip; %v got %v", r, g)
		}
	}

	// Clicking project's tab moves it to the top of its group — the two
	// sets don't change ("the grouped ones are always the rightmost ones").
	m.handleBudgetTabClick(regionProject)
	if got, want := visiblePanels(&m), []panelRegion{regionMemory, regionBackground, regionWorkflow, regionProject}; !slices.Equal(got, want) {
		t.Fatalf("after click: visible = %v, want %v (same free panels, project on top)", got, want)
	}
	if group := m.budgetTabGroupFor(regionProject); !slices.Equal(group, []panelRegion{regionProject, regionGit}) {
		t.Fatalf("after click: group strip = %v, want [project git] (labels unmoved, gate moved)", group)
	}
	if g := m.budgetTabGroupFor(regionGit); g != nil {
		t.Fatalf("git lost the top — its row must carry no strip, got %v", g)
	}

	// The top's own tab is inert — clicking what you see moves nothing.
	before := visiblePanels(&m)
	m.handleBudgetTabClick(regionProject)
	if got := visiblePanels(&m); !slices.Equal(got, before) {
		t.Errorf("clicking the top's tab moved something: %v, want %v", got, before)
	}

	// The group column is the rightmost one: regionAt maps the last column
	// to the top, whatever its render position (project renders before git).
	x := m.mainWidth()
	for _, r := range []panelRegion{regionMemory, regionBackground, regionWorkflow} {
		x += panelWidth(r)
	}
	if got, _ := m.regionAt(x + 1); got != regionProject {
		t.Errorf("rightmost column = %v, want the group's top regionProject", got)
	}
}

// TestBudgetTabGroup_HiddenPeersAndGates pins the strip's gate: the grouped
// set = the rightmost open panels (the ones past the free columns) in fixed
// f1–f6 order (labels never reorder on selection), and only its top carries
// the strip — nil on a wide terminal (nothing grouped), nil for non-top
// rows, nil while tabs are active (ADR-0079's slot can't hold grouped
// peers).
func TestBudgetTabGroup_HiddenPeersAndGates(t *testing.T) {
	m := budgetTabsModel(t)

	group := m.budgetTabGroupFor(regionGit)
	if len(group) != 2 || group[0] != regionMemory || group[1] != regionGit {
		t.Fatalf("top's group = %v, want [memory git] (fixed f1–f6 labels; git is the top, styled)", group)
	}
	if g := m.budgetTabGroupFor(regionMemory); g != nil {
		t.Errorf("a non-top row must not carry the strip, got %v", g)
	}
	if g := m.budgetTabGroupFor(regionNone); g != nil {
		t.Errorf("regionNone group = %v, want nil", g)
	}

	// Wide terminal: every panel fits, so nothing is hidden.
	m.width = 400
	if g := m.budgetTabGroupFor(regionGit); g != nil {
		t.Errorf("nothing budget-hidden must render no strip, got %v", g)
	}
	m.width = 100

	// Every theme grows the strip after the port (ADR-0098): supermilk
	// renders the same group, at the same place.
	setTheme("supermilk")
	t.Cleanup(func() { setTheme("off") })
	if g := m.budgetTabGroupFor(regionGit); len(g) != 2 || g[0] != regionMemory || g[1] != regionGit {
		t.Errorf("supermilk must grow the same budget tabs, got %v", g)
	}
}

// TestBudgetTabAt_HitTestWalksTheStrip pins the geometry shared with the
// render: the strip starts at column 0 (the title row *becomes* the strip),
// one-space joins, gaps and outside miss.
func TestBudgetTabAt_HitTestWalksTheStrip(t *testing.T) {
	group := []panelRegion{regionMemory, regionGit} // "memory git" = 10 cells
	if w := budgetStripWidth(group, 32); w != 10 {
		t.Fatalf("strip width = %d, want 10", w)
	}
	for _, tc := range []struct {
		col  int
		want panelRegion
	}{
		{-1, regionNone},
		{0, regionMemory},
		{5, regionMemory}, // last cell of "memory"
		{6, regionNone},   // the join space
		{7, regionGit},
		{9, regionGit},   // last cell of "git"
		{10, regionNone}, // past the strip — the row's padding
		{31, regionNone}, // last content cell
		{32, regionNone}, // the scrollbar column
	} {
		if got := budgetTabAt(group, 32, tc.col); got != tc.want {
			t.Errorf("budgetTabAt(col=%d) = %v, want %v", tc.col, got, tc.want)
		}
	}

	// The full six-panel group: "memory tasks bg flow files git" = 30 cells.
	full := []panelRegion{regionMemory, regionTasks, regionBackground, regionWorkflow, regionProject, regionGit}
	if w := budgetStripWidth(full, 32); w != 30 {
		t.Fatalf("full group width = %d, want 30", w)
	}
	if got := budgetTabAt(full, 32, 0); got != regionMemory {
		t.Errorf("full strip col0 = %v, want memory", got)
	}
	if got := budgetTabAt(full, 32, 29); got != regionGit {
		t.Errorf("full strip col29 = %v, want git", got)
	}
	if got := budgetTabAt(full, 32, 31); got != regionNone {
		t.Errorf("full strip col31 (past the labels) = %v, want regionNone", got)
	}
}

// TestBudgetTabStrip_ReplacesTitleWithLeftAlignedTabs: when the budget
// hides a peer, the host's title row *is* the strip — its own name appears
// only once (as the underlined tab), the builder's title and F-key hint
// beside it would be a duplication and must not bleed through.
func TestBudgetTabStrip_ReplacesTitleWithLeftAlignedTabs(t *testing.T) {
	group := []panelRegion{regionMemory, regionGit}
	existing := panelTitleLine("git", regionGit, 32)
	if !strings.Contains(ansi.Strip(existing), "F6") {
		t.Fatal("setup: the plain title row carries title + F-key hint")
	}
	strip := ansi.Strip(budgetTabStrip(group, regionGit, 32))
	if strip != "memory git" {
		t.Errorf("row = %q, want exactly the tab labels from col 0", strip)
	}
	if strings.Contains(strip, "F6") {
		t.Errorf("the title row's hint must not survive beside the strip, got %q", strip)
	}
	if strings.HasPrefix(strip, "git") {
		t.Errorf("the host's title must not bleed next to its own tab, got %q", strip)
	}
}

// TestRenderSidePanel_BudgetStripOnlyWhenHidden drives the real render
// funnel: plain title row while nothing is hidden (byte-for-byte today's
// row), strip appended once the budget hides a peer.
func TestRenderSidePanel_BudgetStripOnlyWhenHidden(t *testing.T) {
	oldTTY := isTTY
	isTTY = true // renderSidePanel renders blank rows without a TTY
	t.Cleanup(func() { isTTY = oldTTY })

	m := budgetTabsModel(t)
	m.width = 400 // everything fits: no strip
	base := strings.Split(m.renderSidePanel(regionGit, 8), "\n")[0]
	if plain := ansi.Strip(base); strings.Contains(plain, "memory") || !strings.Contains(plain, "git") {
		t.Fatalf("unhidden host row = %q, want the plain git title", plain)
	}

	m.width = 100 // one slot: memory drops out of the layout
	row := strings.Split(m.renderSidePanel(regionGit, 8), "\n")[0]
	plain := ansi.Strip(row)
	if !strings.HasPrefix(plain, "memory git") {
		t.Errorf("row must become the left-aligned strip (fixed f1–f6 labels from col 0, git styled as the top), got %q", plain)
	}
	if strings.Contains(plain, "F6") {
		t.Errorf("the replaced title's F-key hint must not bleed through, got %q", plain)
	}
	if w := ansi.StringWidth(plain); w != 32 {
		t.Errorf("row width = %d, want 32 (strip padded to the slot)", w)
	}
}

// TestBudgetTabClick_BringToFrontAndInertHostTab drives the mouse path the way
// the TUI does — press and release on the same cell of the host's title row:
// a hidden peer's label brings it to front (stamp claims the slot, evicting
// the previous host, never toggled closed); the host's own label is a
// deliberate no-op — clicking it must leave the panel open and on screen
// (closing stays with the F-key / /panel press, never with a tab click).
func TestBudgetTabClick_BringToFrontAndInertHostTab(t *testing.T) {
	m := budgetTabsModel(t)
	if !m.panelVisible(regionGit) || m.panelVisible(regionMemory) {
		t.Fatalf("setup: git visible=%v memory visible=%v, want git only",
			m.panelVisible(regionGit), m.panelVisible(regionMemory))
	}
	panelRowStart, _ := m.viewportRows()
	click := func(region panelRegion, col int) model {
		t.Helper()
		// handlePanelMouse has a pointer receiver: it returns *model —
		// mutations land on m either way, but keep whatever comes back.
		nm, _ := m.handlePanelMouse(region, col, mouseEvent{X: 0, Y: panelRowStart, Button: tea.MouseLeft, Action: mousePress})
		if p, ok := nm.(*model); ok {
			m = *p
		} else {
			m = nm.(model)
		}
		nm, _ = m.handlePanelMouse(region, col, mouseEvent{X: 0, Y: panelRowStart, Button: tea.MouseLeft, Action: mouseRelease})
		if p, ok := nm.(*model); ok {
			m = *p
		} else {
			m = nm.(model)
		}
		return m
	}

	// Host = git (the top), strip "memory git" (fixed f1–f6 labels, git
	// styled) at cols 0..9 — click "memory" (cols 0-5).
	m = click(regionGit, 5)
	if !m.panelRawOpen(regionMemory) {
		t.Fatal("a hidden tab press must bring the panel to front, not close it")
	}
	if !m.panelVisible(regionMemory) {
		t.Error("memory must claim the slot it was stamped for")
	}
	if m.panelVisible(regionGit) {
		t.Error("the one-slot budget must evict the stalest visible panel (git)")
	}

	// Host is now memory; clicking the host's own tab must NOT close it —
	// the strip only ever moves panels forward.
	m = click(regionMemory, 2)
	if !m.panelRawOpen(regionMemory) {
		t.Error("clicking the shown panel's own tab must not close it")
	}
	if !m.panelVisible(regionMemory) {
		t.Error("the host must stay on screen after its own tab is clicked")
	}
	if g := m.budgetTabGroupFor(regionMemory); len(g) != 2 {
		t.Errorf("strip must remain (git still hidden), got %v", g)
	}
}
