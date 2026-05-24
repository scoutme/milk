# ADR-0026 — Tool-usage UI: inline ⚙ hint line

**Status:** accepted  
**Date:** 2026-05-24

---

## Context

ADR-0018 (Step 6) deferred a tool-usage UI decision. The streaming detector
gives milk precise knowledge of when a tool call starts and ends. Before this
work, the user saw either raw markup leaking (the bug ADR-0018 fixed) or
nothing. After ADR-0018, the markup was suppressed, but tool execution was
invisible — the cursor just paused.

Three UI options were considered in ADR-0018:

1. A single dim status line while the tool is running: `⚙ bash: ls -la`
2. A collapsible viewport block: tool name + args on entry, result on exit
3. An inline transcript indicator: `[tool: bash → exit 0]`

The bridging mechanism was also open: a `ToolEvent` channel, a callback hook
on `StreamDetector`, or structured `tea.Msg` sent via `tea.Program.Send`.

## Decision

**Option 1** — a single dim hint line — was implemented for both local and
Claude agents.

### Local agent (`internal/agent/local/local.go`)

`printToolLine(out io.Writer, tc toolCall)` is called immediately before
dispatching each tool call. It writes a single ANSI dim line to `out`:

```
\n\033[2m⚙ <name>: <key-arg>\033[0m\n
```

`toolArgSummary` extracts the most informative argument value by probing
well-known keys in priority order: `command`, `path`, `file_path`, `url`,
`query`, `pattern`, `reason`, `content`. Values longer than 60 chars are
truncated with `…`. If no known key is present the name is printed alone.

The hint is written to the same `io.Writer` as the token stream, so it flows
through the existing `chunkMsg` path in the TUI with no new message type.

### Claude agent (`cmd/milk/repl.go`)

`WithOnToolUseReady(func(name string, input map[string]any))` is a callback
registered on the Claude agent at TUI startup. It is called after a
`control_request` event is received (i.e. the tool has been approved or
auto-approved), with the tool name and full input map. The TUI callback
formats the same `⚙ <name>: <key-arg>` line via `claudeToolArgSummary` and
sends it as a `chunkMsg` to the transcript.

`WithOnToolUse(func(name string))` fires on the earlier `tool_use` event
(tool started, before input is known) and sends a `toolUseMsg` — this drives
the spinner label, not the hint text.

### Why option 1 over 2 and 3

- Option 2 (collapsible block) requires a new bubbletea component and a
  decision on truncation strategy for long results; the value is low for
  typical short tool outputs.
- Option 3 (inline indicator after completion) would require threading a
  result-side event back through the stream, which is a larger change with
  little benefit over option 1.
- Option 1 is zero new infrastructure: one `fmt.Fprintf` call, same `out`
  writer, same transcript flow.

### Why callback over channel or StreamDetector hook

The callback approach (`WithOnToolUseReady`) keeps the bridging concern in
`repl.go` where the TUI state lives, and avoids leaking bubbletea types into
`internal/agent`. The detector in `internal/agent/local` writes to a plain
`io.Writer`; the hint can be written there directly without callbacks.

## Consequences

- Tool invocations are visible to the user without raw markup leaking.
- The hint appears *before* the tool executes, so the user can see what is
  happening if a tool takes time.
- No new message types or bubbletea components required.
- The hint is part of the transcript (scrollable, persistent in the viewport).
- A collapsible result summary is still possible as a future enhancement
  without changing the current approach.
