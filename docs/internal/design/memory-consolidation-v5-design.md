# Memory Consolidation v5 — Weighing, Decay, Stable-Fact Promotion (design sketch)

Date: 2026-10-29

## Status

Draft sketch superseded as prose by [ADR-0092](../../adr/0092-memory-consolidation-v5.md) (proposed 2026-10-29 — **neither document is implemented**) — the six open questions below are settled there, and where the two differ (score materialization in `w`, migration seeding from the provenance table) the proposal wins. Kept for the corpus evidence and the reasoning trail. Builds on [ADR-0053](../../adr/0053-memory-weight-model.md), [ADR-0056](../../adr/0056-archive-stale-global-percepts.md), [ADR-0090](../../adr/0090-memory-consolidation-v3.md) and [ADR-0091](../../adr/0091-memory-consolidation-v4.md), of which v3/v4 are implemented in `internal/memory/consolidate.go`. Where this sketch repeats v4 it is background, not a proposal. No code accompanies this document.

## Why a v5 — what v4 deliberately left open

v4 settled the mechanics: `derivedW` from persisted primitives, echo-proof corroboration (saturating, run-capped), running-time decay with `use_count`-slowed half-lives, supersession via live `updates` edges, durability-gated promotion. Five fronts were explicitly deferred or kept alive:

1. **The `confidence` × `activation` split** — deferred twice (ADR-0090, ADR-0091) as too costly to migrate; the raw-W/derivedW gate split "captures the operationally important half of it".
2. **Evidence slowing decay** — deferred "until a real corpus shows well-corroborated globals fading too fast".
3. **The use × decay interplay** — kept alive as "the first knob to revisit" if important-but-rarely-restated facts die.
4. **Contradiction resolution beyond blocking promotion** — deferred; conflicts sink both claims and resolution means manual `/forget` or edge removal.
5. **Engrams** — typed but inert: `Engram`, `Percept.EngramID` and `CompositeW` exist in the model (and `archive.go` cleans membership up), but nothing creates, groups, or maintains engrams. The RFC's NeoCortex/REM step is still unmapped.

## Corpus evidence (~/.milk/memory/, sampled 2026-10-29)

- **29 global percepts: 28 `producer: user`, 1 `primary`.** User assertions dominate the global store completely.
- **W distribution: 3 above the 0.20 floor (0.95, 0.95, 0.46); 26 pinned at exactly 0.20.** Two dozen facts the user explicitly stated are, after decay, indistinguishable from dead weight — the single-W conflation made visible: their *truth* never changed, only their *availability* did, and one number now carries both.
- **0 engrams, 1 edge (`extends`) across all sampled shards.** The edge/engram machinery rarely fires because producers rarely declare relations. v5 must work on an edge-sparse store; it cannot assume a rich relation graph.
- Producer mix implies whatever "stable fact" means in practice will mostly mean "user-stated" on this corpus — the design must treat user facts deliberately, not as a fast-path footnote.

## Part A — Weighing: split truth from availability

**Proposal (the third deferral is the charm): make the split first-class in v5.** Each Percept carries:

- **`confidence`** — "how true is this": moves *only* on events (corroboration, contradiction, supersession), exactly v4's derived-weight terms (`corrobBoost`, `contraPenalty`, `supersedPenalty`, marginal deltas, run-caps). Never decays. This is what "forgetting is not falsifying" was about — it becomes structural instead of a gate special case.
- **`activation`** — "how available is this now": decays with milk running time (Part B), refreshed by re-affirmation and partially by use. Never makes anything truer.

