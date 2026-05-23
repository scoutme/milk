# 20. Memory System Refinements: Reachable Promotion and User-Stated Facts

Date: 2026-05-18

## Status

Accepted — partially supersedes [ADR-0016](0016-memory-system-percept-store.md); constants further re-tuned in [ADR-0025](0025-memory-decay-constant-retuning.md)

## Context

ADR-0016 established `promoteThreshold = 0.8` as the weight at which a session-scoped Percept is promoted to `global.json` during NREM consolidation. In practice, Percepts recorded by the local agent or Claude start at `W = 0.7` (the default for non-user producers). After one session-end decay of `−0.03`, their weight drops to `0.67` — permanently below the 0.8 threshold. The GLOBAL (non-core) store was therefore unreachable via normal session activity: only `/learn` (which writes Core=true, W=1.0 directly to global) could populate it.

Additionally, the `record_memory` tool had no way to distinguish a user-stated fact from an agent-inferred observation. Both received `W = 0.7`, regardless of whether the content came from "the user said X" or the agent's own inference. Users stating explicit preferences had no advantage over incidental agent observations.

## Decision

**Lower `promoteThreshold` from 0.8 to 0.6.** A local/claude-produced Percept at `W = 0.67` after one decay now exceeds the threshold and is promoted to GLOBAL on the next consolidation run. Percepts that are never reinforced decay further (`0.67 → 0.64 → ...`) and will eventually be pruned at `W ≤ 0`, so the GLOBAL store remains self-cleaning.

**Add `producer` field to `record_memory`.** The tool now accepts an optional `producer` parameter (`"user"` | `"local"` | `"claude"` | `"system"`). When an agent passes `producer: "user"`, the Percept is recorded at `W = 1.0` (the `ProducerUser` weight constant) instead of the default `W = 0.7`. This matches the weight assigned by `/learn`, making it possible for agents to faithfully record user-stated facts with the appropriate confidence level.

## Consequences

The GLOBAL (non-core) store is now reachable via normal session activity. A local agent or Claude turn that records a Percept and does not contradict it over subsequent sessions will eventually see it promoted without any explicit `/learn` call from the user.

User-stated facts recorded via `record_memory` with `producer: "user"` start at `W = 1.0` and survive consolidation reliably, mirroring the semantics of `/learn` without requiring the user to issue a slash command.

Risk: the lower threshold means more Percepts may promote to GLOBAL. This is mitigated by the decay model — local/claude Percepts at `W = 0.67` are only marginally above the new threshold and will be pruned if unused across sessions. The Core=true + W=1.0 path (via `/learn` or `producer: "user"`) remains the authoritative mechanism for facts that must never be lost.
