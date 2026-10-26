# ADR-0084: Session changeset in the files panel

- **Status**: Accepted, implemented
- **Date**: 2026-10-26
- **Issue**: [#331](https://github.com/scoutme/milk/issues/331)
- **Files**: `internal/session/changeset.go`, `internal/agent/local/filetrace.go`, `cmd/milk/changeset.go`, `cmd/milk/panel_project.go`, `cmd/milk/theme.go`

## Context

During a long run neither the TUI nor an operator can answer "what has this
session changed so far?" without reading the transcript or shelling out to
`git status` — and in non-git workspaces not even that works: the files panel
(`panel_project.go`) styled every entry identically and nothing tracked
session-touched paths at all.

## Decision

**One session-scoped trace + git enrichment, rendered as the files panel's
changeset.**

1. **Tracing at the dispatch boundary.** The five mutating tools
   (`write_file`, `edit_file`, `multiedit`, `delete_file`, `move_file`)
   declare their deltas in `dispatchTool`; they are recorded only when the
   tool's result carries no error. Each record is an O(1) map insert into a
   package-level registry keyed by session ID (`internal/session`) — no stat,
   hash, or diff, per the issue's performance budget. Kinds are *net-aware*:
   created-then-deleted nets to nothing, a moved file this session created
   stays `created` at its new path, `move_file` records both sides.
2. **Git as truth where it exists.** `git status --porcelain -uall` (the
   plumbing the git panel already used) runs off the UI goroutine on the
   panel's existing 3s poll — a burst of tool calls between ticks schedules
   exactly one refresh — and merges over the trace: porcelain is authoritative
   for working-tree state (it catches user-side edits and corrects kinds),
   and traced entries the index reports clean are stale (reverted or
   committed) and drop out. Directories known to be outside a repo stop the
   refresh entirely (re-probed on cwd change, session build, or re-toggling
   the panel — e.g. after a mid-session `git init`).
3. **Rendering consults, never computes.** Results land on the model from
   `Update`; `View` renders the cached lines as before. The tree builder takes
   the merged map and adds: a `changes 1A 2M 1D` summary line under the title
   (deletions never appear as rows — gone from disk — so counts are where they
   stay visible), a compact `A`/`M`/`D`/`R` glyph + per-kind color on changed
   files, and a muted `·` on ancestor directories so changes under collapsed
   branches stay discoverable. Without a changeset the rendering is
   byte-identical to the flat tree.
4. **Both skins.** `styleChange{Created,Modified,Deleted,Renamed}` are filled
   in `rebuildStyles` per skin — adaptive pairs for `off`, supermilk's own
   ANSI ramp for `supermilk`; the zero value renders plain.

## Consequences

- Non-git workspaces get first-class changeset visibility (previously
  impossible); git workspaces get real per-file status in the same place.
- The trace is in-memory and session-keyed: it never enters session JSON,
  dies with the process, is capped (4096 paths/session FIFO, 8 sessions FIFO
  in-process), and follows session switches naturally.
- `write_file` cannot tell create from overwrite without a pre-write stat
  (forbidden by the budget) — outside a repo an overwrite of a pre-existing
  file shows `A` until edited; `git status` corrects it wherever a repo
  exists. edit/multiedit are the modify tools.
- Escalation-only surfaces (claude-cli) don't cross `dispatchTool`, so in
  *non-git* workspaces their edits aren't traced — git workspaces still show
  them via porcelain.

## Alternatives considered

- **Git-only** (issue): loses non-git workspaces, where users can't run
  `git status` themselves.
- **Polling/diffing file-index snapshots**: subprocess + full diff even when
  nothing changed, and still no agent attribution without git.
- **mtime/content hashing**: O(files) per refresh — the class of problem ADR-
  0057 / issue #198 exist to avoid.
- **Showing it in the git panel**: graph-centric and repo-required; the files
  panel is where file-level navigation lives.
- **A changeset filter/toggle** (issue's optional item): deferred — the
  summary line + glyphs answer the at-a-glance question first.
