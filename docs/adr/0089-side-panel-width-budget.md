# 89. Uniform side-panel width budget: fill-in-order with a 40-col floor

- **Status:** accepted
- **Date:** 2026-10-27

## Context

All six side panels are 33-col slots (`panelSlotWidth`), but visibility was
governed by two incompatible rules:

- **project, git, workflow** (added with ADR-0066-era panels) carried an
  explicit width guard: render only if `width ≥ (every other open panel) +
  own width + 40` — i.e. keep a usable 40-col transcript. `workflowPanelVisible`
  even hand-rolled its sum (memory/tasks/background only) while project/git
  used `sidePanelsWidthExcept`.
- **memory, tasks, background** (older panels) had **no guard at all** — they
  rendered at any width and the transcript fell to `mainWidth`'s 20-col
  last-resort clamp (the joined panel row can even overflow the terminal
  below ~119 cols).

User report at a terminal in [119, 138] cols: opening *memory + project +
git* showed **only memory** (the guarded pair needed 139), while *memory +
tasks + background* showed **all three** (unguarded) — the same total panel
width (99 cols), two different behaviours. Which panels you open decided how
many fit, rather than the width budget.

## Decision

One predicate, `panelFits` (`cmd/milk/layout.go`), shared by all six panels:

- Walk `panelRenderOrder` (memory → tasks → background → workflow → project →
  git) over the panels in layout (`panelInLayout`: raw open + tabbed
  front-tab filter, ADR-0079). Each panel claims its width while every panel
  before it fits too; the walk stops before the first panel that would push
  the main area below **`panelMinMainWidth` (40 cols)**.
- `panelVisible` becomes a thin wrapper over `panelFits` — the single
  visibility predicate for measuring, joining, hit-testing and the animation
  tracker. The three per-panel guards and `sidePanelsWidthExcept` are
  deleted; `panelOpen` is removed (its only consumer, `autoOpenPanel`, now
  asks `panelVisible`).

Properties this buys:

- **Count is budget-only.** With uniform slots, how many panels show is
  `floor((width − 40) / 33)` capped by how many are open — identical for
  every panel combination at the same width (the reported inconsistency
  disappears: both scenarios above show two panels at their width).
- **No cliff.** Whatever doesn't fit is the tail of the row: a shortage (or
  a newly opened panel) can only push out panels *after* the first one that
  no longer fits; panels before it keep showing.
- **Transcript floor holds.** Whenever any panel shows, the main area keeps
  at least 40 cols — exactly the budget the old guards protected — so the
  20-col clamp stays unreachable through panel layout.
- **Tabs fall out naturally.** On a tabbed theme only the front tab is in
  layout, so it is the first (only) panel in the walk.
- **Auto-open respects the budget** (`autoOpenPanel`): content may arm a
  panel's raw flag while it doesn't fit, but no toast and no layout move
  until it actually shows (a shrink while open hides it silently; a later
  widen or closing an earlier panel reveals it — raw-flag-keyed polls keep
  the data ready).

## Alternatives considered

- **All-or-nothing** (apply the old project/git rule everywhere): one
  threshold, but toggling a fourth panel could hide the three already
  showing, and at the reporter's width *zero* panels would render where the
  expectation was "not every panel fits, but some do".
- **Always render every open panel** (drop the guards): what you toggle is
  what you get, but below ~119 cols the transcript collapses toward the
  20-col clamp and the joined row overflows the terminal width — broken
  wrapping rather than a hidden panel.

## Consequences

- At widths below 73, even a single open panel stays out (previously
  memory/tasks/background rendered and squeezed the transcript).
- In the mid range, one panel of a three-panel combination may now hide
  where the unguarded trio used to render — by design, it is the price of a
  rule that never depends on panel identity.
- `panel_width_budget_test.go` pins the count-is-budget-only property across
  widths 60–200 for five combinations, the repro scenario's symmetry, the
  threshold (139) and floor (73), the prefix/no-holes and floor invariants,
  and budget-aware auto-open.
