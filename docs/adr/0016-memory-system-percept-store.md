# 16. Memory System: Percept Store with NREM Consolidation

Date: 2026-05-15

## Status

Accepted — partially superseded by [ADR-0020](0020-memory-system-refinements.md) (promotion threshold, user-stated facts), [ADR-0022](0022-claude-percept-skill-nonce-tags.md) (Claude write path via nonce tags), and [ADR-0025](0025-memory-decay-constant-retuning.md) (decay/promote/prune constants re-tuned)

## Context

milk is a local CLI that routes prompts across sessions and agents. Without persistent memory, each session starts cold: the local agent has no awareness of user preferences, past decisions, or recurring facts. The alternative — injecting full session history into every prompt — bloats context and pollutes unrelated turns.

An internal RFC described a brain-inspired memory architecture (Hippocampus, Percept, Engram, NREM/REM cycles) designed for enterprise multi-service systems. milk needs the cognitive model but not the infrastructure.

## Decision

Implement `internal/memory` as a self-contained package with no imports from other milk internals. Core choices:

**Percept over key-value.** A Percept is a natural-language assertion with a confidence weight, provenance, and optional semantic roles. This models memory as statements ("user prefers flat output") rather than typed fields, which is more flexible and directly usable as LLM context injection.

**File-based store (JSON), not SQLite.** Two JSON files: `~/.milk/memory/global.json` (cross-session facts) and `~/.milk/memory/<session-id>.json` (session-scoped). Consistent with session storage (ADR-0005), human-readable, no extra dependency. Vector embeddings are deferred to Phase 2 — at that point the JSON approach can be revisited.

**Unconditional decay (Model A).** All non-Core session Percepts lose −0.03 weight per session end, regardless of whether they were used. The alternative (skip decay if used this session) risks zombie Percepts that survive indefinitely from infrequent reinforcement. Model A is simpler to reason about and ensures the store self-prunes over time.

**Session-scoped by default, auto-promote on consolidation.** Percepts start session-scoped and are promoted to `global.json` only when W ≥ 0.8 after consolidation. This prevents noise from single-turn interactions reaching long-term storage. `/learn` is the explicit override: it writes directly to global as Core=true, W=1.0, immune to decay.

**Local agent writes; Claude only reads.** The local agent has `record_memory` and `get_memory` tool access. Claude has no direct store access — relevant Percepts are injected via `--append-system-prompt` (Phase 4). This keeps the write path deterministic and avoids Claude accumulating memory outside the user's awareness.

**Store interface for extraction.** The package exposes a `Store` struct with no coupling to other milk packages. The llama.cpp embedding client (Phase 2) will be wired via a configurable function to keep the boundary clean.

## Consequences

The local agent can remember user preferences and past decisions across sessions without full history injection. `/learn` gives users a direct, inspectable way to assert persistent facts. Consolidation runs automatically at session end with no user interaction required.

The JSON store will be slow to query at large Percept counts (linear scan). This is acceptable for a personal CLI where global stores are unlikely to exceed a few hundred Percepts before Phase 2 embeddings provide indexed recall. If scale becomes a problem, the `Store` struct can be replaced with a SQLite-backed implementation without touching callers.

Role extraction and edge classification (Phase 3) are deferred — roles are stored as empty structs until the local model is called to fill them at consolidation time.
