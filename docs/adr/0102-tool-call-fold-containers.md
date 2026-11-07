# ADR-0102: Tool-call fold containers

- **Status:** Accepted
- **Date:** 2026-11-07

## Context

The transcript only ever *hinted* at a tool call — a one-line `» name:
summary` streamed by the agent (plus the inline file diff after it) — while
the full input and output always existed: `rawInput` on the live callback,
the result on `onToolResult`, and both persisted per call in the session
trail (`session.TrailCall.Args`/`Result`, the latter size-capped — the same
bytes the model consumed). The reasoning side had solved the "inspect the
rich data without flooding the view" problem two ADRs ago (ADR-0067: out-of-band
buffers spliced in as fold containers). The ask: give tools the same
inline-preview machinery.

## Decision

**Every main-turn tool call becomes a `toolBlock`** (`cmd/milk/toolblocks.go`),
a sibling of `thinkBlock` spliced by the same display pass:

- **Header** (one row by *construction*, not just by truncation — the
  ordinal hit-test counts complete headers, and `toolHeaderRe`'s `.` cannot
  span a newline, so a multi-line summary would mean an unmatchable, i.e.
  unclickable and uncollapsible, block: summaries flatten newlines at both
  producers and at `openToolBlock`, see `flattenSummary`; a heredoc command
  is the regression case): `▸ bash · git status — 34 lines`, `· running`
  until the outcome lands, `· error` on failure, `▾` while expanded. Folded
  by default; click
  the header to toggle (`toolBlockAtLine` — disjoint pattern from
  `▸ thinking · N lines`, so each family's ordinal space sees only its own
  headers); `/tools expand|collapse|view` + bare `/tools` report, busy-safe,
  TUI-only.
- **Body** = pretty-printed input, and once the outcome lands, a blank line
  and the result — *the model-visible* result (capped/spilled exactly as the
  model saw it), not an uncapped re-read.
- **Ownership — out-gated callbacks, not a flag** (`onToolCallView` /
  `onToolCallDone`, `internal/agent/local`): the callback receives the run's
  `io.Writer` and only the main turn's stream (`isMainTurnOut` unwraps the
  whole chain dispatch actually builds — `touchWriter` → `activityWriter` →
  the tags writers → `sendWriter`) yields a message; a `true` return
  suppresses that call's streamed hint + diff, `false` leaves the stream
  byte-identical. Jobs, workflow buffers and agent-as-tool copies (built from
  `da.agentWiring`, never given the callbacks) keep their `»` lines exactly as
  before — the same spawn-registers-interest shape as ADR-0100, minus any
  model-set flag. The gate must unwrap every transparent wrapper or it fails
  closed on *real* turns while passing hand-built fixtures — the original
  two-type version returned false for every dispatched turn (no block ever
  created, tools kept `»`) because dispatch wraps in `activityWriter` and
  `local.Run` in the tags writers; pinned by
  `TestToolBlockView_RealProductionChain`, which drives a real turn through
  the real chain.
- **The container replaces the hint on main turns**: `toolCallStartMsg` opens
  the block where the hint used to land, and the inline file diff renders
  right after the block — the position it had behind the hint. The message
  also carries `hintText` (the exact hint bytes), which
  `workflowDisplaySend` translates into the workflow live view's plain hint
  chunk; `toolCallEndMsg` is dropped there (outcome delivery is main-only;
  an unknown id is a no-op either way).
- **Lifecycle**: closed at turn end (a cancelled call closes outcome-less —
  args-only body stays truthful), turn-boundary tidy folds the previous
  turn's blocks while `toolFoldOpen` is false (`/tools expand` sets the
  status and opts out — the ADR-0067 rules verbatim), and a `/resume` switch
  clears the blocks with the transcript they anchor into. Shared fold
  surface (`ansiThinkBg` — one "inspectable region" family; accent header +
  plain body distinguish execution from reasoning's muted italic).

## Consequences

- Full input/output is inspectable per call, live, without the transcript
  ever carrying it inline; `· running` is the liveness signal (replacing the
  status-bar-only hint for local turns, which now also name the tool).
- **Resume parity with thinking**: blocks are session-view state, not
  replayed from the trail on restart — browsing *persisted* trails
  (`/tools view` over `sess.History`) is the possible follow-up, as is a
  dedicated keybinding (Ctrl+T stays reasoning; headers are clickable).
- `AskUserQuestion` keeps its suppression (milk's selection prompt owns that
  interaction — no container, matching the old "keep the transcript clean"
  rule). ACP is untouched: its host drives `tool_call` events itself.
- Retraction surgery now shifts both families (`adjustBlockAnchors`), and the
  display splice sorts both by anchor with creation-order tie-break
  (`blocksForRender`).
