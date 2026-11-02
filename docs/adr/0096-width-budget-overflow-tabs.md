# 96. Width-budget overflow tabs: hidden panels surface on the rightmost panel

- **Status:** accepted
- **Date:** 2026-11-02

## Context

ADR-0089's width budget shows only the panels that fit (and of those, the
freshest). The budget itself is right — the transcript's 40-col floor and the
33-col slots exist to keep both readable — but a budget-hidden panel left **no
trace on screen**: at ~120 cols with memory + project + git open you saw one
panel and had no way to know two more were still open. The only ways back were
knowing the F-key/`/panel <name>` gesture (bring-to-front, ADR-0089) or
catching a toast (issue #347's events fire regardless of layout, but toasts
come and go). "My panel disappeared" — issue #373.

The tabbed themes (ADR-0079) had already solved the same problem for their
shared slot: a tab bar (`panelTabStrip`) whose labels are both the visible
inventory and the click targets (`panelTabAt` + `handlePanelTabClick`). The
sibling layout was the only one without a discovery affordance.

## Decision

The open panels split into two sets — the **free** ones (opened and visible,
one per column) and the **grouped** ones (tabbed together in the last
column). The split is purely positional: *"the grouped ones are always the
rightmost ones between the opened ones"* — the leftmost open panels in
`panelRenderOrder` fill the columns, and the ones past the free slots group
in the rightmost column. While no more panels are open than columns fit
(ADR-0089's count, unchanged) there is no group at all. Recency has exactly
one role: **between the grouped ones, the fresher one is on top** — its
content fills the group column and its label is the strip's active one. The
group column's title row **becomes the strip** — **labels keep the fixed
f1–f6 order (`panelRenderOrder`) and never reorder on selection**: picking a
new top moves only the styling and the content, never the labels (the
tabbed theme's bar, ADR-0079, follows the same rule — its strip has always
been the fixed table). The builder's title line is *replaced*,
never shown beside it: the top's name already appears as its own
(underlined) tab, and a title next to it would print it twice.

This supersedes ADR-0089's *selection* rule (recency-ranked membership): a
content event no longer swaps panels between columns — a free panel never
leaves its column, and a grouped panel's fresh content lifts it to the top
of its group instead (the same "show the fresh thing" outcome, without the
columns reshuffling). ADR-0089's slot count and width math stand unchanged.

- **Styling is the sibling theme's own vocabulary**: the top's label wears
  the panel-title style underlined (it is the tab on screen), the rest are
  dim — the colour the F-key hint used to own. Deliberately *not*
  `panelTabStrip`'s accent code: theme "off" has no accent token and
  `tkAccent.sgrFg()` panics on it — and the strip only ever renders on
  sibling layouts, where "off" is the theme.
- **A tab click only ever moves a panel to the top of its group**: a
  grouped label goes through the ADR-0089 bring-to-front path (recency
  stamp), so the group column shows it; the two sets themselves never
  change (the grouped ones stay the rightmost open ones) and nothing is ever
  toggled closed. The top's own label does nothing — clicking the panel you
  are pointing at must not move or close it. Closing stays with the F-key /
  `/panel <name>` press on a visible panel, which `handlePanelCmdToggle`
  still toggles.
- **Geometry has one source**: `budgetStripWidth` is shared by the render
  (the strip's truncate bound) and the hit-test (`budgetTabAt`'s right
  edge), mirroring how `panelTabStrip`/`panelTabAt` walk one table — drawn
  and clicked can't drift. The strip starts at column 0, exactly like the
  tabbed bar renders its own; the row is padded to the slot width after it.
  The group column is the rightmost one *by construction* — every grouped
  panel sits after every free one in `panelRenderOrder` — so the row's
  plain render-order join (`viewBody`, `regionAt`, `panelX`) already places
  it last.
- **Gates**: sibling layout only (`!panelTabsActive()` — a tabbed slot cannot
  hold grouped peers), a non-empty group (when everything fits the title row
  is byte-for-byte today's), top not mid-slide, and the click — like the
  tabbed bar's — only on the top row while the panel is scrolled home
  (`ev.Y == panelRowStart && lineIdx == 0`).

## Consequences

- A grouped panel is discoverable and one click away at narrow widths — its
  content takes the full group column as its top — without weakening the
  budget or adding any width cost (the strip lives inside the top's
  existing 32 content columns; when nothing is grouped nothing renders and
  nothing changes).
- While a strip is present the top's whole title line (title + F1–F6 hint)
  is replaced — its name shows exactly once, as its own tab, and the strip is
  that row's only interactive element (`/keys` still documents the keys).
  Selection/copy on the top's title row keeps the builder's plain title
  (the strip is composed at render time on a copy), so copied text stays
  stable.
- Tab clicks only move a panel to the top of its group — the sets never
  change (the grouped ones are always the rightmost open ones) — so nothing
  is ever lost and a misclick at narrow widths costs nothing (cycling
  through the group's panels stays available through the F-keys, which
  still toggle a visible panel closed). The free columns keep their fixed
  left-to-right order and the group column stays pinned rightmost
  throughout.
- Render and hit-test stay locked together by construction
  (`budgetStripWidth`), and `panelTabName` — one label table — is now shared
  by the tabbed bar, its hit-test and this strip.
- Moving a panel to the top is attention on par with opening it (ADR-0089):
  `activatePanelTab` stamps the same `panelActivity` an open toggle does —
  it is what picks the group's top — and the tabbed bar's two move paths
  (the F-key `/panel` switch and a tab-click onto an open tab) announce
  themselves with the same "brought to front" toast the sibling
  bring-to-front always used.
