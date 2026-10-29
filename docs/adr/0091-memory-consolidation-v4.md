# 91. Memory Consolidation v4: Derived Weights, Echo-Proof Corroboration, Supersession and Durability

Date: 2026-10-28

## Status

Accepted 2026-10-28 — supersedes the weighing and reinforcement rules of [ADR-0090](0090-memory-consolidation-v3.md) (and through it the reinforcement rule of [ADR-0053](0053-memory-weight-model.md)). ADR-0090's running-time decay clock, evidence-gated promotion idea and rank ordering stand; [ADR-0056](0056-archive-stale-global-percepts.md) staleness-archiving is untouched.

## Context

v3 fixed the decay cadence and gated promotion on corroboration, but four problems remained:

1. **W is path-dependent.** Weight is mutated in place — reinforcement `+0.05`, edge boosts `±`, decay multipliers all interleave across passes — so the same event history can yield different W depending on pass order and how boosts were clipped. Worse, positive edges re-applied their `+0.05` *every pass* (only the Evidence point was run-capped), so a live edge silently kept inflating its target.
2. **`updates` corroborated the wrong end.** The relation meant "the source updates the target", yet the boost landed on the *target* — the outdated claim got stronger when it was superseded.
3. **Echo counted as much as corroboration.** The flat `+0.05` per near-duplicate restatement, capped at 0.90, let five re-statements in one sitting grind an inference fact to the cap — "one sitting cannot mint a trusted fact" held only for `evidence`, not for W.
4. **"Stable" meant only `w ≥ 0.80`.** Nothing distinguished a fact that sat above the gate for days from one pushed over it by a burst of boosts moments ago; and nothing let a *used* fact forget more slowly than an ignored one.

## Decision

### Weighing — W is derived from persisted primitives, then decayed in time

`Percept` gains five fields (`json:"…,omitempty"`): `prior` (provenance strength — the birth weight table), `contradictions` (conflict events), `superseded` (0/1 flag — set while any live `updates` edge targets it; multiple edges do not stack), `use_count` (re-statements, recalls, injections), `above_threshold_since` (durability stamp). `evidence` (v3) stands as the corroboration count. The boost side of W is a pure function of these primitives:

```
derivedW = clamp(prior + corrobBoost(evidence) − contraPenalty(contradictions)
                 − supersedPenalty·superseded)          // non-core capped at reconfirm_cap
corrobBoost(c)  = 0.25·c/(c+2)                          // saturating: corrobBoost(2) = 0.125
contraPenalty(k)= min(0.10·k, 0.30)                     // saturating too
```

Corroboration saturates, so the second distinct confirmation matters far more than the tenth echo. Non-core derivedW is capped at `reconfirm_cap` (0.90) — corroboration never mints perfect confidence (the ADR-0053 invariant, now structural).

Events apply **marginal deltas**: an event moves the primitive counts and shifts W by exactly `derivedW(after) − derivedW(before)`, so event effects telescope however many passes they arrive in, and re-running a pass over the same events changes nothing (the per-run award sets already capped them; now the W side is idempotent too). **Re-affirmation** (reinforcement) is the exception that proves the rule: a fact re-stated *now* refreshes W to `derivedW` outright — the forgetting curve restarts from the current derived strength, not from a decayed one.

Decay stays exactly v3's running-time exponential (session → 0 at 3 days half-life, global → the 0.20 floor at 60 days; core exempt; Δt clipped at `updated_at`), with two modulations: the half-life grows with `use_count` (`1 + 0.25·min(use_count, 4)` — up to 2× slower forgetting for facts that keep being put to use; repetition buys persistence, never truth) and halves while the percept is superseded (the newer claim wins by faster forgetting).

Gates: `min_confidence` and `prune_threshold` keep reading **raw W** — age never hides a fact, weak facts die. **Promotion reads `derivedW`** — forgetting is not falsifying: a fact affirmed strongly yesterday and one affirmed this minute are equally true, and a raw-W re-check at the end of the durability window would make the gate unsatisfiable (a session fact at the threshold drops below it within hours under the 3-day half-life). Hype still cannot mint: derivedW is saturated and run-capped, and the gates below hold.

### Edge semantics — `updates` supersedes

| relation | corroborates | other effect |
|---|---|---|
| `extends`, `derives` | target (`To`) | — |
| `updates` | source (`From`) — the newer claim | target is superseded: half-life halved, promotion blocked while the edge is live |
| `contradicts` | — | one `contradictions` event per endpoint per run (−0.10 marginal, saturating at −0.30) |

Supersession is re-derived from live edges every pass (not a cumulative count), so removing an `updates` edge lifts the penalty and the promotion block. Contradiction events, like corroboration events, are permanent history (counts never decrease); a live `contradicts` edge additionally blocks promotion of both endpoints until resolved — v3's rule.

