# ADR-0011: Claude pipe-only mode with stdin disconnect

Date: 2026-05-08

## Status

Accepted

## Context

Claude Code CLI has two output modes:

- **Interactive TUI** (default when stdin is a TTY): full Ink-based terminal UI with spinners, menus, and tool-approval prompts rendered via cursor positioning and alternate screen sequences.
- **Structured JSON stream** (`--print --output-format stream-json`): NDJSON events emitted to stdout, no UI chrome.

milk needs to consume Claude's output programmatically (stream text tokens, detect session IDs, detect error states). The structured stream is the only viable path for that. However, Claude detects TTY mode via its stdin fd — if stdin is a TTY, it launches the interactive TUI even when `--print` is passed.

A VT100 filter approach was explored: run Claude in a PTY, feed raw bytes through a `midterm` emulator, suppress Ink TUI chrome (spinners, separator lines, status bar), and forward only clean text lines. This was implemented in full on `feat/claude-pty` but abandoned for the following reasons:

- Chrome detection required an ever-growing allowlist of UI-specific strings and Unicode characters that varied across Claude versions.
- Spinner frames and partial lines leaked through during high-activity turns.
- Tool approval prompts are suppressed by Claude when structured output is requested, so the VT approach couldn't solve the original approval problem either.
- The PTY path added ~300 lines of fragile plumbing with no clear maintenance path.

## Decision

Always run Claude in pipe mode (`--print --output-format stream-json --verbose`) and explicitly set `cmd.Stdin = nil` before starting the subprocess. A nil stdin causes Go's `exec.Cmd` to connect the child's stdin to `/dev/null` (a non-TTY), so Claude never detects an interactive terminal and always emits structured JSON regardless of milk's own terminal state.

The VT filter implementation is preserved on the `feat/claude-pty` branch for future reference.

Tool approval prompts are handled separately via `--dangerously-skip-permissions` (config flag) and the proactive/reactive permission system (ADR-0012).

## Consequences

- Claude's interactive TUI is never shown to the user; all output is plain text streamed from the JSON events.
- The `creack/pty` and `vito/midterm` dependencies are removed from `go.mod`.
- Tool approval prompts do not reach the user via Claude's own UI — this is an intentional trade-off addressed by ADR-0012.
- If a future Claude CLI version changes its TTY detection logic, this approach may need revisiting.
