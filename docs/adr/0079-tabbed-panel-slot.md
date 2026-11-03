# 79. Tabbed Panel Docking: One Shared Slot When the Theme Draws Tabs

- **Status:** accepted
- **Date:** 2026-10-25

> **Superseded by [ADR-0098](0098-one-panel-layout-every-theme.md)
> (2026-11-03):** the hybrid budget port (#373 / ADR-0096) runs on every
> theme. The shared slot, exclusive navigation, the front-tab layout
> filter, the exclusivity enforcer and the six-tab bar are retired;
> supermilk is a skin again. What remains of this ADR's machinery is the
> view stack (`panelTabMRU` / `activatePanelTab` / `panelActive`) feeding
> the persisted active panel — visibility itself is the budget's.

## Context

The supermilk skin (ADR-0077) gives every side panel the same title row: the
six-tab strip (`panelTabStrip` — memory, tasks, jobs, flow, files, git), with
the panel's own tab in the accent colour. What the panels did *not* do is
what a tab bar means: they were still laid out **side by side**, so with
memory and git open the user got two copies of the six-tab strip over two
sibling columns, each highlighting a different tab. Six tabs over six
stacked panels is a tab bar; six copies of it over six sibling columns is
not. The report was direct: *if the theme uses tabs, panels should occupy the
same spot*.

## Decision

When the theme renders the tab strip (exactly `th.modern` — the two are tied
so the tab bar and the layout can never disagree, `panelTabsActive()`), the
side panels are **tabs of one shared slot**:

- **One slot, one panel.** Exactly one panel occupies layout columns —
  the *front tab* (`panelActive`) — and tabs are **exclusive**: moving to a
  panel closes the one you moved away from (see the amendment below), so a
  tabbed theme never runs hidden-open siblings. Everything that measures or
  joins the layout (`mainWidth`, `regionAt`, `viewBody`, `panelX`,
  `panelAltBackground`, the animation tracker) derives from `panelVisible`
  (open + width-permitted + front tab), so multi-open states seeded outside
  the activation paths — sibling-session state restored before the theme is
  known, a `/theme` switch from siblings — can never render side by side
  either; `enforceTabbedPanelExclusivity` collapses them to the front panel
  at startup and on `/theme`. `panelOpen` stays the plain "is this panel
  open" for state, persistence and notifications. The alternating
  background invariant holds trivially: the single panel in the slot is
  always the first one, so it always differs from the chat.
- **Exclusive navigation.** F1-F6 / `/panel <name>` and a click on a tab
  label open the target panel *after closing whatever was in the slot*
  (`closeAllButPanelTabs`); on the front panel the F-key still closes it,
  leaving the slot empty. The navigation close is not a dismissal — the
  user's manual override (ADR-0059/0078) is untouched, so panels the user
  opened themselves still stick over auto-open. A press on an open-but-
  hidden panel (only reachable as multi-open residue) brings it forward
  *and* closes the rest, so the invariant repairs itself on first use; the
  click hit-test (`panelTabAt`) walks the same join the strip renders with.
- **No fallback on close.** With exclusivity, closing the front tab empties
  the slot — there is no sibling to hand it back to. `panelTabMRU` remains
  only to pick the front among whatever open panels a legacy multi-open
  state hands the loader (`active` in the folder's panel-state file).
- **Auto-open takes a free slot only.** Content-driven auto-open (ADR-0044)
  fires while the shared slot is empty — "panel opened" must mean the user
  can see it — and never while another panel is shown: live content (every
  workflow chunk, every task update) cannot yank the view off the panel the
  user is looking at, and content for the shown panel updates it in place.
- **Tab switches animate nothing.** The slot stays occupied and only its
  content changes: a browser-like instant swap. The existing slide-in /
  shrink-out choreography (ADR-0076) still runs for what it was built for —
  the slot appearing (first panel) and disappearing (last panel).
- **The front tab is a preference.** The panel-state file (ADR-0078, now
  folder-bound) gained an
  optional `active` key, written only for manually chosen panels (the same
  rule as the open flags: auto-opened state is not a preference) and restored
  at startup. A stale `active` naming a closed panel is harmless — the front
  falls back to the first open panel.

Without tabs (theme `"off"`) none of this applies: panels stay siblings,
joined left-to-right in `panelRenderOrder`, and every predicate degenerates
to the plain open flag — the pre-existing behaviour is untouched.

## Consequences

- Opening several panels on a tabbed theme costs one panel width of screen
  instead of N, and switching is a click on a tab or a press of its F-key.
- Panels you move away from are closed — their flags go false (the layout
  collapses to the single slot) and they come back with their F-key or tab
  click; scroll offsets and manual preferences survive in the model/state
  file as before. There is no hidden-open panel to close separately.
- Arriving from a sibling layout (theme `"off"`, or a restored
  multi-open state) collapses to one panel: the MRU front if history says
  so, otherwise the leftmost open panel.
- The tab bar is the panel's first content row, so it scrolls with the
  panel's content like any title row — a tab click is only possible while the
  panel is scrolled to the top. Pinning the bar to the panel's top edge would
  change every panel's line indexing (selection, click → row); left for a
  follow-up if it bothers anyone.

## Amendment (2026-10-25): tabs are exclusive

The first cut kept moved-away panels open behind their tabs, browser-style,
with an MRU fallback handing the slot back when the front tab closed. The
follow-up report was direct: *when using tabbed panels, moving to a panel
must close the previously opened ones.* Tabs are therefore exclusive — the
Decision above reflects the amended behaviour: every activation closes what
it displaces (`closeAllButPanelTabs`, a navigation close that never touches
the user's manual override), closing the front tab empties the slot instead
of reviving the MRU, and content-driven auto-open is limited to a free slot
so it still never moves the user. Residue from outside the activation paths
is collapsed by `enforceTabbedPanelExclusivity` (after the theme is known at
startup, and on `/theme`).