### Promotion — strength, corroboration, durability

- **Fast path (unchanged):** `core` and `producer: user` facts promote at `derivedW ≥ promote_threshold` (0.80) — explicit assertions.
- **Everything else:** `derivedW ≥ promote_threshold` **and** `evidence ≥ 2` (two distinct runs) **and** **durability**: `above_threshold_since` set and `now − above_threshold_since ≥ promote_stable_days` (default 1) of running time. The stamp is set at the first pass observing `derivedW ≥ threshold`, cleared when derivedW dips below it (only events move derivedW — decay never clears the stamp). A boost burst cannot graduate on momentum; a twice-corroborated fact graduates a running day after it first crossed the gate.
- A live `contradicts` edge or an `updates` supersession defers (counted `promotion_deferred`) until resolved.
- Ordering preserved: promotion runs after the global pass, so fresh promotions are not decayed again in the same run.

### Reinforcement — echo-proof

Re-stating a near-duplicate (Jaccard ≥ 0.60) re-affirms: `evidence += 1` **at most once per Store lifetime** (as v3), `use_count += 1`, `updated_at` touched, W refreshed to derivedW. Additional re-statements in the same run change nothing — five echoes in one sitting move W once, not five times. The W trajectory of an inference fact is now the corroboration series `0.7 + 0.25·c/(c+2)`: 0.783 in run 1, 0.825 in run 2 (over the gate), converging to the 0.90 cap — instead of v3's flat `+0.05` per restatement.

### Migration

Pre-v4 percepts (`prior == 0`) are normalized once, on first touch: `prior := W − corrobBoost(evidence) + contraPenalty(contradictions)` — the stored strength with the count-derived boosts backed out, so re-deriving them cannot double-count. Boosts older models baked flat into W stay in the prior: a one-time, monotone-safe approximation. No existing fact is promoted by migration itself — `above_threshold_since` starts fresh, so the durability gate defers every non-fast-path promotion to a later pass.

### Config and constants

`reconfirm_boost` is removed from the `memory` config block (the saturating corroboration term replaces it; the old key is ignored). `promote_stable_days` (1) joins it. The derived-weight terms (`corrob_max` 0.25, `corrob_half_sat` 2, `contra_delta` 0.10, `contra_max` 0.30, `supersed_penalty` 0.10, `supersed_half_life` 0.5, `use_decay_slowing` 0.25 / cap 4) are code constants like the old `edge_boost` — tune by rebuild until a real corpus says otherwise.

## Consequences

- Consolidation is idempotent per run and order-independent in its boosts: every event applies exactly once, and re-affirmation — not pass noise — restores strength.
- Echo chambers are closed at the W level, not just the evidence level: repetition in one sitting adds nothing; only distinct runs (or distinct edges) corroborate.
- Superseded facts fade instead of being boosted; conflicts sink both claims but never below `prior − 0.30` by penalty alone — pruning still decides death.
- Used facts persist longer (spaced repetition) without becoming truer — `use_count` moves decay only.
- The durability gate delays non-fast-path promotion by at least one running day past the threshold crossing; the graduation trajectory of a twice-corroborated fact is "cross → hold a day → promote".
- One deliberate asymmetry is documented and accepted: promotion reads derivedW while visibility/pruning read raw W, so a very decayed fact can graduate into global at low raw W (it re-earns rank through use like any other global).

## Rejected alternatives and deferred work

- **`confidence` × `activation` split** (carried over from ADR-0090's deferral): immutable truth vs. decaying availability, score = product — semantically cleaner ("never decay truth, decay availability") but a JSON-schema and every-read-path migration. The raw-W / derivedW gate split captures the operationally important half of it. Revisit if the single-W model conflates behaviors again.
- **Counting use toward `evidence`** — rejected: it would make "used" mean "true". `use_count` moves decay only. Open question kept alive: if the corpus shows important-but-rarely-restated facts dying while sitting high on derivedW, the use/decay interplay is the first knob to revisit, not the truth terms.
- **Evidence slowing global decay** beyond `use_count` — deferred until a real corpus shows well-corroborated globals fading too fast.
- **Per-relation edge-boost tuning** — the fixed saturating terms stay code constants (like the old `edge_boost`) until a real corpus says otherwise.
- **Contradiction resolution beyond blocking promotion** — deferred; for now conflicts sink both claims (−0.30 penalty cap) and resolution means `/forget` of one side or removal of the edge.
- **Corroboration saturation below the cap** (max +0.25 of the 0.90 `reconfirm_cap`) is by design, not a tunable: even a well-corroborated inference fact never reaches user-level confidence through corroboration alone.
