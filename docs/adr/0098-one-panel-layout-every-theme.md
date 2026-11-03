# 98. One Panel Layout for Every Theme: the Hybrid Budget Everywhere

- **Status:** accepted
- **Date:** 2026-11-03

## Context

Two layout models had accumulated for the six side panels:

- **ADR-0079 (supermilk)** — tabbed docking: the theme's six-tab strip made
  the panels tabs of *one shared slot*: exactly one visible panel, exclusive
  navigation (moving to a panel closed the one you left), auto-open only
  into a free slot, and a front-tab filter plus an exclusivity enforcer to
  keep multi-open state out of layout.
- **ADR-0096 / issue #373 (theme "off")** — the hybrid budget: the width
  budget splits the *open* panels **by position** into free columns and a
  rightmost grouped column whose title row becomes a strip; the freshest
  grouped panel is on top; tab clicks move a panel to the top of its group
  and never close anything; content events lift a grouped panel to the top
  instead of reshuffling columns.

The models disagreed on everything layout-shaped — what is visible
(front-tab vs. positional), what a move does (close vs. top-of-group),
whether content moves columns — and the two themes answered differently to
the same window size. The request on #373: the hybrid "would fit either
classic and supermilk", plus a config knob to keep the one-column
experience for whoever wants it.

## Decision

**One model for every theme — ADR-0096's hybrid, overflow-only** (the
option the requester picked: group only what the width can't hold; the
alternatives — an always-one-tabbed-column supermilk, or a knob-only opt-in
— were considered and rejected in favor of a single rule):

- **The theme is a skin.** supermilk keeps its colours, chrome, status bar
  and diff surface; it no longer docks, filters or closes panels. The
  six-tab bar (`panelTabStrip` and its hit-test) is deleted; free columns
  carry the plain title row on **both** themes, and the group column's
  strip (ADR-0096's, `budgetTabStrip`) is the only tab strip left. At
  widths where everything fits there is no strip anywhere — supermilk's
  panel area is skin-only, exactly like the requester's chosen option.
- **Positional split, freshest-on-top, fixed f1–f6 labels, forward-only
  clicks** govern both themes with unchanged semantics (see ADR-0096):
  `panelInLayout` is the raw open flag everywhere; the front-tab layout
  filter is gone; an F-key / `/panel` press on a grouped panel brings it to
  the top (stamped + "brought to front", the same event class as opening),
  and a press on a visible panel closes it — theme-independent.
- **Exclusivity retires with the docking.** `closeAllButPanelTabs`,
  `enforceTabbedPanelExclusivity` (startup + `/theme` collapse) and the
  exclusive-slot branches in `handlePanelCmd`/the mouse handler/auto-open/
  the animation tracker are deleted. Hidden-open panels are the budget's
  grouped set, not residue; multi-open state restores as-is on any theme.
  What survives of ADR-0079's machinery is the **view stack** —
  `panelTabMRU` / `activatePanelTab` / `panelActive`: "which panel did I
  look at last", persisted as the state file's `active` panel and read by
  the preferences display. It never decides visibility; the budget does.
- **`max_shown_panels`** (config, `0`/absent = the width-derived count)
  caps the column count inside `budgetSlotCount`, read live from the
  session config on every layout pass — the hot-reload rule (apply now,
  never restart) gives it immediate effect. `1` is the one-column
  experience on any theme: every open panel behind the group column's
  strip, still switchable by clicking. A cap never *adds* columns beyond
  what fits.

## Consequences

- **One layout code path.** `panelFits` → `budgetSlotCount` →
  `budgetGroup`/`budgetTop` → strip render/hit-test are theme-independent;
  the theme decides styles only. The parity test pins it by construction:
  the same seeds produce the same visible set and group on both themes.
- **Supermilk's default changes** (this is the port): wide supermilk shows
  sibling columns like theme "off" does. Anyone keeping its old behavior
  writes `max_shown_panels: 1` — reproducing the single column minus the
  exclusive-close semantics (navigation moves the top instead of closing
  panels behind it, which the requester's rules require anyway).
- **The persisted `active` panel no longer implies visibility** — restore
  puts it on the view stack; the budget decides whether it shows (wide
  enough, it does).
- ADR-0079 is superseded (its note points here); ADR-0096 loses its
  sibling-only scope and the `!panelTabsActive()` gate; ADR-0089's count
  math is untouched but may now be capped by config.
- Test churn replaces the docking pins (share-one-slot, exclusive moves,
  instant tab swap, slot-free auto-open) with parity, no-close navigation
  and cap pins — behavior the earlier suite *pinned as wrong* under the
  new model.
