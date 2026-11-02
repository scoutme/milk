package main

import (
	"strings"

	"github.com/charmbracelet/x/ansi"
)

// Tabbed panel docking (ADR-0079): a theme whose panel title row is a tab
// strip (theme_chrome.go's panelTabStrip — exactly the "modern" skins) means
// the panels to be tabs of *one* shared slot, not six sibling columns each
// wearing the same six tabs. Six tab labels over six stacked panels is what a
// tab bar means; six copies of the strip over six side-by-side panels is not.
//
//   - one slot, one panel: exactly one panel occupies layout columns — the
//     front tab (panelActive). Tabs are exclusive (the user's rule):
//     moving to a panel CLOSES the one you moved away from, so a tabbed
//     theme never runs hidden-open siblings. The activation paths below
//     keep that invariant; the layout predicates still filter to the front
//     tab, and enforceTabbedPanelExclusivity collapses multi-open states
//     set outside them (sibling-session state restored before the theme is
//     known, /theme switching from siblings to tabs).
//   - switching: F1-F6 / /panel <name> and a click on a tab label open the
//     target panel — whatever was in the slot is closed — and on the front
//     panel the F-key still closes it, leaving the slot empty. Content-
//     driven auto-open only fires while the slot is free, so live content
//     never moves the user off the panel they are looking at.
//   - closing the front tab leaves the slot empty: with exclusivity there
//     is no other open panel to hand it back to. panelTabMRU still picks
//     the front among the open panels a restored multi-open state hands
//     the loader — its only remaining job.
//
// Without tabs (theme "off") nothing here applies: panels stay siblings,
// joined left-to-right in panelRenderOrder, and every predicate below
// degenerates to the plain open flag.

// panelTabsActive reports whether the active theme renders the panel tab
// strip and therefore docks panels into one shared slot. Tied to th.modern
// because that is exactly the condition panelTitleLine renders the strip
// under — the tab bar and the shared slot can never disagree.
func panelTabsActive() bool { return th.modern }

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

// activatePanelTab brings region's tab to the front of the shared slot by
// pushing it onto the most-recently-viewed stack. Moving a panel to the top
// is attention on par with opening it (ADR-0089 — an open toggle stamps):
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

// panelActive is the panel in front of the shared slot: the most recently
// viewed tab whose panel is actually open (raw flag — every layout predicate
// derives from this, so it must not consult the width guards, which derive
// from it in turn), falling back to panelRenderOrder for state restored or
// seeded without tab history. regionNone when no panel is open at all.
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
// raw open flag plus the tabbed front-tab filter, with no width guards —
// those derive from this, so asking them here would recurse.
func (m *model) panelInLayout(region panelRegion) bool {
	if !m.panelRawOpen(region) {
		return false
	}
	return !panelTabsActive() || region == m.panelActive()
}

// panelVisible reports whether a panel occupies layout columns right now:
// in layout (raw open, and the front tab on a tabbed theme — panelInLayout)
// and within the width budget (panelFits, ADR-0089: count from the width,
// membership from content recency). Everything that measures or joins the
// layout (mainWidth, regionAt, viewBody, panelX, panelAltBackground, the
// animation tracker) asks this — the single visibility predicate; it
// replaced a per-panel mix of guarded and unguarded checks that made the
// same width budget show three panels in one combination and only the first
// one in another.
func (m *model) panelVisible(region panelRegion) bool {
	return m.panelFits(region)
}

// anyPanelVisible reports whether the panel slot is occupied at all — the
// animation tracker uses it to tell a slot appearing/disappearing (slide in,
// shrink out) apart from a tab swap inside an occupied slot (instant), and
// autoOpenPanel uses it to keep content from moving the user.
func (m *model) anyPanelVisible() bool {
	for _, r := range panelRenderOrder {
		if m.panelVisible(r) {
			return true
		}
	}
	return false
}

