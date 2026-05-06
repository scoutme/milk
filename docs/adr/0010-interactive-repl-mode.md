# ADR-0010: Interactive REPL mode

## Status
Accepted

## Context

milk was initially single-prompt only: one invocation, one response, exit. For iterative work (debugging sessions, multi-step code edits) this creates friction — the user must retype flags and wait for session load on every turn. An interactive mode removes that overhead.

## Decision

`milk` with no prompt argument enters a REPL loop. Key design choices:

### Readline library: chzyer/readline over peterh/liner

`liner` was the first choice (minimal dependency) but it rejects ANSI escape codes in prompt strings via a control-character validation step, making colored prompts impossible without pre-printing the label and calling `Prompt("")` — which breaks line redraw on backspace.

`chzyer/readline` accepts ANSI codes natively in the prompt string and redraws them correctly on every keystroke. It also exposes a `Painter` interface for colorizing the input buffer as the user types.

### Prompt label reflects routing state

The prompt label (`[local] >`, `[claude] >`, `[claude:waiting] >`) uses the same color scheme as the agent speaker labels on output — green for local, blue for Claude, yellow for claude:waiting. This makes the current state visible without extra status lines.

### Slash commands for in-session control

Force-mode flags (`--escalate`, `--local`) become `/escalate` and `/local` slash commands. Other commands: `/new`, `/drop`, `/list`, `/help`, `/exit`. Slash prefix is conventional for chat-style REPLs and avoids ambiguity with prompt text.

### Tab completion

- `/` prefix: completes slash commands
- `@` prefix: completes file paths from cwd using `os.ReadDir`

The `@path` token is passed as-is in the prompt text to the agent rather than being expanded — the agent resolves it. This keeps the completion lightweight (no file reads at completion time) and preserves the original intent in session history.

### Painter: colorize tokens as typed

`milkPainter.Paint` colorizes `/commands` in yellow and `@paths` in dim as the user types, using the `Painter` hook in readline's `Config`. This provides visual feedback without altering the submitted string.

### Ctrl-C / Ctrl-D behaviour

- Ctrl-C (`ErrInterrupt`): if a force-mode flag is active (`/escalate` or `/local`), clears it and stays in the session; otherwise exits
- Ctrl-D (`io.EOF`): always exits

## Consequences

- The readline dependency adds ~400 KB to the binary.
- Completion candidates cannot be individually colorized — readline's candidate display loop is hardcoded (not hookable without forking).
- The `@path` pass-through means the local model must understand the `@` convention; this is documented in the system prompt context passed on escalation.
