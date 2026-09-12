# 44. Side-Panel Auto-Open, Shortcut Hints, and Alternating Background

- **Status:** accepted
- **Date:** 2026-09-12

## Context

The memory panel opens by default; tasks, background-agents, and workflow are
opt-in via `/panel <name>` or F1-F4 (ADR-0043). The workflow panel already had
an exception: four call sites unconditionally set `workflowPanelOpen = true`
whenever `workflow.ProgressMsg`/`WorkflowDoneMsg`/`workflowResumeCheckMsg`
fired, so it opened itself once a workflow started, but tasks and
background-agents had no equivalent — a task created or a background job
spawned mid-turn produced no visible signal beyond a transcript line and the
status-bar count, easy to miss with panels already competing for screen
width.

The workflow panel's existing auto-open was also unconditional: it reopened
every time regardless of whether the user had just pressed F4 to close it,
which is what closing a panel is supposed to mean.

Separately, with four panels now toggled by F1-F4 (ADR-0043), remembering
which key maps to which panel is not obvious from the panel itself, and with
several panels open side by side, only the scrollbar column separates them —
easy to misread which columns belong to which panel.

## Decision

### Auto-open with sticky manual override

Generalize the workflow panel's ad-hoc auto-open into one shared mechanism
and extend it to tasks and background-agents:

- `(*model).autoOpenPanel(region)` (`panel_common.go`) opens a panel unless
  the user has explicitly shown or hidden it this session.
- `model.panelManualOverride map[panelRegion]bool` records that fact,
  set in `handlePanelCmd` — the single handler behind both `/panel <name>`
  and its F-key, so both paths record the same override.
- New trigger points call `autoOpenPanel` instead of setting the panel field
  directly: `Manager.SetOnStart` (`internal/agent/local/background.go`,
  new — fires the instant `Spawn` creates a job, before it even acquires a
  concurrency slot) → `backgroundJobStartedMsg`; `tasks.Store.SetOnChange`
  (fires on create/update/complete/delete) → `taskStoreChangedMsg` (renamed
  from the reused `memoryRefreshMsg`, which conflated "redraw the memory
  panel's poll tick" with "something in the task store changed" — two
  unrelated signals that happened to share a message type). The four
  existing workflow-panel auto-open sites were switched to go through
  `autoOpenPanel` too, so they now respect the same override.

Once a region is in `panelManualOverride`, no automatic trigger touches it
again for the rest of the session — in either direction. This applies
per-session, not persisted: a fresh session starts with no overrides, so a
newly-created task still opens the tasks panel even if the user closed it in
a previous session.

### Shortcut hint in the title

`panelTitleLine(title, region, inner)` (`panel_common.go`) renders the
existing title left-aligned and a dimmed F-key hint right-aligned within the
panel's inner width (e.g. `tasks    F2`), truncating the title rather than
the hint if both don't fit. `panelShortcut(region)` is the single source for
the region → F-key mapping, mirroring the order F1-F4 are wired in in
`repl.go`'s key handler. All four `build<Name>PanelLines` functions route
their title line through it.

### Alternating background tint

Every other currently-open panel, counted left-to-right in `View()`'s join
order (memory, tasks, background, workflow — not a fixed parity per region),
renders with a subtle background tint so adjacent panels are easier to tell
apart at a glance. `panelAltBackground(region)` recomputes this from the
live open/closed state on every render, so closing a panel in the middle of
the sequence correctly reshuffles which of the *remaining* open panels are
tinted, rather than leaving two adjacent panels sharing a background by
coincidence.

`panelAltBackgroundCode()` picks a small, fixed step off pure black/white —
`lipgloss.HasDarkBackground()` only tells us which direction to step, not
the terminal's actual background color, which lipgloss/termenv can't read
back.

**The reset problem.** Every existing panel style — this codebase's
hand-rolled ANSI helpers (`dim`, `green`, `red`, `bold` in `ansi.go`) and
`lipgloss`'s own `Render()` — closes a styled span with a full SGR reset
(`\x1b[0m`), which clears background too, not just foreground. Confirmed
live: lipgloss emits the identical `\x1b[0m`. Naively wrapping a fully
assembled panel line in a background style would therefore show the tint
only in plain-text stretches and drop it the instant the line hits any
dim/colored badge, title, or section header — i.e. most of the content.
`withPanelBackground(line, bg)` fixes this by re-injecting `bg` immediately
after every embedded `\x1b[0m` (`strings.ReplaceAll`) rather than wrapping
the line once, so the tint survives behind previously-styled spans. Applied
in the shared `renderSidePanel`/`renderSidePanelScrollbar` after padding, so
one implementation covers all four panels and their scrollbar columns.

One gap accepted rather than special-cased: the memory panel's
`staleContentColor` gradient (the fresh→stale legend and percept/brick
tinting) opens its truecolor spans with a combined reset+set code
(`\x1b[0;38;2;r;g;bm`) rather than a plain reset, so the tint briefly drops
out behind just those specific characters. Single existing call site,
barely visible in practice, not worth a second code path.

## Consequences

- Tasks and background-agents panels now behave like workflow's pre-existing
  auto-open, and workflow's own auto-open now respects a prior explicit
  close — three call sites' worth of copy-pasted "just set it true" logic
  collapsed into one shared, override-aware helper.
- `taskStoreChangedMsg` replaces `memoryRefreshMsg` for task-store changes;
  the memory panel's own periodic-tick redraw is unaffected (still
  `memoryRefreshMsg`, now used for exactly one thing).
- Live-verified in the real TUI (not just simulated `Update()` tests):
  F1-F4 hints render in real panel titles, the alternating tint's ANSI
  survives real lipgloss-rendered spans (adaptive 256-color titles, dim
  hints), a fresh session's tasks panel opens itself on the model's first
  `create_task` call, and a manually-closed tasks panel correctly stays
  closed through a second `create_task` call in the same session.