// closeAllButPanelTabs closes every open panel except keep — the
// exclusivity rule of the tabbed slot (ADR-0079): moving to a panel closes
// the ones you moved away from, so a tabbed theme never runs hidden-open
// siblings. This is a navigation close, not a dismissal: the manual
// override (the user's own preference, ADR-0059/0078) is left untouched,
// so panels the user opened themselves still stick and auto-open behaviour
// is unchanged. Reports whether it actually closed anything. No-op on a
// sibling (theme "off") layout.
func (m *model) closeAllButPanelTabs(keep panelRegion) bool {
	if !panelTabsActive() {
		return false
	}
	closed := false
	for _, r := range panelRenderOrder {
		if r == keep || !m.panelRawOpen(r) {
			continue
		}
		if ptr := m.panelOpenPtr(r); ptr != nil {
			*ptr = false
			closed = true
		}
	}
	return closed
}

// enforceTabbedPanelExclusivity collapses a multi-open panel state — state
// restored from a theme-"off" session, or /theme switching from siblings to
// tabs — to the single front panel. The activation paths keep the invariant
// themselves; this covers the two moments where panel flags are set outside
// them: right after the theme is known at startup, and on /theme. No-op
// while tabs are inactive; when it does collapse something, the new state is
// persisted right away — the collapse must not wait for the user's next
// panel action to reach the state file.
func (m *model) enforceTabbedPanelExclusivity() {
	if m.closeAllButPanelTabs(m.panelActive()) {
		m.savePanelStateReport()
	}
}

//
// Sibling layout only (theme "off"): the open panels split into two sets —
// the *free* ones (opened and visible, one per column) and the *grouped* ones
// (tabbed together in the last column). The split is purely positional:
// "the grouped ones are always the rightmost ones between the opened ones" —
// the leftmost open panels in panelRenderOrder fill the columns, and the
// ones past the free slots group in the rightmost column. While no more
// panels are open than columns fit there is no group at all. Recency plays
// exactly one role: between the grouped ones, the fresher one is on top —
// its content fills the group column and its label is the one styled active.
//
// The group column's title row becomes the strip — labels in fixed f1–f6
// order (panelRenderOrder): selecting a new top moves only the styling and
// the content, never the labels ("keep tab titles order unchanged — even on
// selection"). The builder's title line is replaced, never shown beside it
// (the top's name would otherwise print twice). Same row treatment and
// visual language as the tabbed theme's bar (ADR-0079): labels from column
// 0, the top's label in the panel-title style underlined, the rest dim.
//
// A tab click only ever moves a panel to the top of its group — a grouped
// label brings that panel to the top (recency stamp, never toggled closed —
// ADR-0089's rule); the two sets themselves never change (the grouped ones
// stay the rightmost open ones). The top's own label is a no-op: clicking
// the panel you are pointing at must not move or close it — closing stays
// with the F-key / /panel press. When everything fits there is no group and
// the title rows are byte-for-byte today's; a tabbed theme never reaches
// this code (panelTabsActive gate — its exclusive slot cannot hold
// grouped peers).

// budgetGroup returns the width budget's grouped set (issue #373): the
// rightmost open panels — the ones past the free slots — in
// panelRenderOrder. Empty while no more panels are open than columns fit
// (no group at all then), below the one-panel floor (nothing shows), and on
// a tabbed theme.
func (m *model) budgetGroup() (group [regionGit + 1]panelRegion, n int) {
	if panelTabsActive() {
		return group, 0
	}
	open, no := m.panelOpenOrder()
	slots := budgetSlotCount(open, no, m.width)
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
// strip. nil otherwise, when nothing is grouped, or on a tabbed theme.
func (m *model) budgetTabGroupFor(region panelRegion) []panelRegion {
	if panelTabsActive() || region == regionNone {
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
// panelTabStrip walks its own table — with host styled as the active tab.
// Vocab is the sibling theme's own: the host label wears the panel-title
// style underlined (tab-ness must read even where colour doesn't — theme
// "off" has no accent token, and panelTabStrip's accent code would panic on
// it), the hidden peers dim, like the F-key hint they replace.
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
