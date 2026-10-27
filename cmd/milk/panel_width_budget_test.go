package main

import "testing"

// Tests for the unified side-panel width budget (ADR-0089): one fill-in-order
// rule shared by all six side panels, with a 40-col transcript floor. The
// motivating report: at a terminal width in [119,138], opening
// memory+project+git showed only memory (project/git were width-guarded)
// while memory+tasks+background showed all three (no guard at all) — the same
// total panel width, two different behaviours.

// visiblePanels returns the panels that occupy layout columns right now.
func visiblePanels(m *model) []panelRegion {
	var out []panelRegion
	for _, r := range panelRenderOrder {
		if m.panelVisible(r) {
			out = append(out, r)
		}
	}
	return out
}

// TestPanelWidthBudget_SameBudgetSameCount pins the reported inconsistency:
// at every width, how many panels show depends only on the width budget —
// never on which panels are open. With uniform 33-col slots the count is
// floor((width-40)/33), capped at the number of open panels.
func TestPanelWidthBudget_SameBudgetSameCount(t *testing.T) {
	combos := []struct {
		name  string
		setup func(*model)
	}{
		{"memory+project+git", func(m *model) { m.panelMemory, m.panelProject, m.panelGit = true, true, true }},
		{"memory+tasks+background", func(m *model) { m.panelMemory, m.panelTasks, m.panelBackground = true, true, true }},
		{"memory+tasks+background+workflow", func(m *model) {
			m.panelMemory, m.panelTasks, m.panelBackground, m.workflowPanelOpen = true, true, true, true
		}},
		{"project+git only", func(m *model) { m.panelProject, m.panelGit = true, true }},
		{"tasks+workflow", func(m *model) { m.panelTasks, m.workflowPanelOpen = true, true }},
	}
	for width := 60; width <= 200; width++ {
		want := (width - panelMinMainWidth) / 33 // uniform slots
		if want < 0 {
			want = 0
		}
		for _, tc := range combos {
			m := &model{width: width}
			tc.setup(m)
			n := len(visiblePanels(m))
			open := 0
			for _, r := range panelRenderOrder {
				if m.panelInLayout(r) {
					open++
				}
			}
			exp := want
			if exp > open {
				exp = open
			}
			if n != exp {
				t.Errorf("width %d, %s: %d panels visible, want %d (budget-only count)",
					width, tc.name, n, exp)
			}
		}
	}
}

// TestPanelWidthBudget_ReproScenarioIsSymmetric is the user's exact report:
// at a width where all three panels of one combination show, the other
// combination must show the same number (the rightmost panel loses, never a
// panel that already fit).
func TestPanelWidthBudget_ReproScenarioIsSymmetric(t *testing.T) {
	for width := 119; width <= 138; width++ {
		a := &model{width: width, panelMemory: true, panelProject: true, panelGit: true}
		b := &model{width: width, panelMemory: true, panelTasks: true, panelBackground: true}

		if got, want := len(visiblePanels(a)), 2; got != want {
			t.Errorf("width %d: memory+project+git shows %d panels, want %d", width, got, want)
		}
		if got, want := len(visiblePanels(b)), 2; got != want {
			t.Errorf("width %d: memory+tasks+background shows %d panels, want %d", width, got, want)
		}
		// In each combination it is the rightmost panel that doesn't fit.
		if a.panelVisible(regionGit) {
			t.Errorf("width %d: git must be the panel that doesn't fit", width)
		}
		if !a.panelVisible(regionProject) {
			t.Errorf("width %d: project must still fit (it precedes git)", width)
		}
		if b.panelVisible(regionBackground) {
			t.Errorf("width %d: background must be the panel that doesn't fit", width)
		}
		if !b.panelVisible(regionTasks) {
			t.Errorf("width %d: tasks must still fit (it precedes background)", width)
		}
	}
}

