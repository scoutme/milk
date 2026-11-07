# 66. Project tree and git graph panels

Date: 2026-10-19

Status: Accepted

## Context

Issue #206: the TUI's side panels covered memory, tasks, background and workflow progress, but gave no view of the *project* itself — users left the TUI to see the file layout or the git history/branch structure, even though most sessions work on exactly those. The issue proposed one or both of a project tree and a git graph, using the existing panel framework (regions, per-region mouse scoping, sticky offsets, the F-key/`/panel` toggles).

## Decision

Two new panel regions, `project` (`/panel project`, **F5**) and `git` (`/panel git`, **F6**), right of the workflow panel — following the panel framework at every seam (`sidePanelLines`, `panelOffsetPtr`, `panelMaxOffset`, `panelSelLines`, `handlePanelMouse`, the alternating-tint order). No new framework concepts.

**Project tree** — the session cwd's files from the same candidate index the `@`-file completion uses (`fileindex.go`, ADR-0057: `git ls-files` when available, pruned walk otherwise). This sidesteps the walk-budget problem outright and gives `.gitignore` handling for free. Directories start expanded at the top level and collapsed below it, with explicit click toggles recorded per directory; rendering is capped (1500 rows) with a truncation note. **Click cadence — one machine for every side panel** ([ADR-0103](0103-keyboard-panel-focus.md)'s click-to-focus applied uniformly): the **single click's job is focus** — deferred past the 400 ms double-click window, never an action — so browsing the tree (or any panel) cannot dump content off a stray click, and **no double click ever moves focus**: the deferred single is abandoned, the panel's action runs untouched. **Directories expand/collapse on the double click** — uniform with file rows (folders demanded the same rule). A **file previews on the same double click** within 400 ms — as the file's working-tree diff when it has one (modified → `git diff HEAD`, untracked → whole-file added diff via `--no-index`; one `git status --short` probe decides), raw contents otherwise (the ADR-0047 attach view over a static `livebuf`, new `attachPreview` kind). A **triple click** on the same file inserts an `@`-mention into the input (the gesture the double click used to own, pushed one click along). Binary and oversized files are refused/capped rather than dumped into the view.

**Git graph** — `git log` plumbing (`%h %H %P %s %D %ar`) with the lanes **laid out in code** (`layoutGraph`, `gitgraph.go`) instead of parsing git's own `--graph` ASCII. The reasons the issue cared about this apply directly: every row's commit hash is known structurally, so click-to-preview needs no text scraping, and the layout is a pure function — unit-tested (linear, merge, branch-merge-back) without a repo. Commit dot `●`, straight lanes `│`, fan-out corners `╮`/`╭`, merge-in corners `╯`/`╰`. Header shows the current branch and dirty count (`git status --porcelain -b`); **double-clicking** a commit attaches a capped `git show --stat --patch` preview, and **double-clicking** the header line itself attaches the working tree's complete local state (`git status --short` — untracked included — plus the full diff vs HEAD; a clean tree answers `working tree clean`) — a single click in the panel only focuses it (the deferred-focus cadence above: previews must not steal a click's focus job). Outside a repo (or without git) the panel shows a one-line note.

**Refresh** — "live-ish" per the issue: a 3 s tick while either panel is open refetches off the UI goroutine (the file index is TTL-cached and single-flight; git runs a few short processes). Data lands in the model via `projectDataMsg`/`gitDataMsg`, and the derived `lines`/`ids` arrays are rebuilt only in Update — View renders a value copy, so caches must never be written on the render path (selection, scroll clamping and rendering all read the same arrays, like `buildPanelLineIDs` does for memory).

## Alternatives considered

- **One merged "project" panel with tree/graph tabs**: rejected — the two views are consulted side by side (e.g. "what changed on this branch" + "where is that file"), and the framework's region model is per-panel.
- **fsnotify-driven refresh**: rejected for now — the walk-budget and watcher-lifecycle costs don't pay for a panel whose content is navigational, not streaming. The tick + the file index's TTL is enough; can be revisited.
- **`git log --graph` ASCII parsing**: rejected, see above.
- **Auto-open on session start**: rejected — the panels are navigational; nothing "becomes active". Manual toggle only, so the ADR-0059 override machinery sees them as always-finalized.

## Consequences

- The file tree reflects the index, not live disk state mid-tick (≤ ~3 s old, same as `@`-completion); untracked-but-not-ignored files show up because the walk fallback and `git ls-files --others` both contribute.
- The graph shows the current branch's history (`git log`, not `--all`) — remote branches appear as decorations. `--all`/reflog views are future work.
- `panel_manual_override` applies to the new panels like the rest; since their content is always "finalized", the override drops as soon as the panel is closed (never blocks a later auto-open — and none is scheduled).
