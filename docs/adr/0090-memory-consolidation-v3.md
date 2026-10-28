# 90. Memory Consolidation v3: Running-Time Decay and Evidence-Gated Promotion

Date: 2026-10-28

## Status

Accepted 2026-10-28 — supersedes the decay, recall-ordering and promotion rules of [ADR-0025](0025-memory-decay-constant-retuning.md) and [ADR-0053](0053-memory-weight-model.md). Reinforcement, typed edges, and staleness-archiving ([ADR-0056](0056-archive-stale-global-percepts.md)) stand as they are.

## Context

The weight model after ADR-0053/0056 works — reinforcement instead of duplicate rejection, wired edges, recency ordering, idle-time archiving — but three structural problems remain:

1. **Decay cadence is wrong.** `Consolidate()` runs after *every* primary-agent turn (`cmd/milk/repl.go`, `cmd/milk/main.go` deferred calls), yet `applyDecayCount` still subtracts a per-*session* constant (−0.10) per pass and `applyGlobalDecay` a per-pass −0.02. In a 20-turn session a session percept (born 0.7) is pruned within ~5 turns and a global one sinks to its 0.20 floor in a single long session. The ADR-0056 staleness clock fixed exactly this class of problem for archiving — "any rule counted in consolidations or sessions would depend on how chatty the sessions were" — but decay never got the same treatment. The doc comments in `consolidate.go` ("session end", "−0.10 per session") still describe the old cadence.
2. **One scalar `w` conflates three things**: how true a fact is (producer confidence), how much it is corroborated (evidence), and how alive it is now (recency). Promotion, pruning, recall gating and injection budgeting all read the same number, so tuning one behavior disturbs the others.
3. **"Stable" means only `w ≥ 0.80`.** A fact re-stated four times in one sitting grinds `reconfirm_cap` (0.90) and graduates on the same evidence a fact confirmed across two weeks has. Nothing distinguishes *repeated* from *durable*.

## Decision

### Weighing

**`w` keeps exactly one meaning: current strength in [0,1], the quantity that decays.** Every gate reads raw `w` (`min_confidence`, `promote_threshold`, `prune_threshold`) so age or hype can never hide or mint a fact. Ordering and eviction use a derived **rank score**, computed never stored:

```
rank_score = w · (1 + r) · (1 + e)
r = RecencyBoost · 0.5^(age_days / RecencyHalfLifeDays)   // since updated_at, as today
e = 0.05 · min(evidence, 4)
```

`rank_score` replaces the additive `w + recency` tie-break in `Query`, drives `PruneGlobal`'s eviction order (lowest score first, core never evicted), and decides which percepts the injection budget (`percept_inject_max`, `percept_inject_max_bytes`) drops when over budget. Token-hit count remains the primary sort key in `Query`.

**`evidence` is one new integer field on `Percept`** (omitempty): corroboration events, **awarded at most once per milk run** (one Store lifetime; tracked by an in-memory per-run set, only the count persists):

- +1 when the percept is reinforced (near-duplicate restatement), and
- +1 when a positive edge (`extends`/`updates`/`derives`) boosts it at consolidation.

*Use does not count.* Injection and `get_memory` keep a fact alive (recency, the archive clock) but do not make it true. Because increments are capped at one per run, `evidence ≥ 2` means the fact was corroborated in at least two distinct runs — one sitting cannot mint a global fact.

### Decay — running-time exponential, not per-pass subtraction

Each pass takes **Δt, the milk running time since the previous pass** (the same interval `accrueAndArchiveLocked` already accrues into `idle_seconds`) and applies, for non-core percepts only:

- **session:** `w ← w · 2^(−Δt / session_half_life_days)`, then prune at `prune_threshold` (0.20).
- **global:** `w ← prune_floor + (w − prune_floor) · 2^(−Δt / global_half_life_days)` — still floored at 0.20, so global facts sink in rank and are removed only by staleness-archiving or the size cap, exactly as ADR-0053/0056 intend.

