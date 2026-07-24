# 4. Context Handoff via --append-system-prompt-file (split static/dynamic)

Date: 2026-05-05
Updated: 2026-06-03

## Status

Accepted — updated to split-file approach

## Context

When escalating from the local agent to Claude, the local conversation history must be transferred. Options are: (a) a separate LLM reformulation call, (b) template-based summarization, or (c) passing the raw transcript to Claude directly.

The original implementation passed a single `--append-system-prompt` (later `--append-system-prompt-file`) containing everything: identity block, instructions (with a per-turn nonce), and the dynamic summary. This guaranteed a cache miss on every turn because the nonce changed and `LastLocalSummary` changed on each primary→escalation transition.

## Decision

Pass context as two separate `--append-system-prompt-file` flags:

1. **Static file** — `BuildStaticContext`: `NeedInstruction` + `MemoryInstruction` + percepts. Uses a per-session stable nonce (`sess.EscalationNonce`, generated once at `ContextModeFirst`). This file is byte-identical across turns and hits Claude's prompt cache.

2. **Dynamic file** — `BuildDynamicContext`: identity block + escalation brief + current need + `LastLocalSummary`. On `ContextModeFirst` when a prior escalation session exists (fresh-start forced by staleness), `LastEscalationSummary` is also injected as a `[Prior escalation session summary]` block so Claude retains key context from the dropped session. Changes per turn but is suppressed when content is unchanged (hash guard). Only re-sending this small file does not invalidate the large static prefix.

Claude orients itself from this context without a separate reformulation step.

## Consequences

Cache hits on the static instruction prefix across all resume and returning turns. Only the dynamic summary (small, frequently empty on RESUME turns) changes between turns. The nonce must now be persisted in the session file (`EscalationNonce`) rather than regenerated per turn. The `BuildContext` function is kept as a deprecated compatibility wrapper over the two split functions.
