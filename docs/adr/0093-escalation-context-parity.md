# ADR 0093 — Escalation Context Parity: summary as index, transcript as ground truth

## Status

Accepted (2026-10-29). Amends [ADR 0029](0029-summary-bricks-escalation-context.md)
for the local/inference-server hand-off path; claude-cli keeps ADR 0029's
bounded-resume design unchanged.

## Context

ADR 0029 replaced full-history injection into the Claude CLI escalation with
bounded "summary bricks" — correct there, because Claude `--resume` already
owns its conversation and re-serialising `sess.History` doubled the context
(165K+ tokens, autocompact refill loops). That rationale **does not apply to
local/HTTP providers**: a local agent has no native session, its entire view is
rebuilt from `sess.History` every turn. For it, the brick was not protecting a
history — it was *replacing* one, lossily.

The full-3way eval showed the cost: `milk-esc` (same harness, stronger model)
lost to `milk-auto` exactly on restraint and *verification evidence* — "only
claims to have tested", "didn't explicitly verify". Five stacked losses made
the escalation view strictly worse than the primary's:

1. the primary's tool evidence was dropped from history entirely
   (`buildAgentHistoryFull` kept its text, `ownTrailReplays` replayed only the
   receiving agent's own trails);
2. the brick truncated what little it carried (silent 500-char result cap,
   oldest turns dropped first at `context_budget_chars`);
3. the escalation history budget was smaller — orientation (brief/need/brick)
   subtracted from the same message budget the primary spent on transcript;
4. the transcript trim was drop-oldest, so the original task framing was the
   first casualty;
5. hand-off framing primed a reviewer ("Escalation brief from primary agent",
   "Expect mid-conversation hand-offs"), not an owner.

Product requirement: **escalation must not receive a worse picture than the
primary had; summarization must grant the full picture, not limit access to
it.** Full analysis:
[docs/internal/analysis/escalation-context-parity.md](../internal/analysis/escalation-context-parity.md).

## Decision

**Summary as index, transcript as ground truth.** The brick stays as
orientation (a TL;DR telling the agent where to look); the transcript stops
being lossy for cross-agent work.

- **A — evidence-preserving cross-agent rendering** (`local.NarrateTrails`,
  `cmd/milk/main.go`): the other agent's `Turn.Trail` is narrated into the
  receiving agent's history as third-person lines —
  `[<name> called <tool>(<args>)]` / `[result: …]`, role-safe system-role text,
  never first-person tool calls the receiving agent never issued. Retention is
  planned by the shared `planTrails` — the same newest-2-expansion /
  160KB-newest-first / manifest-for-old policy `ReplayTrails` gives an agent
  its own trails: **retention parity, symmetric cost**. Legacy pre-trail turns
  (bare `ToolCalls` + `RoleToolResult`) are narrated the same way.
- **B — budget parity and head+tail trim** (`cmd/milk/runner.go`,
  `cmd/milk/main.go`, `internal/config`): the escalation message budget no
  longer subtracts orientation — same formula as the primary path; the brick
  shrinks to a TL;DR (`context_budget_chars` default 12000 → 3000) so total
  request size stays roughly flat while the transcript grows. The transcript
  trim (`trimSplitSpans`) replaces drop-oldest with head+tail: the original
  request + first plan and the recent turns survive at message-segment
  boundaries (a tool call is never split from its result), the middle is
  summarized (`trimLocalMessagesWithCompaction`, spliced between head and tail)
  or dropped with a marker. Benefits both roles.
- **C — the brick is an addition, not a filter** (`internal/session`): every
  truncation is explicit — results over the per-result cap (raised 500 → 1500)
  get `… (truncated — full result in the session history)`, trimmed bricks get
  `(earlier activity omitted — N turns)`, cleared capture-time results say so.
  Trail-era turns render their tool activity in the brick (calls + results),
  not just final text. A single over-budget turn keeps a head+tail of itself
  (`textbudget.SummarizeLong`) instead of emptying the brick.
- **D — continuity framing, not takeover** (`internal/escalation`,
  `internal/agent/local`): the shared history is presented as the session's own
  working context — "you have the complete conversation including tool
  activity; continue as the owner of this work and don't recap unless asked" —
  and the reviewer-priming labels ("Escalation brief from primary agent",
  "Expect mid-conversation hand-offs") are gone.
- **E — tests pin the requirement**: other-agent trail evidence appears in the
  receiving history; for the same session each side's rendering carries every
  text, tool name, argument and result the other shows (superset test); every
  brick truncation carries a marker; head+tail trim keeps the task definition.

## Consequences

- Escalation (local providers) now sees what the primary *did and found*, not
  just what it claims — the two eval misses this targets are structurally
  addressed.
- Cross-agent history grows by the narrated trails under the same bounded
  retention the primary pays for itself; the brick shrink offsets it, so
  in-token cost is roughly flat (validated by the `esc-parity` eval experiment:
  milk-auto vs milk-esc, in-token parity + per-criterion gaps).
- Lazy mode (`experimental_lazy_history_management`) stays opt-in lossy;
  `ReplayTrails` own-turn constants stay as they were.
- claude-cli escalation keeps ADR 0029's bounded bricks (its double-injection
  constraint is real); it inherits C's honest markers and D's framing through
  the shared builders.
