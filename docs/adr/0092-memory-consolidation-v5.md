# 92. Memory Consolidation v5: confidence × activation, producer-aware forgetting, engram stability, contradiction resolution

Date: 2026-10-29

## Status

Proposed 2026-10-29 — a redesign of the weighing, decay and promotion rules of [ADR-0091](0091-memory-consolidation-v4.md). **Not yet implemented:** no code accompanies this document, so as long as that holds, ADR-0091 (implemented in `internal/memory/consolidate.go`) governs and this proposal is aspirational only; where they differ, v4 wins in practice today. ADR-0091's event discipline carries over unchanged and would be simplified by this decision: run-capped evidence, echo-proof corroboration, saturating boost/penalty terms, and the `updates`-supersedes / `contradicts`-sinks edge semantics. [ADR-0056](0056-archive-stale-global-percepts.md) staleness-archiving keeps its idle-based rule, restated in activation terms below. Settles the open questions of the design sketch `docs/internal/design/memory-consolidation-v5-design.md`; where the two differ (score materialization, migration seeding), this proposal wins.

## Context

v4 settled the mechanics — `derivedW` from persisted primitives, echo-proof corroboration, running-time decay, supersession, durability-gated promotion — and deferred five fronts: the `confidence` × `activation` split, evidence slowing decay, the use × decay interplay, contradiction resolution beyond blocking, and engrams (typed but inert). A corpus sample of `~/.milk/memory/` (2026-10-29) shows the deferrals now matter:

