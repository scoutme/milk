# 22. Claude Percept Skill: Nonce Tags, Consumer Hints, and Bidirectional Memory

Date: 2026-05-20

## Status

Accepted — partially supersedes [ADR-0016](0016-memory-system-percept-store.md)

## Context

ADR-0016 established that "Claude only reads" memory — relevant Percepts are injected at turn start
via `--append-system-prompt`, and the write path belongs exclusively to the local agent via
`record_memory`. In practice this created an asymmetry: Claude would produce important facts during
coding and analysis turns that the system had no way to capture without the user or local agent
manually calling `record_memory` after the fact.

A naive write path — giving Claude direct tool access to `record_memory` — creates two problems:

1. **False captures during explanation.** Claude frequently explains its own output format, writes
   documentation, or quotes instructions. If the write tag is a static string like
   `<milk:percept>`, Claude's explanation of how to use the system would trigger an actual record.

2. **Context compaction.** Claude Code compresses context during long sessions. A system prompt
   injected only at session start may be silently dropped. If the memory instruction is lost,
   Claude can no longer emit facts for the rest of the session.

## Decision

**Nonce-tagged percept emission.** Claude is instructed via the `MemoryInstruction` fragment
(appended to every `--append-system-prompt`) to emit facts using session-specific tags:

```
<milk:percept:NONCE>fact</milk:percept:NONCE>
```

The nonce is a 6-character alphanumeric string generated fresh per session by
`claude.GenerateNonce()`. Because the nonce is not known ahead of time and changes each session,
it cannot appear in pre-written explanations or code examples — only live responses contain the
correct nonce.

**Stream interception via `perceptWriter`.** When `StreamOpts.OnPercept` and `PerceptNonce` are
set, the `out io.Writer` passed to `Stream` is wrapped in a `perceptWriter`. This FSM-based writer
buffers bytes until it recognises a complete `<milk:percept:NONCE>…</milk:percept:NONCE>` sequence,
strips the tag from the display output, and calls `OnPercept(content, consumerHint)`. Tags may span
multiple `Write` calls; partial tag bytes are buffered and flushed at stream end. An unclosed open
tag (stream ended before the matching close tag) is silently discarded.

**Consumer hints.** The tag body may be prefixed with `@local: ` or `@claude: ` to restrict which
agent receives the percept at injection time. `consumerHintFrom` strips the prefix and returns the
body and hint label. `ConsumerLocal` percepts are filtered out when building Claude's `[Remembered
facts]` block; `ConsumerClaude` percepts are filtered out of the local agent's context.

**Re-injection on every `RunResume`.** The `MemoryInstruction` fragment (with its nonce) is
appended to `--append-system-prompt` on every Claude turn, including `--resume` turns. This ensures
the instruction survives context compaction: even if Claude's context window is compressed and the
original instruction is dropped, the next turn re-injects it.

**`BuildContext` percept injection.** `BuildContext(sess, nonce, percepts []string)` accepts an
optional list of content strings. When non-empty, they are rendered as a `[Remembered facts]` block
appended after the `MemoryInstruction`. This is the read path for Claude: top-k Percepts filtered
by `ConsumerClaude` or `ConsumerAll` are passed in at turn start.

## Consequences

Claude can now contribute to the shared memory store as a first-class writer. Facts recorded by
Claude arrive via `ProducerClaude` and start at `W = 0.7`, subject to the same decay/promote cycle
as local-agent Percepts (ADR-0020). The nonce mechanism prevents spurious captures and keeps the
write path auditable: every Claude-recorded Percept has `ProducerClaude` attribution.

The static `<milk:percept>` tag (no nonce) is kept as a legacy fallback for the zero-nonce code
path but is never used in production; production code always generates a nonce via
`GenerateNonce()`.

`perceptWriter` and `stripPerceptTags` handle arbitrary chunk boundaries; this is exercised by
`TestPerceptWriter_SplitAcrossWrites` and related tests in `internal/agent/claude/stream_test.go`.
