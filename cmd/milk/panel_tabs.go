package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Panel layout for every theme (ADR-0098 — the port of ADR-0079's
// docking into the hybrid budget): theme "off" and "supermilk" share one
// model — the open panels split positionally into the free ones (leftmost,
// one column each, up to the width budget) and the grouped ones (tabbed in
// the rightmost column, freshest on top, see budgetGroup below). The
// supermilk skin stays a *skin*: colours, chrome and the status bar — it no
// longer docks panels into an exclusive shared slot. What remains of
// ADR-0079's vocabulary is the view stack (panelTabMRU / activatePanelTab:
// which panel the user looked at last), feeding panelActive — the state
// file's persisted "active" panel and the preferences display.
//
// The strip a reader still sees — the group column's title row — is the
// overflow strip (#373 / ADR-0096), rendered by theme-independent
// budgetTabStrip on both themes; supermilk's old six-tab bar over every
// panel title is gone with the port.

// panelRawOpen reports a panel's raw open flag: no width guards, no tab
// filter. This is the persisted state (ADR-0078) and what notifications and
// manual overrides reason about.
func (m *model) panelRawOpen(region panelRegion) bool {
	p := m.panelOpenPtr(region)
	return p != nil && *p
}

// panelWidth is a region's rendered slot width (body + scrollbar column),
// one place for the per-region constants so layout sums and hit tests can
// never disagree.
func panelWidth(region panelRegion) int {
	switch region {
	case regionMemory:
		return memoryPanelWidth
	case regionTasks:
		return tasksPanelWidth
	case regionBackground:
		return backgroundPanelWidth
	case regionWorkflow:
		return workflowPanelWidth
	case regionProject:
		return projectPanelWidth
	case regionGit:
		return gitPanelWidth
	}
	return 0
}

// activatePanelTab pushes region onto the view stack
// (most-recently-viewed — what panelActive reads, ADR-0098). Moving a panel
// to the top is attention on par with opening it (ADR-0089 — an open toggle
// stamps):
// it stamps the panel's recency too — the same attention record an open
// toggle leaves, and where recency decides something (the budget group's
// top, ADR-0096) that is this stamp. The stack is rebuilt into a fresh slice
// every
// time: the model travels by value copy through Update, and sharing a backing
// array with the previous copy would let two renders reorder each other's
// history.
func (m *model) activatePanelTab(region panelRegion) {
	if region == regionNone {
		return
	}
	m.stampPanelActivity(region)
	mru := make([]panelRegion, 0, len(m.panelTabMRU)+1)
	mru = append(mru, region)
	for _, r := range m.panelTabMRU {
		if r != region {
			mru = append(mru, r)
		}
	}
	m.panelTabMRU = mru
}

// panelActive is the most recently viewed panel whose panel is actually
// open (raw flag — every layout predicate derives from this, so it must not
// consult the width guards, which derive from it in turn), falling back to
// panelRenderOrder for state restored or seeded without view history. This
// is "which panel did I look at last" — the state file's persisted active
// panel and the preferences display — NOT a visibility predicate: what
// occupies columns is the budget's free/grouped split (ADR-0096/0098).
// regionNone when no panel is open at all.
func (m *model) panelActive() panelRegion {
	for _, r := range m.panelTabMRU {
		if m.panelRawOpen(r) {
			return r
		}
	}
	for _, r := range panelRenderOrder {
		if m.panelRawOpen(r) {
			return r
		}
	}
	return regionNone
}

// panelInLayout is the layout membership test the width sums derive from:
// the raw open flag on every theme (the port dissolved ADR-0079's front-tab
// filter — hidden-open panels are the budget's grouped set, not residue),
// with no width guards — those derive from this, so asking them here would
// recurse.
func (m *model) panelInLayout(region panelRegion) bool {
	return m.panelRawOpen(region)
}

// panelVisible reports whether a panel occupies layout columns right now:
// in layout (raw open — panelInLayout) and within the width budget
// (panelFits: the width-derived count — optionally capped by
// max_shown_panels — the positional free/grouped split, and the group's top
// on top, ADR-0089/0096/0098). Everything that measures or joins the
// layout (mainWidth, regionAt, viewBody, panelX, panelAltBackground, the
// animation tracker) asks this — the single visibility predicate; it
// replaced a per-panel mix of guarded and unguarded checks that made the
// same width budget show three panels in one combination and only the first
// one in another.
func (m *model) panelVisible(region panelRegion) bool {
	return m.panelFits(region)
}

// anyPanelVisible reports whether any panel occupies layout columns right
// now — autoOpenPanel uses it to keep content from moving the user (the
// stamp still lands, the open may wait for a free column).
func (m *model) anyPanelVisible() bool {
	for _, r := range panelRenderOrder {
		if m.panelVisible(r) {
			return true
		}
	}
	return false
}

// The open panels split into two sets — the *free* ones (opened and
// visible, one per column) and the *grouped* ones (tabbed together in the
// last column). The split is purely positional:
// "the grouped ones are always the rightmost ones between the opened ones"
// — the leftmost open panels in panelRenderOrder fill the columns, and the
// ones past the free slots group in the rightmost column. While no more
// panels are open than columns fit there is no group at all. Recency plays
// exactly one role: between the grouped ones, the fresher one is on top —
// its content fills the group column and its label is the one styled active.
//
// The group column's title row becomes the strip — labels in fixed f1–f6
// order (panelRenderOrder): selecting a new top moves only the styling and
// the content, never the labels ("keep tab titles order unchanged — even on
// selection"). The builder's title line is replaced, never shown beside it
// (the top's name would otherwise print twice). On both themes (the port —
// ADR-0098): theme "off" renders plain title rows beside it, and supermilk's
// titles are plain too since its six-tab bar retired.
//
// A tab click only ever moves a panel to the top of its group — a grouped
// label brings that panel to the top (recency stamp, never toggled closed —
// ADR-0089's rule); the two sets themselves never change (the grouped ones
// stay the rightmost open ones). The top's own label is a no-op: clicking
// the panel you are pointing at must not move or close it — closing stays
// with the F-key / /panel press. When everything fits there is no group and
// the title rows are plain everywhere.

// budgetGroup returns the width budget's grouped set (issue #373, ported
// to every theme by ADR-0098): the rightmost open panels — the ones past
// the free slots, capped by max_shown_panels — in panelRenderOrder. Empty
// while no more panels are open than columns fit (no group at all then),
// below the one-panel floor (nothing shows).
func (m *model) budgetGroup() (group [regionGit + 1]panelRegion, n int) {
	open, no := m.panelOpenOrder()
	slots := budgetSlotCount(open, no, m.width, m.maxShownPanels())
	if slots < 1 || no <= slots {
		return group, 0
	}
	start := slots - 1 // the group column takes the last slot
	copy(group[:], open[start:no])
	return group, no - start
}

// budgetTop is the group column's panel: the freshest grouped one ("between
// the grouped ones, the fresher one is on top") — its content fills the last
// column and its label is the one styled active in the strip (label
// positions never change). Ties keep panelRenderOrder (the
// leftmost grouped panel), so a fresh session — every timestamp zero —
// shows the leftmost grouped panel. regionNone when nothing is grouped.
func (m *model) budgetTop() panelRegion {
	group, n := m.budgetGroup()
	top := regionNone
	for i := 0; i < n; i++ {
		r := group[i]
		if top == regionNone || m.panelActivity[r].After(m.panelActivity[top]) {
			top = r
		}
	}
	return top
}

// budgetTabGroupFor returns the strip labels for region's title row: the
// grouped set in panelRenderOrder — labels keep their fixed f1–f6 order and
// never move on selection (budgetTabStrip styles the top active in place) —
// but only when region is that top: only the group column carries the
// strip. nil otherwise or when nothing is grouped.
func (m *model) budgetTabGroupFor(region panelRegion) []panelRegion {
	if region == regionNone {
		return nil
	}
	group, n := m.budgetGroup()
	if n < 2 || region != m.budgetTop() {
		return nil
	}
	return append([]panelRegion(nil), group[:n]...)
}

// budgetTabGroupWidth is the plain-text width of a group's labels joined by
// one space each (styling never changes a label's cells).
func budgetTabGroupWidth(group []panelRegion) int {
	w := 0
	for i, r := range group {
		if i > 0 {
			w++
		}
		w += len([]rune(panelTabName(r)))
	}
	return w
}

// budgetStripWidth is the strip's rendered width on an inner-wide row — the
// single source shared by the strip's render (its truncate bound) and its
// click hit-test (its right edge), so what is drawn and what is clicked can
// never drift.
func budgetStripWidth(group []panelRegion, inner int) int {
	return min(budgetTabGroupWidth(group), inner)
}

// budgetTabStrip renders a group's labels — one space join, exactly like
// the retired tab bar walked its own table — with host styled as the active
// tab. Vocab that works on every theme: the host label wears the panel-title
// style underlined (tab-ness must read even where colour doesn't — theme
// "off" has no accent token and an accent-coloured code would panic on it),
// the peers dim, like the F-key hint they replace.
func budgetTabStrip(group []panelRegion, host panelRegion, inner int) string {
	var b strings.Builder
	for i, r := range group {
		if i > 0 {
			b.WriteString(" ")
		}
		name := panelTabName(r)
		if r == host {
			b.WriteString(colorize(stylePanelTitle.Render(name), "\033[4m"))
		} else {
			b.WriteString(dim(name))
		}
	}
	return ansi.Truncate(b.String(), budgetStripWidth(group, inner), "")
}

// budgetTabAt maps a content column on the host's title row to the tab label
// covering it (regionNone on a gap or past the strip) — walking the same
// single-space join from column 0 that budgetTabStrip renders with.
func budgetTabAt(group []panelRegion, inner, col int) panelRegion {
	if col < 0 || col >= budgetStripWidth(group, inner) {
		return regionNone
	}
	x := 0
	for _, r := range group {
		w := len([]rune(panelTabName(r)))
		if x <= col && col < x+w {
			return r
		}
		x += w + 1
	}
	return regionNone
}