- **29 global percepts: 28 `producer: user`, 1 `primary`.** Whatever "stable fact" means in practice mostly means "user-stated" on this corpus — user facts must be handled deliberately, not as a fast-path footnote.
- **W distribution: 0.95 ×2, 0.46 ×1, 0.20 ×26.** Two dozen facts the user explicitly stated are, after decay, indistinguishable from dead weight. Their truth never changed — one overloaded scalar carries both truth and availability, and every remedy under single-W (slower decay, evidence–decay coupling, gate asymmetries) is a special case bolted onto it.
- **0 engrams, 0 edges in `global.json` (1 `extends` across all shards).** Producers rarely declare relations; v5 must work on an edge-sparse store.
- **Clustering-coverage check (settles the sketch's open question 3):** `roles.theme` is present on 23/29 globals (49/57 across all shards) — but **0 themes are shared by ≥2 percepts**, and **0 of 406 global content pairs reach Jaccard ≥ 0.30**. `record_memory` already funnels its `subject` argument into `Roles.Theme`, so the declaration path exists and is used; the labels are simply unique free text. Exact-theme grouping and Jaccard fallback both produce singletons on the real store.

## Decision

### Weighing — truth and availability are different quantities

Each Percept carries two primitives in place of one overloaded W:

- **`confidence`** — how true this is. Moves **only on events** (corroboration, contradiction, supersession, resolution), never on time. It *is* v4's `derivedW`, renamed and reinterpreted:
  `confidence = clamp(prior + corrobBoost(evidence) − contraPenalty(contradictions) − supersedPenalty·superseded)`, non-core capped at `reconfirm_cap` (0.90). Computed from primitives, never mutated in place. "Forgetting is not falsifying" becomes structural instead of a gate special case.
- **`activation`** — how available this is now. Decays with milk running time (below); refreshed by re-affirmation and use. **Never makes anything truer.**

The **score** is `confidence × activation`. Percept keeps `w` as the *persisted score* — but it is now derived state: every pass rewrites it from the primitives, so it can never become path-dependent (ADR-0091's root complaint). Nothing mutates `w` in place; the only stateful decay is `activation`, the only stateful truth moves are event counts. This keeps every existing W reader (panel, forget, injection ordering, tool output) working on the same number with the same meaning *as a score*, while the gate matrix collapses into one rule per gate:

- promotion and durability read **`confidence`** (truth gates read truth),
- pruning, visibility, injection ranking and display read **`w`** (availability gates read availability),
- `get_memory`'s `min_confidence` reads the score — age alone hides nothing the score floor (below) doesn't already bound.

Events keep ADR-0091's discipline and get simpler: they move counts/flags and the score follows at the next re-derivation, so idempotence and telescoping fall out of the model instead of needing marginal-delta bookkeeping on W. Re-affirmation (near-duplicate re-statement, Jaccard ≥ 0.60) sets `activation := 1` (and touches `updated_at`); `evidence += 1` at most once per Store lifetime, `use_count += 1` — v4's echo-proof rule, unchanged. Use (recall, injection) increments `use_count` and doubles `activation` (capped at 1) — **use refreshes availability, never truth** (v4's rejected "use as evidence" stays rejected).

### Decay — activation only, producer-aware

Decay touches `activation` alone; `confidence` is untouched by time. Carried over from v4: exponential in milk running time (Δt, the summed-over-sessions clock — ADR-0056's lesson: never count passes or sessions), core exempt, half-life grows with `use_count` (`1 + 0.25·min(use_count,4)`, at most 2×), half-life halves while superseded. What changes:

- **Producer-aware half-lives.** Agent-produced percepts keep session 3 / global 60 running days. **`producer: user` percepts get `user_activation_halflife_days` (365).**
- **A score floor, not a W floor.** User percepts' score never *decays* below the `get_memory` retrieval gate (0.40) — implemented as an activation decay floor of `0.40 / confidence`. A user statement ranks lower with age; it does not age into invisibility. The floor clamps *decay*, not values: activation already below the floor at migration is not lifted (see Migration). Age-out stays staleness-archiving's job (ADR-0056) — idle-based, the right guard for "nobody cares anymore".
- **Evidence slows decay** (the deferred knob, now clean): the activation half-life grows like the use factor — `1 + 0.25·min(evidence,4)`, at most 2×. Corroboration buys persistence here without buying truth; the split keeps the two currencies separate.
- The **use × decay question** is answered by construction: a used-but-never-restated fact stays available (activation refreshed by use) without being reinforced (confidence untouched) — the corpus worry, resolved without redefining "evidence".

User percepts promote on v4's fast path at their first consolidation (prior 0.90 ≥ 0.80), so the long half-life governs globals in practice. Agent globals decay `activation` toward 0 and drop below the retrieval gate on their own — staleness-archiving and size-cap eviction remain their death paths, as in v4.

### Promotion — percept gates unchanged, engram stability on top

Percept promotion is v4's rule reading `confidence` (identical to `derivedW`, so the rule text is unchanged): fast path (`core`, `producer: user`) at `confidence ≥ promote_threshold`; otherwise `confidence ≥ promote_threshold` **and** `evidence ≥ 2` distinct runs **and** durability (`above_threshold_since`, revocable, `promote_stable_days`). A live `contradicts` edge or `updates` supersession defers (`promotion_deferred`). A boost burst cannot graduate on momentum.

**Stable facts** get a second, coarser graduation — engrams become the consolidation layer (the deferred NeoCortex/REM mapping, file-based and LLM-free):

- **Formation** (at consolidation, deterministic and idempotent): group percepts that share a subject — exact `Roles.Theme` (the `record_memory.subject` label) first, Jaccard ≥ 0.60 on content as fallback. `Percept.EngramID` records membership; the engram's `PerceptIDs` is the source of truth (membership cleanup on removal already exists).
- **On the current corpus this is a near-term no-op** (0 shared themes, 0 Jaccard pairs) and that is accepted: every engram effect is strictly conditional, and the user-fact persistence guarantee above does **not** depend on engrams. The convention nudge is documentation, not machinery — reusing a `subject` label is what makes grouping fire.
- **`CompositeW` becomes composite confidence** — the confidence-weighted mean of members, recomputed each pass (what the type comment promises today).
- **Stability gate (engram-level):** membership ≥ 2 percepts or a promoted member, composite confidence ≥ `promote_threshold`, no live `contradicts` on any member, held `promote_stable_days` of running time. A stable engram is the store's slow consolidated knowledge: the subject survives member decay, archiving and supersession (a superseding member inherits the engram instead of starting cold).
- **Effects of stability:** members become core-adjacent (activation floor raised to the score floor; archivable only via explicit `/forget` or contradiction demotion) and a stable engram pins at least its best-ranked member into the injection budget (members still rank individually — the middle path; a grouped subject line is deferred until relevance gating is subject-aware).

### Contradiction resolution — the conflict is adjudicated, not just blocked

At consolidation, each `contradicts` edge with both endpoints live and no recorded outcome is adjudicated deterministically:

1. **Winner:** newer `updated_at`; ties broken by `producer: user` over agent, then higher `confidence`.
2. **Outcome is a store-side marker on the declared edge** (`resolved_at`, `winner_id`) — **not** a fabricated `updates` edge (ADR-0053: milk never fabricates relations).
3. **Loser:** superseded treatment (half-life halved, promotion blocked), activation demoted below the retrieval gate so it stops being injected, and staleness-archive-eligible early. It stays retrievable via `get_memory` with a lowered `min_confidence` — silence, then archive, never silent deletion (`/forget` stays the delete path).
4. **Winner:** the adjudicated conflict is withdrawn from its history — `contradictions` decremented for that edge (floor 0), applied once per resolved edge per Store lifetime (per-run award-set discipline), so the penalty forgives exactly once and re-running the pass changes nothing. The loser's count stands. The promotion block lifts for the winner; the loser stays blocked.

Conflicts sink both claims *until* resolved, then the survivor recovers. Resolution runs after edge events (penalties already applied) and before durability tracking, so forgiveness can start the durability clock in the same pass. Multi-claim clusters are adjudicated pairwise; `updates` supersession is untouched.

### Pass structure (delta over v4)

v4's pass stands — decay → edge events → durability → prune → staleness-archive → promote → size-cap eviction → persist. v5 inserts two steps **immediately after edge events** (both passes): **engram formation/recompute**, then **contradiction resolution**. Everything remains idempotent per run; engram recomputation is a pure function of membership and member confidences.

### Migration

One-time, first-touch (the `normalizePriors` precedent), keyed on `activation == 0` with `w > 0` (activation is never legitimately zero outside pruning):

1. `prior := initialWeight(producer)` when unset — seeded from the **provenance table** (user 0.9 / inference 0.7 / system 0.4 / core 1.0), *not* backed out of decayed W. v4's `prior := W − corrobBoost + contraPenalty` is superseded before it ships: backing decayed W into `prior` would seed the 26 floor-pinned user facts at confidence 0.20, record their decay as untruth, and strand them (use could never recover rank). Where the sketch said `confidence := derivedW(old)`, this ADR wins.
2. Event counts carry over as recorded (all zero on pre-v4 shards).
3. `activation := clamp(w / confidence, 0, 1)`; `w` is left as-is and equals `confidence × activation` by construction.

Consequences of the seeding: no fact's score rises at cutover; ordering among facts at or below their provenance ceiling (26 of 29 in the sample) is exactly preserved; the rare fact whose stored W exceeded its provenance ceiling settles to score = confidence (the two 0.95 sample facts dip ≤ 0.05 — accepted; truth-seeding wins over exact ordering at the ceiling). Migration promotes and archives nothing; durability stamps start fresh, as in v4.

### Config and constants

`user_activation_halflife_days` (365) and `evidence_decay_slowing` (0.25, cap 4 counted — 2× max half-life) join the `memory` block; `engram_stable_composite` defaults to `promote_threshold`. No v4 key changes meaning (`promote_threshold` now reads confidence and composite confidence). Code constants, tuned by rebuild: use-refresh (one doubling per use), score floor (0.40 — the `min_confidence` default), subject-cluster Jaccard threshold (0.60 — the reinforcement threshold), engram formation minimums.

## Consequences

- Forgetting is structurally not falsifying: nothing truth-side ever moves with time, and nothing availability-side ever moves without it.
- The corpus's headline problem — user statements decaying into dead weight — is fixed by construction: they keep a 365-day activation half-life, a score floor at the retrieval gate, and rank recovery through use alone (activation doubling per use: 0.22 → 0.44 → 0.88 → 1.0 for a migrated floor-pinned fact).
- Consolidation gets *simpler* where it was hardest: `w` is materialized from primitives each pass, so boost telescoping and event idempotence are properties of the model rather than bookkeeping.
- Contradictions recover: the survivor's penalty is forgiven on adjudication instead of both claims sinking to the −0.30 cap forever.
- The v4 raw-W/derivedW asymmetry is no longer a documented quirk — promotion reads truth, visibility reads availability, and each gate has one meaning.
- Engram stability is forward-looking on today's store (graceful no-op) and load-bearing once producers share `subject` labels; nothing in v5 waits on it.
- Observability: `promotion_deferred` stands; resolution is counted as `milk.memory.contradictions_resolved`.
- Lands with it (docs): `docs/guides/operations.md` "Memory weight model", the memory bullet in `CLAUDE.md`, the memory-design.md RFC-mapping table (Engram row becomes real; NREM row gains the engram layer).

## Rejected alternatives and deferred work

- **Patch set on single W** (slower decay, evidence–decay coupling, gate tweaks, no split) — rejected: the corpus problem is structural and the deferral list would recur as v6. The split is a one-time migration; the patch set migrates behavior forever.
- **Full decay exemption for user percepts** — rejected: `activation` would be producer-dependent in a way that surprises every reader of a score. Slow decay + score floor instead.
- **Migration seeds `confidence` from decayed W** (sketch formula) — rejected: it records forgetting as falsity and strands exactly the facts this ADR exists to rescue.
- **Activation clamp-up at migration** — rejected: a rank jump at cutover. Below-floor migrated values recover through use only.
- **Use counting toward `confidence`** — stays rejected (v4): use means relevant, not true.
- **LLM-written engram summaries** — rejected: consolidation stays deterministic and offline; NL synthesis is recall-side (memory-design.md Phase 2), not a consolidation step.
- **Fabricating an `updates` edge to record resolution** — rejected (ADR-0053): the outcome is a marker on the declared edge.
- **Hard deletion of resolution losers** — rejected: `/forget` is the delete path; resolution silences and archives (ADR-0056 reversibility).
- **Semantic recall / embeddings for clustering** — deferred: everything above runs on exact-label and token-Jaccard machinery that already exists.
- **Subject-aware relevance gating and grouped injection lines** — deferred until the engram layer has real memberships to rank; the pin-best-member middle path covers the budget case.
- **Per-relation edge-boost tuning, multi-claim (non-pairwise) adjudication** — deferred until a corpus with real edges exists.
