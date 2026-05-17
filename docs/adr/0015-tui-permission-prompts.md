# ADR-0015: TUI-native permission prompts via blocking goroutine + channel

Date: 2026-05-14

## Status

Accepted

## Context

ADR-0013 wired permission prompts through an `inputReader` interface backed by `stdinInputReader` — a `bufio.Scanner` reading from `os.Stdin`. This worked in the original readline REPL because the TUI was suspended during agent execution (`tea.Exec`), so stdin was available.

With the full TUI (ADR-0014), `tea.Exec` is gone and the alt-screen is always live. The agent runs in a goroutine. Reading from `os.Stdin` inside that goroutine deadlocks: bubbletea owns the terminal and is reading from stdin itself; the agent goroutine never receives input.

## Decision

Replace `stdinInputReader` in interactive mode with `tuiInputReader`, which routes permission prompts through the bubbletea message queue:

### Mechanism

1. The agent goroutine calls `tuiInputReader.readLine(prompt)`.
2. `readLine` creates a `chan string` and calls `p.Send(permRequestMsg{prompt, respCh})`, then **blocks** on `<-respCh`.
3. `Update` handles `permRequestMsg`: stores it as `model.pendingPerm`, appends the prompt text to the transcript, and resets the textarea.
4. While `pendingPerm != nil`, key events are routed to `handlePermKey` instead of the normal handler. Enter sends `m.ta.Value()` to `respCh` and clears `pendingPerm`. Ctrl-C sends `"n"`.
5. The agent goroutine unblocks, receives the answer, and continues.

### Why blocking the goroutine is acceptable

The agent goroutine is already blocking on I/O (Claude subprocess or llama.cpp HTTP). Blocking it additionally on a channel does not consume a thread — Go's goroutine scheduler parks it. The TUI remains responsive throughout.

### Why not a second message type for the response

Sending a `permResponseMsg` from `Update` back to the goroutine would require a shared channel stored on the model, which is copied on every `Update` call. A channel on a pointer field of the model survives copies; the `permRequestMsg` itself carries the response channel, so no model field is needed.

## Consequences

- Permission prompts appear inline in the transcript, consistently with all other output.
- The TUI remains live and responsive during permission prompts — the spinner continues, the viewport is scrollable.
- The agent goroutine is parked (not spinning) while waiting for user input.
- `stdinInputReader` is still used for single-shot (non-REPL) mode where the TUI is not running.
- If the user force-quits (Ctrl-C twice) while a prompt is pending, the goroutine leaks briefly until the context is cancelled by the program exit. This is acceptable.
