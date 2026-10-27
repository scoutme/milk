package main

import (
	"context"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/memory"
)

// Tests for the unified side-panel width budget (ADR-0089): one rule shared
// by all six side panels — how many fit is a pure function of the width
// (with a 40-col transcript floor), and *which* fit is decided by content
// recency (last content/attention event, ties keeping panelRenderOrder). The
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
// structural guarantees of the budget. These bare models carry no recency
// stamps — the startup/restored baseline every timestamp ties there — so the
// relevance rank degenerates to panelRenderOrder and the visible set is the
// prefix of the in-layout row (no holes). Second: whenever any panel shows,
// the transcript keeps at least panelMinMainWidth cols — exactly the budget
// the width guard protects, never the 20-col last-resort clamp.
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

// TestAutoOpenPanel_SwapsIntoTheBudget: content is the relevance basis
// (ADR-0089) — even when every slot is taken, the panel whose content just
// changed claims one, evicting the least recently relevant panel; below the
// one-panel floor there is no slot to swap into, so nothing renders or
// toasts (the raw open is still recorded).
func TestAutoOpenPanel_SwapsIntoTheBudget(t *testing.T) {
	m := layoutTestModel(t, 120, 40) // budget: 2 slots (106 <= 120 < 139)
	m.panelMemory = true
	m.panelTasks = true // both never-updated → tie → fill order (memory, tasks)

	m.autoOpenPanel(regionBackground)
	if !m.panelRawOpen(regionBackground) {
		t.Fatal("auto-open must arm the panel's raw flag")
	}
	if !m.panelVisible(regionBackground) {
		t.Error("fresh content must swap its panel into the budget even when every slot is taken")
	}
	if !m.panelVisible(regionMemory) {
		t.Error("memory (rank 2: behind background, ahead of tied tasks by render order) must keep its slot")
	}
	if m.panelVisible(regionTasks) {
		t.Error("the stalest panel (no event yet, last in render order) must be the one evicted")
	}
	if !toastMentions(m, "background panel opened") {
		t.Errorf("a panel that just became visible should toast its open, got %#v", m.toastVisible)
	}

	// Below the one-panel floor (73 cols) there is no slot to swap into.
	narrow := layoutTestModel(t, 60, 40)
	narrow.autoOpenPanel(regionTasks)
	if !narrow.panelRawOpen(regionTasks) {
		t.Error("auto-open must still record the raw open state")
	}
	if narrow.panelVisible(regionTasks) {
		t.Error("no panel can fit at width 60")
	}
	if len(narrow.toastHistory) != 0 {
		t.Errorf("no toast for a panel that cannot show, got %#v", narrow.toastHistory)
	}
}

// TestPanelActivity_RecencySwapsPanels: the visible set is the budget's
// most-recently-updated panels (ties keep panelRenderOrder = the startup
// baseline), and every stamp switches panels on its event — the stalest one
// leaves. This is the "show the more relevant info" behaviour the recency
// basis exists for.
func TestPanelActivity_RecencySwapsPanels(t *testing.T) {
	m := &model{width: 130, panelMemory: true, panelProject: true, panelGit: true} // 2 slots
	if !m.panelVisible(regionMemory) || !m.panelVisible(regionProject) || m.panelVisible(regionGit) {
		t.Fatalf("tie baseline: memory+project fill the budget, git waits; got %v", visiblePanels(m))
	}

	m.stampPanelActivity(regionGit) // git's content changed
	if !m.panelVisible(regionGit) || !m.panelVisible(regionMemory) || m.panelVisible(regionProject) {
		t.Errorf("after git update: git (newest) + memory (beats tied project) must show; got %v", visiblePanels(m))
	}

	m.stampPanelActivity(regionProject) // project's content changed next
	if !m.panelVisible(regionProject) || !m.panelVisible(regionGit) || m.panelVisible(regionMemory) {
		t.Errorf("after project update: project+git show, the stalest (memory) leaves; got %v", visiblePanels(m))
	}

	// Widening the terminal grows the budget — nothing stays hidden.
	m.width = 139
	if len(visiblePanels(m)) != 3 {
		t.Errorf("at 139 cols all three panels fit regardless of recency; got %v", visiblePanels(m))
	}
}

// TestPanelToggle_StampedAsMostRelevant: opening a panel by hand is an
// attention event — the panel the user just asked for wins a slot
// immediately, evicting the stalest, even though the budget was full.
func TestPanelToggle_StampedAsMostRelevant(t *testing.T) {
	m := layoutTestModel(t, 130, 40) // 2 slots
	m.panelMemory = true
	m.panelProject = true

	next, _ := m.handlePanelCmdToggle("git")
	m = next.(model)
	if !m.panelRawOpen(regionGit) {
		t.Fatal("toggle must open the git panel")
	}
	if !m.panelVisible(regionGit) {
		t.Error("the panel the user just opened must claim a slot right away")
	}
	if !m.panelVisible(regionMemory) || m.panelVisible(regionProject) {
		t.Errorf("eviction takes the stalest (project, no event yet); got %v", visiblePanels(&m))
	}
}

