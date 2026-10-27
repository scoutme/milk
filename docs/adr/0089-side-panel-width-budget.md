# 89. Side-panel width budget: recency-ranked panels over a 40-col floor

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

Fixing the *count* was only half the question: when not every open panel
fits, static position decides *which* ones show — a panel far left can hold
its slot for hours while fresher information waits invisibly behind it. The
product direction: pick the panels by **last update timestamp**, so the
layout switches panels on events and always shows the most relevant info.

## Decision

One predicate, `panelFits` (`cmd/milk/layout.go`), in two halves, shared by
all six panels:

1. **Budget — how many fit.** Walk `panelRenderOrder` (memory → tasks →
   background → workflow → project → git) over the panels in layout
   (`panelInLayout`: raw open + tabbed front-tab filter, ADR-0079). Each
   panel claims its width while every panel before it also fits; stop before
   the next panel would push the main area below **`panelMinMainWidth`
   (40 cols)**. With uniform slots this count is a pure function of width —
   `floor((width − 40) / 33)` capped by how many are open.
2. **Selection — which ones fill it.** `panelRankByActivity` sorts the
   in-layout panels by `panelActivity[region]` (last content/attention
   event) newest-first; ties keep `panelRenderOrder`. The top `budget`
   entries are the visible ones.

`panelVisible` is a thin wrapper over `panelFits` — the single visibility
predicate for measuring, joining, hit-testing and the animation tracker.
The three per-panel guards and `sidePanelsWidthExcept` are deleted;
`panelOpen` is removed (its only consumer, `autoOpenPanel`, now asks
`panelVisible`).

### What stamps recency (and what never does)

Stamping is deliberately narrow — an event must mean *content actually
changed* or *the user actually looked*, or polling would pin a panel as
freshest forever:

- **Content events, centrally in `autoOpenPanel`'s first line** — before
  every gate (master switch ADR-0087, manual override ADR-0059, tab slot):
  the content changed whether or not opening is currently allowed. This
  covers tasks, background spawn/start, workflow launch/progress/done/resume
  and memory gain, which all route through it.
- **Events that finalize instead of auto-open:** background job done,
  background shell done (status row changed).
- **Real change inside polls:** the memory refresh compares a content
  fingerprint (first tick = baseline only), the changeset tick compares the
  merged file set (first load = baseline), the git poll compares the
  rendered graph lines (first load = baseline). A no-change poll never
  stamps.
- **Attention:** a mouse press or wheel inside a *visible* panel
  (`handleMouse`) — a burst of events elsewhere can't swap a panel out from
  under the user mid-read.
- **Intent:** opening a panel by hand (`handlePanelCmdToggle`) — the panel
  you just asked for claims a slot immediately, even when the budget is full.
  A press for a panel the budget *hid* (`/panel <name>` or F1–F6) is intent
  too — the strongest kind ("user interest is a significant event for the
  panel"): it brings that panel to the front instead of toggling the panel
  closed, and only once it is on screen does the next press close it. (Below
  the one-panel floor no stamp can reveal anything, so there the press falls
  back to the plain toggle.) Tabbed-theme tab moves stamp the same way.
- **Never:** cosmetic redraws (elapsed counters), closing, startup/restored
  state (all-zero timestamps → the deterministic left-to-right baseline).

### Properties

- **Count is budget-only.** How many panels show is
  `floor((width − 40) / 33)` capped by how many are open — identical for
  every combination at the same width (the reported inconsistency
  disappears: both scenarios above show two panels at their width).
- **Fresh content always wins.** Rank 0 is inside the budget whenever at
  least one slot exists (width ≥ 73), so a content event's panel swaps in
  and the least recently relevant panel steps out — panels switch *on
  events*, not on position.
- **User interest wins outright.** A shortcut press (F1–F6 or `/panel
  <name>`) for a budget-hidden panel swaps it in as the freshest candidate
  instead of toggling it closed — the layout answers the user before it
  answers the feed.
- **Deterministic baseline.** With every timestamp tied (startup, restored
  layout, panels that never saw an event) the rank degenerates to
  `panelRenderOrder` — the original left-to-right fill, as tie-break only.
- **Transcript floor holds.** Whenever any panel shows, the main area keeps
  at least 40 cols; the 20-col clamp stays unreachable through panel layout.
- **Tabs fall out naturally.** On a tabbed theme only the front tab is in
  layout, so it is the only candidate — recency is irrelevant there
  (ADR-0079 unchanged).
- **No flapping.** Only substantive changes stamp, so swaps track real
  events (a workflow holding a slot *while it streams* is the point; when
  its stream ends, the next event elsewhere overtakes it).
- **Ranking is O(6), allocation-free** (stack array + insertion sort) so
  the per-render `panelVisible` calls stay inside ADR-0088's budget.

## Alternatives considered

- **Static fill-in-order as the selection rule** (the first implementation
  of this ADR): position decides, not relevance — a stale panel holds its
  slot while fresher content waits invisibly. Superseded by the recency
  direction: "a last-update timestamp basis is the better rationale; it
  switches panels on events and shows the more relevant info".
- **Touch-on-visible / LRU on render:** rejected — self-reinforcing. Every
  rendered panel would refresh its own stamp and never be evicted, so
  hidden fresh content could never get in: the exact opposite of the goal.
- **All-or-nothing** (apply the old project/git rule everywhere): one
  threshold, but toggling a fourth panel could hide the three already
  showing, and at the reporter's width *zero* panels would render where the
  expectation was "not every panel fits, but some do".
- **Always render every open panel** (drop the guards): what you toggle is
  what you get, but below ~119 cols the transcript collapses toward the
  20-col clamp and the joined row overflows the terminal width — broken
  wrapping rather than a hidden panel.
- **Decay curves / dwell-time hysteresis:** rejected as over-engineering —
  strict event ordering plus non-stamping cosmetic refreshes already gives
  meaningful, non-flapping swaps without tunable constants.

## Consequences

- At widths below 73, even a single open panel stays out (previously
  memory/tasks/background rendered and squeezed the transcript).
- In the mid range, which panels of a three-panel combination show now
  *changes on every event* — the visible set is live, positions on screen
  are still fixed by `panelRenderOrder` (only membership moves, riding the
  existing open/close transition animation).
- Recency only ranks panels in layout: stamping a closed panel (content
  events hit closed panels too) is inert until the panel is opened, and
  opening stamps anyway.
- A panel can become visible without a toast when it merely swaps in as
  already-raw-open; toasts still only fire on a raw-closed → visible open
  (`autoOpenPanel` unchanged there).
- `panel_width_budget_test.go` pins the count-is-budget-only property across
  widths 60–200 for five combinations, the repro scenario's symmetry, the
  threshold (139) and floor (73), the tied-baseline prefix and floor
  invariants, recency swaps (content event in / stalest out, widen
  reveals), hand-open claiming a slot, shortcut bring-to-front (hidden panel
  swaps in on its keypress, the second press closes it, and below the
  floor the press falls back to the plain toggle), budget-aware auto-open,
  and the change-only stamping of the memory and git polls.
