# 95. Panel content events: per-event notifications, edge-based layout

- **Status:** accepted
- **Date:** 2026-11-01

## Context

Issue #347 observed that the **git** and **project** panels were the two
exceptions to milk's panel story ("content appears → panel appears",
ADR-0044/0059/0078/0087): no `autoOpenPanel` call site existed for
`regionGit`/`regionProject`, and while closed the panels received no data at
all — `gitRefreshMsg` early-returned on `!m.panelGit`, which also killed the
3s poll chain, so there was not even an event source to trigger from. The
issue was filed as *evaluate first*: content-driven opening of *state*
panels (a working tree merely going dirty) may be more surprising than
helpful, and a naive wiring would have made "close the panel" a lie —
`panelContentFinalized` returned unconditionally `true` for both regions, so
a manual close's veto (ADR-0059) was dropped immediately and the next
dirty tick reopened the panel.

The same audit showed a second, broader gap: only the background panel had
event toasts. Tasks, workflow, memory, git and project state changes were
silent unless a panel happened to open.

Two requirements from the user shaped the decision:

1. **The manual-close veto must be released by the panel domain's own "no
   contents" abstraction** — a clean working tree for git. Closing the git
   panel sticks until the dirtiness comes *from a clean previous state*.
2. **Toasts are per-event and granular** ("as in background events"). Edge
   semantics belong to the closed-state override (the layout layer), not to
   notifications: a repeat occurrence toasts again.

## Decision

Split panel content handling into two layers with deliberately different
semantics (`cmd/milk/panel_events.go`):

### Notification layer — per event, granular

Every *observed* domain occurrence gets its own toast, whether the panel is
open or closed, and is never swallowed by the auto-open / master-switch /
tab-slot gates. Vocabulary per panel, in the background panel's
spawned/done/failed granularity:

- **git**: `branch switched to <b>`, `working tree has N change(s)`,
  `working tree clean`
- **project**: `N file(s) (…) changed this session`, `session changeset clean`
- **tasks**: `task created/removed: "<title>"`, `task "<title>": <status>`,
  `all tasks done`
- **workflow**: `started / finished / failed / interrupted / paused at its
  iteration limit / is waiting for your answers`
- **memory**: `N percept(s)`, `no percepts left` (percept-count changes
  only — context-brick redraws are not content events)
- **background**: unchanged (already the model)

The first observation of each domain is a **baseline** and stays silent:
state that pre-existed at startup is not news.

### Layout layer — edges and state only

- **Auto-open edges**: content arriving *from a clean previous state* —
  clean→dirty and branch switches for git, empty→non-empty for the project
  changeset. Dirty-count drift while already dirty never moves the layout.
- **Manual-override release is domain-specific**: `panelContentFinalized`
  now reads each panel's "no contents" — git: `gitStatusDirty == 0` (clean
  tree; unknown and non-repo count as clean — nothing is in flight),
  project: empty merged changeset. A close therefore sticks exactly while
  dirt is live; the observed clean state drops the veto without
  force-opening, and the *next* clean→dirty edge opens the panel again.
  This generalizes ADR-0059's rule ("the veto covers exactly the live
  content it was dismissed over") to state panels.

### Event source — always on, cheap

- The changeset probe (`git status --porcelain -uall -b`, off the UI
  goroutine, 3s timeout, non-repo short-circuit cached) now carries the
  branch header and dirty count too, and the 3s poll chains run for every
  session with both panels closed — the memoryPollTick cost precedent
  (one bounded git subprocess per tick). Only the *display* fetches (file
  listing, commit graph) stay gated on the panel being open; an auto-opened
  panel is refreshed by its own event.
- Agent-caused writes in non-git workspaces get a zero-poll wakeup:
  `session.SetFileChangeListener` (fired from `RecordFileChange` /
  `RemoveFileChange`, outside the registry lock) pokes the TUI, coalesced
  into one changeset sync per 250ms burst.

## Consequences

- "Close the panel" now means "stay closed over this dirt" for git/project,
  the same contract the lifecycle panels already had.
- Startup in a dirty repo is silent and does not move the layout; the
  transition into dirt is what announces itself.
- Every session pays one `git status` per 3s while idle (none outside a
  repo); the graph/listing fetches remain open-panel-only.
- In a repo the git and project event texts are two views of one underlying
  `git status` (tree state vs. which files); both panels announce their own
  domain, accepted as the cost of "every panel notifies its events".
