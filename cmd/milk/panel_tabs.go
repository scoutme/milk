package main

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
// pushing it onto the most-recently-viewed stack. The stack is rebuilt into
// a fresh slice every time: the model travels by value copy through Update,
// and sharing a backing array with the previous copy would let two renders
// reorder each other's history.
func (m *model) activatePanelTab(region panelRegion) {
	if region == regionNone {
		return
	}
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