// TestMemoryRefresh_StampsOnlyOnChange: the 5s refresh is a poll, not an
// event — a no-op tick must never outrank panels with real news (first tick
// = baseline only; a real store change stamps; an unchanged tick keeps the
// previous stamp).
func TestMemoryRefresh_StampsOnlyOnChange(t *testing.T) {
	m := layoutTestModel(t, 200, 40)
	store, err := memory.NewStoreParams(t.TempDir(), "sess", memory.Params{})
	if err != nil {
		t.Fatal(err)
	}
	m.mem = store

	next, _ := m.Update(memoryRefreshMsg{})
	m = next.(model)
	if !m.panelActivity[regionMemory].IsZero() {
		t.Fatal("the first tick only establishes the content fingerprint — no stamp")
	}

	if _, err := store.Record(context.Background(), "a fresh percept", memory.ProducerUser, memory.ConsumerAll, memory.Roles{}, false); err != nil {
		t.Fatal(err)
	}
	next, _ = m.Update(memoryRefreshMsg{})
	m = next.(model)
	if m.panelActivity[regionMemory].IsZero() {
		t.Fatal("a real store change must stamp the memory panel")
	}
	stamped := m.panelActivity[regionMemory]

	time.Sleep(5 * time.Millisecond) // distinct clock reading if a re-stamp happens
	next, _ = m.Update(memoryRefreshMsg{})
	m = next.(model)
	if !m.panelActivity[regionMemory].Equal(stamped) {
		t.Error("an unchanged tick must not restamp — it would pin the memory panel as freshest forever")
	}
}

// TestGitDataMsg_StampsOnlyOnGraphChange: the 3s git poll is also cosmetic
// most of the time — the first load sets the baseline (startup ties keep
// fill order), a real graph/branch change stamps, an identical re-poll
// doesn't.
func TestGitDataMsg_StampsOnlyOnGraphChange(t *testing.T) {
	m := layoutTestModel(t, 200, 40)
	m.panelGit = true

	next, _ := m.Update(gitDataMsg{branch: "main"})
	m = next.(model)
	if !m.panelActivity[regionGit].IsZero() {
		t.Fatal("the first data load is the baseline — it must not stamp")
	}

	next, _ = m.Update(gitDataMsg{branch: "main", err: "not a git repository"}) // error note renders → lines differ
	m = next.(model)
	stamped := m.panelActivity[regionGit]
	if stamped.IsZero() {
		t.Fatal("the graph content changing is real content change — it must stamp")
	}

	time.Sleep(5 * time.Millisecond)
	next, _ = m.Update(gitDataMsg{branch: "main", err: "not a git repository"}) // identical refresh poll
	m = next.(model)
	if !m.panelActivity[regionGit].Equal(stamped) {
		t.Error("a no-change poll must not restamp — polling would keep git fresher than live events elsewhere")
	}
}

// TestPanelShortcut_BringsHiddenPanelToFront: pressing /panel <name> or its
// F1-F6 shortcut for a panel the width budget hid is explicit user interest
// — the strongest relevance signal there is (ADR-0089). The press must bring
// that panel into the layout, evicting the stalest one, and must NOT toggle
// it closed: the user pressed the key to *see* it. Only once it is on screen
// does the next press close it (the plain toggle). Below the one-panel floor
// no stamp can reveal anything, so there the press falls back to the plain
// toggle — the shortcut always does something.
func TestPanelShortcut_BringsHiddenPanelToFront(t *testing.T) {
	m := layoutTestModel(t, 130, 40) // budget: 2 slots (106 <= 130 < 139)
	m.panelMemory = true
	m.panelProject = true
	m.panelGit = true // tie baseline → memory+project show, git hidden

	if m.panelVisible(regionGit) {
		t.Fatal("baseline: git must start hidden (budget of 2, render-order tie)")
	}

	next, _ := m.handlePanelCmd("git")
	m = next.(model)
	if !m.panelRawOpen(regionGit) {
		t.Fatal("the press must not close a panel the user just asked to see")
	}
	if !m.panelVisible(regionGit) {
		t.Error("the pressed panel must take a slot back — user interest outranks content recency")
	}
	if !m.panelVisible(regionMemory) || m.panelVisible(regionProject) {
		t.Errorf("eviction takes the stalest (project, no event yet); got %v", visiblePanels(&m))
	}
	if !toastMentions(m, "git panel: brought to front") {
		t.Errorf("bring-to-front should explain itself, got %#v", m.toastVisible)
	}

	// Now that it is shown, the next press is the plain toggle again.
	next, _ = m.handlePanelCmd("git")
	m = next.(model)
	if m.panelRawOpen(regionGit) {
		t.Error("a second press on the visible panel must close it")
	}

	// Below the one-panel floor (73 cols) there is no slot to swap into:
	// the press falls back to the plain toggle so it still has an effect.
	narrow := layoutTestModel(t, 60, 40)
	narrow.panelTasks = true
	if narrow.panelVisible(regionTasks) {
		t.Fatal("baseline: no panel fits at width 60")
	}
	next, _ = narrow.handlePanelCmd("tasks")
	narrow = next.(model)
	if narrow.panelRawOpen(regionTasks) {
		t.Error("with no room to show it, the press falls back to closing the panel")
	}
}
