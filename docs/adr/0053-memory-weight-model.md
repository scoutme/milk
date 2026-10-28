# 53. Memory Weight Model v2: Reinforcement, Wired Edges, Recency

Date: 2026-10-13

Status: Accepted — decay, recall-ordering and promotion rules superseded by [ADR-0090](0090-memory-consolidation-v3.md)

## Context

Issue #167: the Percept store's staleness/promotion behavior had never been re-evaluated against real usage. The review (measured against the real `~/.milk/memory/` corpus: 31 percepts, 16 global all at `w=0.800`, 15 session `w=0.2–0.7`, **0 edges**) found the model was half-dead machinery:

- **Re-confirmation was punished.** `Record` returned `DuplicateError` for near-duplicates — a fact restated by the user was rejected as an error instead of reinforcing the percept. Net effect: `w` only ever decayed; nothing strengthened memory.
- **Edge propagation never fired.** `applyEdges` was real code but no production path ever constructed an `Edge` (0 in the corpus), while `initialWeight`'s comment literally said inference percepts (born 0.7) "need edge boost to promote" (≥ 0.80) — i.e. local/escalation percepts could mathematically never be promoted, by a mechanism that didn't exist.
- **No staleness or recency concept.** `Query` ranked by token hits then `w`; age never mattered. Session percepts decayed −0.10/session while global percepts never decayed at all.
- **A zombie band existed**: inference percepts fell below `get_memory`'s default `min_confidence=0.4` after ~3 consolidations but weren't pruned until ~5 — kept yet invisible.
- **Comments contradicted the constants** ("−0.03" vs 0.10; "prune W <= 0" vs W ≤ 0.20).

## Decision

All five forces on `w` are now explicit (docs/guides/operations.md "Memory weight model"):

1. **Birth weights** unchanged (user 0.9, inference 0.7, system 0.4, core 1.0).
2. **Reinforcement replaces duplicate rejection** (the B design): a near-duplicate (Jaccard ≥ 0.60) strengthens the existing percept — `w += ReconfirmBoost (0.05)`, capped at `ReconfirmCap`, `updated_at` touched — instead of inserting a copy. `Record`/`RecordGlobal` return a `RecordOutcome{ID, Reinforced, Similarity, Existing, W}`; `DuplicateError` is gone. Reinforcement **never lowers** `w` (a percept already above the cap keeps its weight). The cap is `0.90 = promoteThreshold + decayPerSession` deliberately: promotion runs after the −0.10 session decay, so a fully re-confirmed inference percept (0.7 → 0.90 ×4) survives the decay and graduates at the next consolidation — repeated confirmation is a real promotion path, while a single echo can never mint a fully trusted fact.
3. **Edges are wired** (the user chose wiring over removal, the C decision): `Store.AddEdge` validates and persists typed edges; `record_memory` gained `relation` (`extends|updates|derives|contradicts`) + `related_to` (ID/prefix/unique description) so the *producer agent* declares semantics — milk never fabricates relations. Consolidation applies them exactly as the old code did (+0.05 to the target of positive relations; −0.10 to both endpoints of `contradicts`), now on both the session pass and the global pass. Edges are stored in the "from" percept's store file and bite endpoints in that same file (cross-store edges are inert — honest and documented). Edges are idempotent.
4. **Global decay** (D): non-core (promoted) global percepts decay −0.02 per consolidation, floored at `pruneThreshold` (0.20) — old facts sink in rank but are never deleted from global by decay (eviction stays size-based; `/forget` stays the delete path). `core` percepts (`/learn`) never decay anywhere.
5. **Recency in recall** (D): `Query` orders by token hits, then `w + recencyBonus` where the bonus is `0.10 × 0.5^(age_days/30)` since `updated_at`. Ordering only — `min_confidence` still gates raw `w`, so age never hides a fact, and a more relevant old fact always outranks a fresher also-ran.

Promotion now runs **after** the global pass, so freshly promoted percepts aren't decayed again in the same run. The zombie band is closed on the upside by reinforcement (re-stated facts recover past `min_confidence`) and made honest on the downside by the recency tie-break; thresholds moved to `Params` (config `memory` block, prepopulated like Otel/Rules/Permissions) so tuning needs no rebuild.

## Consequences

- `Record`/`RecordGlobal` signature changed to `(RecordOutcome, error)`; `DuplicateError`/`IsDuplicate` removed — all call sites (`/learn`, `record_memory`, dispatch write-paths) updated. `/learn` now reports "already known — reinforced" instead of "skipped".
- Two latent issues fixed by construction: comments now match the constants, and the promotion dead-end for inference percepts is gone.
- Validation is deterministic and pinned as regression tests (`model_validation_test.go`): the exact reinforcement trajectory (0.7 → 0.75 → 0.80 → 0.85 → 0.90, promotion at consolidation), recency ordering (fresh wins ties; hits dominate; raw-`w` gating), global decay floor + core exemption, edge wiring both passes, and a corpus-effect run (env `MEMORY_CORPUS` runs it against a real store copy; synthetic mirror asserts trends — exact counts are float-noise sensitive around the prune threshold).
- Deferred: staleness-driven *expiry* for global percepts (the floor at 0.20 means very old unconfirmed facts rank last but persist — deletion policy stays `/forget` + eviction); edge inference by the model without tool parameters (agents must declare relations); per-relation boost tuning.
- The NREM metaphor is exactly that — a metaphor; no offline nightly pass exists (see the SWARMDEMO copy pass). Consolidation still runs at session end (`Consolidate` at exit/new paths).
