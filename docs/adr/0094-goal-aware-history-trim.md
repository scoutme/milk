# ADR 0094 — Goal-aware history trim: the superseded ask is not pinned

## Status

Accepted (2026-10-30). Amends the head+tail retention described in
[ADR 0032](0032-returning-fresh-start-on-stale-context.md)'s 2026-10-29 update
and `docs/internal/analysis/prompt-context-management-review.md` §8. Issue #362.

## Context

`trimSplitSpans` pins the transcript head ("the task definition must survive")
and keeps the recent tail; the middle is summarized or dropped. After a goal
switch the pinned head is the *stale original ask*, verbatim, while the new goal
could sit in the dropped middle or carry the "verify before acting" label. The
old escalation-only history scoping avoided this by deleting everything before
the escalation boundary — which also deleted the agent's own work and caused
the recap failures behind PRs #359/#360, so it was removed for non-CLI runners
(PR #361). The trim-proof "Goal changed" orientation marker (PR #363) only
covered sessions with escalation history.

## Decision

1. **`RecordNeed` position fixed.** It runs before the turn's user message
   reaches `History` (ADR-0038) but stored `len(History)`; every reader expects
   `len(History)+1`. A first-turn need collided with the unset sentinel and a
   switch right after an escalation answer was not seen as a switch.
2. **Boundary-aware trim.** `buildAgentHistoryWithBoundary` reports the message
   index where the current need's turn begins (one turn expands to a variable
   number of messages, so it can only be read off while building). `trimPlan`
   uses it:
   - pre-boundary requests compete oldest-first — the stale ask is the first
     thing to go, and if removing just enough of them fits the budget, the plan
     ends there (everything from the switch on stays verbatim);
   - only when the goal's own turns exceed the budget do all pre-boundary
     messages go, and the post-boundary turns get ordinary head+tail retention
     (the switch turn and the first plan stay pinned);
   - the orientation prefix is never trimmed; with compaction, one summary
     covers all removed spans and is flagged as including superseded requests.
   No usable boundary (no need, need on the first turn, out of range) is
   exactly the previous behavior. Nothing changes when the history fits.
3. **Primary marker broadened.** `Session.NeedSupersedesEarlier` (a need
   declared with earlier user turns before it) also triggers the "Goal changed"
   marker in the primary orientation, not only `NeedChangedSinceLastEscalation`.
   The escalation orientation keeps its escalation-scoped condition.

## Consequences

- Unlike the removed scoping, this is pressure-triggered and non-destructive:
  superseded content is summarized, never silently deleted, and recent
  pre-switch messages stay verbatim while they fit.
- When everything fits, the stale ask stays verbatim at the top; only the
  marker counters it.
- A model that re-emits a need tag for the *same* task mid-conversation moves
  the boundary; the earlier turns then compete as droppable/summarizable, which
  is lossy but not destructive.
- Persisted sessions hold the old `CurrentNeedSetAt` (one turn early); harmless
  and self-correcting on the next need.
