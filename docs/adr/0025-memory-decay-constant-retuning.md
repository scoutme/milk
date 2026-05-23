# 25. Memory Decay Constant Re-tuning

Date: 2026-05-24

## Status

Accepted — partially supersedes [ADR-0016](0016-memory-system-percept-store.md) and [ADR-0020](0020-memory-system-refinements.md) on consolidation constants

## Context

After observing live sessions, no Percept was ever seen to decay or be pruned. Every model inference created a Percept that immediately appeared in the global store. Investigation revealed two compounding bugs in the original constants:

1. `promoteThreshold = 0.6` was too low. A Local/Claude Percept starts at `W=0.7` and decays by `−0.03` per session. After one consolidation: `0.7 − 0.03 = 0.67 ≥ 0.6` → promoted to global. **Everything went global on first consolidation.**

2. `decayPerSession = 0.03` was too small to matter within the promotion window. Even if promotion had been disabled, it would take 23 sessions for a `W=0.7` Percept to reach zero.

3. `pruneThreshold = 0.0` meant percepts were only removed when W hit exactly zero (floating-point equality) — effectively never.

4. `initialWeight(ProducerUser) = 1.0` created Percepts that were already at or above any reasonable threshold on arrival.

The result: the session store accumulated everything, consolidation immediately promoted all of it to global, and the global store grew without bound.

## Decision

Retune all constants to implement the intended confidence-based lifecycle:

| Constant | Old | New | Rationale |
|---|---|---|---|
| `decayPerSession` | 0.03 | **0.10** | Meaningful decay over ~5 sessions |
| `promoteThreshold` | 0.6 | **0.80** | Only high-confidence facts graduate |
| `pruneThreshold` | 0.0 | **0.20** | Cull weak percepts before they accumulate |
| `initialWeight(ProducerUser)` | 1.0 | **0.9** | Promotes after 1 session; leaves room for decay |
| `initialWeight(ProducerSystem)` | 0.5 | **0.4** | Pruned after 2 sessions |
| `initialWeight(ProducerLocal/Claude)` | 0.7 | **0.7** | Unchanged; decays over ~5 sessions |

**Intended lifecycle per producer:**

- **User** (`W=0.9`): promotes after one session (`0.9 − 0.10 = 0.80 == promoteThreshold`). User-stated facts are explicit and high-confidence.
- **Local/Claude** (`W=0.7`): never promotes without edge reinforcement; pruned after ~5 sessions (`0.7 → 0.6 → 0.5 → 0.4 → 0.3 → 0.2`, pruned at step 5). Model inferences are ephemeral unless reinforced by edge propagation.
- **System** (`W=0.4`): pruned after ~2 sessions (`0.4 → 0.3 → 0.2`, pruned at step 2). System hints are low-confidence.
- **Core** (`W=1.0`): immune to decay regardless of producer.

The `/learn` command continues to write `Core=true, W=1.0` directly to global — unaffected by consolidation constants.

## Consequences

- **Global store stops growing unboundedly.** Only user-stated facts survive to global without explicit `/learn`.
- **Session files self-clean.** Weak inferences expire within a handful of sessions.
- **Edge propagation becomes meaningful.** A Local inference that extends or updates another Percept receives `+0.05` — boosting it toward the promotion threshold. This was always the intended path for model inferences to reach long-term storage, but was bypassed because the threshold was too low.
- **Tests updated** to reflect new constants (initial weights, prune counts, promote/no-promote boundaries).