Defaults: `session_half_life_days = 3`, `global_half_life_days = 60` (milk running days, the same "summed over sessions" clock as `global_archive_after_days`). These replace `decay_per_session` / `global_decay_per_session` in the `memory` config block; the old keys are ignored. Core is exempt everywhere. Reinforcement still adds `reconfirm_boost` (0.05) capped at `reconfirm_cap` (0.90) and touches `updated_at`; use still resets `idle_seconds`/`last_used_at`. The consequences of that split are deliberate and stay: a used-but-never-restated fact still decays (ADR-0016's Model A — decay is unconditional, re-confirmation is the life signal), while a used global never becomes archive-eligible (ADR-0056).

### Promotion — strength plus durability

- **Fast path (unchanged):** `core` percepts and `producer: user` facts promote once `w ≥ promote_threshold` (0.80). They are explicit assertions; a user fact born at 0.9 still graduates on its first pass.
- **Everything else:** `w ≥ promote_threshold` **and** `evidence ≥ 2` — corroborated in at least two distinct runs. A restatement in run 1 (`w` 0.7 → 0.75, `evidence` 1) followed by one in run 2 (`w` → 0.80, `evidence` 2) is the intended graduation trajectory.
- **A live `contradicts` edge blocks promotion of both endpoints** until the conflict is resolved (`/forget` of one side, or removal of the edge). The symmetric −0.10 penalty at consolidation stays; an unresolved conflict simply cannot graduate.
- Ordering preserved: promotion runs **after** the global pass, so freshly promoted percepts are not decayed again in the same run.

### Pass structure

One `ConsolidateCtx`, same call sites as today (after every primary-agent turn, at exit, after a single-prompt run):

1. Accrue Δt → time-decay session and global `w` (core exempt); accrue `idle_seconds` for globals not used in the interval.
2. Edge propagation (positive → target, `contradicts` → both ends); award `evidence` for positive-edge boosts (per-run cap).
3. Prune session percepts at `w ≤ prune_threshold`.
4. Staleness-archive eligible globals (ADR-0056 rules, unchanged).
5. Promote qualifying session percepts (fast path or evidence-gated).
6. Size-cap eviction (`PruneGlobal`) by ascending `rank_score`.
7. Persist both files; metrics under `milk.memory.consolidation.*` gain `promotion_deferred` (would-be promotions held back by the evidence gate or a live contradiction) alongside the existing `decayed`/`pruned`/`promoted`/`global_decayed`/`archived` counters.

### Knobs

The `memory` config block (zero/missing → default, `Params.normalized()` unchanged): `session_half_life_days` (3), `global_half_life_days` (60), `reconfirm_boost` (0.05), `reconfirm_cap` (0.90), `promote_threshold` (0.80), `prune_threshold` (0.20), `recency_boost` (0.10), `recency_half_life_days` (30), `global_archive_after_days` (14).

## Consequences

- **Chattiness stops mattering.** Decay scales with milk running time: one 2-hour session decays like four 30-minute ones. A session fact survives roughly a week of running time unreinforced (0.7 → prune at ~6 running days with the 3-day half-life); global facts sink over months of running time.
- **The two clocks stay separate and both matter:** running-time decay since `updated_at` is the importance signal, idle time (use) guards archiving. Re-confirmation is still the only way to raise `w`.
- **The single-echo promotion path closes** without touching user/core facts: inference percepts graduate only when corroborated in a second run. Existing inference percepts carry `evidence = 0` and get a bounded window (one half-life of running time) to earn a second-run corroboration before the faster session decay retires them — a one-time slowdown, not data loss.
- **Lands with it:** `Percept.Evidence` plus per-run award bookkeeping in `Store`; `Query`/`recencyScore`, `PruneGlobal` and the injection-budget drop switch to `rank_score`; `Params` and generated-config keys (old decay keys retired); re-pinned `model_validation_test.go` trajectories (the 0.7 → 0.75 → 0.80 → 0.85 → 0.90 reinforcement path gains the evidence gate before promotion); the stale "session end / −0.10 per session" comments in `consolidate.go`; `docs/guides/operations.md` "Memory weight model" and the memory bullet in `CLAUDE.md`.
- **Rejected alternative (deferred, not ruled out):** splitting immutable `confidence` from decaying `activation` (score = product) — semantically cleaner ("never decay truth, decay availability") but a JSON-schema and every-read-path migration; revisit if the single-`w` model conflates behaviors again. Also deferred: counting use toward `evidence`, per-relation edge-boost tuning, contradiction resolution beyond blocking promotion, and letting `evidence` slow global decay.