Score = `confidence × activation`, computed, never stored as the primary quantity (v4's `rank_score` precedent: derived numbers stay computed).

**Why split now rather than keep patching the gate matrix:**

- The corpus shows the conflation biting: 26 user facts decayed to the floor. Under one W, every remedy (slower decay, evidence-decay coupling, promotion asymmetry fixes) is a special case bolted onto one overloaded scalar — each of which v4 had to name as a deferral.
- The gate matrix is already two-valued (raw W for visibility/pruning, derivedW for promotion). The split collapses it: promotion reads `confidence`; visibility and pruning read the score; `min_confidence` reads the score so age still never hides a fact *by itself*.
- Migration has a precedent (`normalizePriors`, v4): one-time, first-touch, monotone-safe. Sketch: on first touch, `confidence := derivedW(old)` and `activation := w / derivedW(old)` clamped to [0,1] — preserving exactly the current visibility ordering, so no fact moves rank at upgrade time.

**Cheaper alternative (to compare against, not the recommendation):** keep single W and ship only the deferred knobs (evidence-slowed decay, use-decay knob, contradiction resolution). Lower migration cost, but the corpus problem above stays structural and the v6 deferral list is predictable. The split is a JSON-schema migration exactly once; the patch set migrates behavior forever.

## Part B — Decay: activation only, producer-aware

Under the split, decay touches `activation` alone. What carries over from v4: exponential in running time (Δt, summed-over-sessions clock — ADR-0056's lesson stands: never count passes or sessions), core exempt, half-life grows with `use_count` (≤ 2×), half-life halves while superseded.

What changes:

- **Producer-aware half-lives.** Session 3 days / global 60 days keeps holding for agent-produced percepts. **User percepts get a much longer activation half-life (order-of-magnitude suggestion: 365 running days) and an activation floor at or above the retrieval gate** — a user statement must never age into invisibility; it ranks lower, it does not disappear. Age-out stays staleness-archiving's job (ADR-0056), which is idle-based — a *use*-based guard, the right one for "nobody cares anymore".
- **Evidence slows decay** (deferral #2, now with a corpus warrant on the "fading" side only for high-confidence globals): half-life grows modestly with `evidence`, saturating like everything else truth-side. Corroboration buys persistence here without buying truth — the split keeps the two currencies separate, which is why this knob was awkward in v4 and is clean now. Small constant, cap at ~2×; tune by rebuild (v4 policy).
- **Use refreshes activation, not confidence** (v4's invariant kept: use never makes anything truer). Re-affirmation refreshes both (activation to full, confidence per the marginal-delta event rules).

The use × decay question (deferral #3) is answered by construction: a used-but-never-restated fact now stays *available* (activation refreshed by use) without being *reinforced* (confidence untouched) — the exact behavior the corpus worry was about, without redefining "evidence".

## Part C — Stable facts: engrams become the consolidation layer

This is the "promote stable facts" answer and the deferred NeoCortex/REM mapping, done file-based and LLM-free (semantic recall stays Phase 2 of [memory-design.md](memory-design.md)):

- **Formation (at consolidation):** group percepts sharing a subject — `Roles.Theme` where present, Jaccard ≥ threshold as fallback (same machinery as reinforcement's near-duplicate test, no embeddings). Deterministic and idempotent like the rest of the pass. Engram membership sets `Percept.EngramID`; the engram's `PerceptIDs` is the source of truth, membership cleanup on removal already exists in `archive.go`.
- **`CompositeW` becomes `composite confidence`** — the confidence-weighted mean of members, recomputed at consolidation (exactly what the type comment promises today).
- **Stability gate (engram-level):** membership ≥ 2 percepts **or** a promoted member, composite confidence ≥ `promote_threshold`, no live `contradicts` edge on any member, and held for `promote_stable_days` of running time (v4's durability stamp, applied at the engram level). A stable engram is the store's "slow consolidated knowledge": the subject-level fact survives even as individual member percepts decay, get archived, or supersede one another — the newer member's percept inherits the engram instead of starting a new subject cold.
- **Effects of stability:** members get the persistence floor (activation never below the retrieval gate — a stable fact cannot fade out from under the engram); the engram line participates in injection ranking (context economy: one subject line where N member percepts would compete — see open question 5); pruning and staleness-archiving treat stable-engram members as core-adjacent (archivable only via explicit `/forget` or contradiction demotion).

Promotion of *percepts* (session → global, v4 gates) is unchanged; engram stability is a second, coarser graduation on top of it.

## Part D — Contradiction resolution (unblocking deferral #4)

At consolidation, each `contradicts` cluster (same subject, both endpoints live) is adjudicated deterministically:

1. **Winner selection:** newer `updated_at` wins; `producer: user` beats agent on ties; higher `confidence` last. The winner is the surviving claim.
2. **Loser handling:** marked superseded (reuse the `updates` semantics that already exist: half-life halved, promotion blocked), demoted below the visibility gate, and staleness-archive-eligible early. It stays retrievable via `get_memory` (age/history never destroys truth) but stops being injected — silence, then archive, never silent deletion.
3. **Counters:** the `contradictions` penalty applies once to each endpoint as today; on resolution, the winner's penalty is forgiven (marginal-delta inverse, so idempotence holds) and the loser's stands — conflicts sink both claims *until* resolved, then the survivor recovers.

**Design tension to settle (open question 4):** ADR-0053's invariant says "milk never fabricates relations" — resolution here adjudicates *declared* edges, but recording the outcome as a fabricated `updates` edge would violate the letter of that invariant. Preferred: resolution is a store-side outcome (timestamped winner ID on the cluster or a `resolved` marker on the contradicts edge), not a new relation.

## Pass structure (delta over v4's seven steps)

v4's pass (decay → edge events → durability → prune → archive → promote → size-cap eviction → persist) stands. v5 inserts, between edge events and promote:

- **Engram formation/recompute** (Part C) — grouping, composite confidence, stability stamps.
- **Contradiction resolution** (Part D) — after edge events (penalties already applied), before promote (resolution unblocks the winner's promotion in the same pass).

Everything remains idempotent per run and marginal-delta based; engram recomputation is a pure function of membership and member confidences.

## Config knobs (following v4's policy: gates configurable, shape constants code)

Candidate `memory` block additions: `user_activation_halflife_days` (365), `evidence_decay_slowing` (0.25, cap 2×), `engram_stable_composite` (defaults to `promote_threshold`). Code constants: subject-cluster Jaccard threshold (reuse the reinforcement threshold), engram formation minimums. Removed from scope: nothing in v4's key set changes meaning.

## Migration

One-time, first-touch, `normalizePriors`-style: `confidence := derivedW(old)`, `activation := w / derivedW(old)` clamped. Existing engram fields map straight over (all empty in the sampled corpus anyway). No fact is promoted or archived by migration itself — stability stamps start fresh, exactly as v4 did it.

## Testing and validation

- Re-pin `model_validation_test.go` trajectories for the new primitives (v4 precedent).
- Corpus-effect run via the existing `MEMORY_CORPUS` harness against a copy of a real store: assert the trends that motivated this sketch (user facts stay visible; floor-pinned facts recover rank *only* through use; engrams form on `roles.theme` clusters).
- Synthetic mirror trend assertions for engram stability and contradiction resolution (exact counts are float-noise sensitive — ADR-0053's caution).

## Lands with it (docs)

`docs/guides/operations.md` "Memory weight model", the memory bullet in `CLAUDE.md`, the [memory-design.md](memory-design.md) RFC-mapping table (Engram row becomes real; NREM row gains the engram layer), and a v5 ADR superseding this sketch on acceptance.

## Open questions (decisions needed before this becomes an ADR)

1. **Split vs. patch set** (Part A) — the structural call. Recommendation: split.
2. **Old user facts** — full activation floor above the retrieval gate, or exempt user percepts from activation decay entirely? Recommendation: slow decay + floor; total exemption makes `activation` producer-dependent in a way that will surprise every reader of a rank score.
3. **Clustering key coverage** — sampled percepts carry `roles.theme`, but is theme coverage high corpus-wide? Verify before committing to theme-first clustering; Jaccard fallback may need to be primary.
4. **Relation fabrication** — resolution outcome as edge vs. store-side marker (Part D). Recommendation: store-side marker.
5. **Injection shape for stable engrams** — grouped subject line in the injection budget vs. member percepts ranked individually? The context-economy win argues for the former; the relevance gate is per-percept today. A middle path: members rank individually, but a stable engram pins at least its best-ranked member into the budget.
6. **Does v5 need semantic recall (Phase 2)?** Recommendation: no — everything above works on keyword/Jaccard machinery that already exists.

## Rejected alternatives

- **Counting use toward `confidence`** — stays rejected (v4): use means "relevant", not "true".
- **LLM-written engram summaries** — rejected for v5: consolidation stays deterministic and offline; NL synthesis is the RFC's recall-side concept, not a consolidation step.
- **Hard deletion of losers on contradiction resolution** — rejected: `/forget` stays the delete path; resolution silences and archives, never destroys (ADR-0056's reversibility stance).
