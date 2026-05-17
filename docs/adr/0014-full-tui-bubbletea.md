# ADR-0014: Full TUI with bubbletea viewport+textarea

Date: 2026-05-14

## Status

Accepted — partially superseded by [ADR-0019](0019-tui-input-history-and-turn-cancellation.md) (input locking and history)

## Context

The readline-based REPL (ADR-0010) had fundamental ordering problems once bubbletea was introduced for other UI work:

- `tea.Exec` (suspend/resume) made output ordering uncontrollable: echoed input could appear after agent output, spinner frames leaked into the transcript, and slash command output could be overwritten by the next render cycle.
- Multi-line input was impossible with readline's single-line model.
- There was no persistent transcript — the terminal scrollback was the only history.
- The permission prompt (ADR-0013) required suspending the TUI to read from stdin, which left the screen in an undefined state.

The root cause was architectural: readline and bubbletea have incompatible ownership models. readline owns the terminal; bubbletea owns the terminal. Running both simultaneously requires `tea.Exec` to explicitly hand over control, and the handoff boundary is where ordering breaks.

## Decision

Replace the readline REPL entirely with a persistent bubbletea alt-screen TUI:

### Layout

```text
┌──────────────────────────────────────────┐
│  transcript viewport  (scrollable)       │
├──────────────────────────────────────────┤
│  status bar: session · agent · cwd       │
├──────────────────────────────────────────┤
│  [local] > █                             │
└──────────────────────────────────────────┘
```

- `charmbracelet/bubbles/viewport` for the transcript pane
- `charmbracelet/bubbles/textarea` for the input area
- Status bar rendered as a lipgloss-styled line between the two

### Agent execution: goroutine + p.Send(), not tea.Exec

Agent turns run in a goroutine. Output is streamed to the TUI via a `sendWriter` that calls `p.Send(chunkMsg{})` on each write. The TUI never suspends — the alt-screen stays live throughout.

`tea.Exec` was explicitly rejected: it suspends the TUI, hands the terminal to a subprocess, and resumes — which means the TUI can't render during execution and ordering is determined by when bubbletea decides to flush, not by when writes happen.

### Streaming

`sendWriter` implements `io.Writer` and calls `p.Send(chunkMsg{text})` on each `Write` call. The `Update` handler appends `chunkMsg.text` to the transcript immediately, so the viewport updates as tokens arrive. Both the local agent (llama.cpp SSE stream) and the Claude agent (NDJSON stream) were updated to write tokens as they are received rather than buffering until completion.

### Transcript wrapping

The viewport does not wrap content. Long lines are wrapped using `ansi.Wrap` (from `charmbracelet/x/ansi`) before being set on the viewport, so ANSI escape sequences in agent output are handled correctly. The transcript is re-wrapped on every append and on terminal resize.

### Mouse and selection

Application-level mouse capture (bubbletea `WithMouseCellMotion`) was tried and reverted. It blocks native terminal selection, making word/character-level copy impossible. The decision is to not capture the mouse at all and let the terminal handle selection natively — this gives the user standard click+drag selection for any granularity, which is strictly more capable than line-granularity application selection.

### Multi-line input

`ctrl+n` is the most reliable binding — works across all WSL terminals. `shift+alt+enter`, `alt+enter`, and `altgr+enter` also work; `shift+enter` alone is unreliable in WSL (arrives as plain `\r`). All are wired via `textarea.KeyMap.InsertNewline.SetKeys`.

### Input locking

The input area is locked (key events dropped) while an agent turn is in progress. Ctrl-C still works to force-quit. Ctrl-C on a non-empty input clears the field first, then clears force-mode, then quits.

## Consequences

- Multi-line input is supported.
- Output ordering is deterministic: echo → streamed tokens → newline, all appended to the same transcript in arrival order.
- The permission prompt can be handled entirely within the TUI (see ADR-0015).
- The binary grows by the viewport/lipgloss dependency footprint, but these were already indirect dependencies.
- Native terminal selection replaces the application-managed copy mode that was briefly implemented and then removed.
- `tea.Exec`, `replExec`, and `activityWriter` are removed. The spinner is now driven by `tea.Tick`.
