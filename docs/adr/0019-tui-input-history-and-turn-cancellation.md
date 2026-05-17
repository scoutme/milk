# ADR-0019: TUI input history persistence and turn cancellation

Date: 2026-05-17

## Status

Accepted

## Context

ADR-0014 established the bubbletea TUI but left two gaps:

1. **Input history was in-memory only.** Restarting milk meant navigating a blank history, which is particularly painful in session-heavy workflows.
2. **Ctrl-C during an agent turn exited the whole program.** There was no way to interrupt a slow or stuck turn and return to the prompt without killing the process.

Additionally, different directories and sessions should maintain independent histories — pasting a command from a previous project's session is confusing and error-prone.

## Decision

### Turn cancellation

Each agent turn now runs under a per-turn `context.WithCancel` stored in `model.cancelTurn`. Ctrl-C while `model.busy == true` calls `cancelTurn()` instead of `tea.Quit`. The agent goroutine returns when the context is cancelled; `agentDoneMsg` fires with a cancellation error, which is suppressed in favour of printing `[interrupted]` (dim) to the transcript. Input unlocks normally. The existing Ctrl-C idle behaviour (clear field → clear force-mode → quit) is unchanged.

### Persistent input history: session + global

Two files are maintained in parallel:

| File | Path | Scope |
| --- | --- | --- |
| Session history | `~/.milk/sessions/<session-id>.history` | Entries typed in this session |
| Global history | `~/.milk/input_history` | Entries across all sessions |

Both are plain text, one entry per line, capped at 500 entries (oldest pruned on write). Every submitted entry is appended to both (consecutive duplicates are skipped). On startup both files are loaded; on exit (normal or interrupted) both are written back.

Navigation (Up/Down, Ctrl+Up/Ctrl+Down) defaults to **session history**. The `/history global` command switches to global; `/history session` switches back; `/history` shows the current mode and entry counts.

Session history is the default because it matches the user's mental model: navigating history within a session should surface what was typed in that session, not an unrelated command from another directory.

### Incremental search (Ctrl+R / Ctrl+S)

A lightweight three-state search mode is implemented inside the bubbletea model (no external library):

- `Ctrl+R` enters reverse incremental search (newest → oldest).
- `Ctrl+S` enters forward incremental search (oldest → newest).
- While searching, printable characters extend the query and immediately jump to the nearest match.
- `Ctrl+R` / `Ctrl+S` while already searching move to the next older / newer match without clearing the query.
- `Enter` accepts the current match and exits search mode, leaving the matched entry in the textarea ready to submit or edit.
- `Esc` / `Ctrl+C` cancels and restores the pre-search textarea content.
- The status bar shows `(reverse-i-search)\`query'` or `(forward-i-search)\`query'` while active.

The search operates on `model.activeHistory()`, which returns either session or global history depending on the current mode.

The pure search functions (`searchBack`, `searchForward`, `appendDeduped`) are extracted as package-level functions so they can be unit tested independently of the TUI model.

## Consequences

- Restarting milk preserves the input history for the session and globally.
- Different sessions (different directories or explicit `--session` flags) have independent per-session histories.
- A slow or hung agent turn can be cancelled with Ctrl-C without killing the process.
- Ctrl-S is used for forward search, which may conflict with terminal flow-control (XON/XOFF) on some systems. In practice, bubbletea's alt-screen mode disables flow control, so this is not an issue in the target WSL environment.