// TestPanelWidthBudget_ThresholdAndFloor: exactly three 33-col panels plus
// the 40-col floor fit at 139; below 73 even a single panel stays out and the
// transcript keeps the full width.
func TestPanelWidthBudget_ThresholdAndFloor(t *testing.T) {
	m := &model{width: 3*33 + panelMinMainWidth, panelMemory: true, panelProject: true, panelGit: true}
	if got := len(visiblePanels(m)); got != 3 {
		t.Errorf("at width %d all three panels fit, got %d visible", m.width, got)
	}
	if got, want := m.mainWidth(), panelMinMainWidth; got != want {
		t.Errorf("mainWidth = %d, want the floor %d", got, want)
	}

	narrow := &model{width: 33 + panelMinMainWidth - 1, panelMemory: true}
	if got := len(visiblePanels(narrow)); got != 0 {
		t.Errorf("at width %d a single panel must stay hidden, got %d visible", narrow.width, got)
	}
	if got := narrow.mainWidth(); got != narrow.width {
		t.Errorf("mainWidth = %d, want the full width %d when no panel shows", got, narrow.width)
	}
}

// TestPanelWidthBudget_PrefixAndMainAreaFloor sweeps widths and open/close
// combinations (including gaps in the open set) and asserts the two
// structural guarantees of the fill-in-order rule: the visible set is always
// a prefix of the in-layout row (no holes), and whenever any panel shows the
// transcript keeps at least panelMinMainWidth cols — exactly the budget the
// width guard protects, never the 20-col last-resort clamp.
func TestPanelWidthBudget_PrefixAndMainAreaFloor(t *testing.T) {
	combos := []func(*model){
		func(m *model) { m.panelMemory, m.panelProject, m.panelGit = true, true, true },
		func(m *model) { m.panelMemory, m.panelTasks, m.panelBackground = true, true, true },
		func(m *model) {
			m.panelMemory, m.panelTasks, m.panelBackground, m.workflowPanelOpen = true, true, true, true
		},
		func(m *model) { m.panelTasks, m.panelBackground = true, true },                      // no memory
		func(m *model) { m.panelProject, m.panelGit = true, true },                           // state panels only
		func(m *model) { m.panelMemory, m.workflowPanelOpen, m.panelGit = true, true, true }, // gap: project closed
		func(m *model) { m.workflowPanelOpen = true },
	}
	for width := 60; width <= 220; width++ {
		for ci, setup := range combos {
			m := &model{width: width}
			setup(m)

			// Prefix: once a panel is hidden, nothing after it may show.
			seenHidden := false
			for _, r := range panelRenderOrder {
				if !m.panelInLayout(r) {
					continue
				}
				v := m.panelVisible(r)
				if seenHidden && v {
					t.Fatalf("width %d combo %d: panel %v visible after an earlier in-layout panel was hidden (holes in the row)",
						width, ci, r)
				}
				if !v {
					seenHidden = true
				}
			}

			// Floor: visible panels leave (at least) 40 cols to the main area.
			used := 0
			for _, r := range visiblePanels(m) {
				used += panelWidth(r)
			}
			if used == 0 {
				continue
			}
			if got := m.mainWidth(); got < panelMinMainWidth {
				t.Fatalf("width %d combo %d: mainWidth %d below the %d-col floor with %d cols of panels open",
					width, ci, got, panelMinMainWidth, used)
			}
			if got, want := m.mainWidth(), width-used; got != want {
				t.Errorf("width %d combo %d: mainWidth = %d, want %d (width minus the visible panels)",
					width, ci, got, want)
			}
		}
	}
}

// TestAutoOpenPanel_RespectsWidthBudget: content-driven auto-open may arm a
// panel's raw flag while the budget doesn't fit it — but it must stay
// invisible (no toast, no layout move) and show up as soon as room appears.
func TestAutoOpenPanel_RespectsWidthBudget(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.panelMemory = true
	m.panelTasks = true // 66 + 73 = 139 > 120 — no room for a third panel

	m.autoOpenPanel(regionBackground)
	if len(m.toastHistory) != 0 {
		t.Errorf("auto-open must not toast a panel the width budget can't show, got %#v", m.toastHistory)
	}
	if !m.panelRawOpen(regionBackground) {
		t.Fatal("auto-open must still arm the panel's raw flag")
	}
	if m.panelVisible(regionBackground) {
		t.Fatal("background panel must stay hidden while the budget doesn't fit it")
	}

	// Closing an earlier panel frees the budget — the armed panel shows.
	m.panelTasks = false
	if !m.panelVisible(regionBackground) {
		t.Error("background panel must appear once the budget fits it again")
	}
}
