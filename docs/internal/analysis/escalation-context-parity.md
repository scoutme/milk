# Escalation context parity — problem assessment & proposed solution

Status: implemented (2026-10-29) — A+B+C+D+E landed on
`feat/escalation-context-parity` (ADR-0093), to be validated by the
`esc-parity` eval experiment (`~/.milk/eval/experiments/esc-parity.yaml`,
milk-auto vs milk-esc on the three failing criteria plus in-token parity)
before merge. Scope: `internal/escalation/`, `cmd/milk/main.go`
history builders, `internal/session` summary bricks, `cmd/milk/runner.go`
budgeting. Motivated by the full-3way eval finding that `milk-esc` (same
harness, stronger model) loses to `milk-auto` exactly on *restraint and
verification evidence* — and by the product requirement: **escalation must not
receive a worse picture than the primary had; summarization must grant the full
picture, not limit access to it.**

## 1. Why the mechanism looks the way it does (and where that rationale expired)

ADR-0029 (summary bricks) was written for the **claude-cli `--resume`** path:
Claude Code already owns its conversation, and re-serialising the whole
`sess.History` on every hand-off doubled the context (165K+ tokens, autocompact
refill loops, `rapid_refill_breaker` aborts). There, "inject only what the
resumed session doesn't already know" is correct.

That rationale **does not apply to local/HTTP providers** (the path every
`milk-esc` eval arm uses — e.g. `mimo-pro-local`). A local agent has no native
session: its entire view is rebuilt from `sess.History` on every turn
(`localRunner.Execute`, `cmd/milk/runner.go`). For it, the summary brick is not
protecting a native history — it is *replacing* one, lossily. The brick
machinery became the universal cross-agent hand-off without being re-derived
for the stateless case.

## 2. Where the escalation view is worse than the primary's — five stacked losses

Each layer is individually defensible; together they explain the observed
behavior (pro "pads", "claims to have tested", under-demonstrates).

1. **The other agent's tool evidence is dropped from history entirely.**
   `buildAgentHistoryFull` (`cmd/milk/main.go`) keeps the primary's assistant
   *text* as `[name as primary] …` but includes `RoleToolResult` only
   `if t.Agent == selfAgent`, and `ownTrailReplays` replays only the receiving
   agent's own trails ("another agent's tool calls would be nonsense to a model
   that never issued them"). The data is recorded — `dispatch.go` stores
   `Trail` on every local and escalation turn — it is simply never rendered for
   the other side. Escalation therefore sees the primary's **claims without
   evidence**: exactly the two eval misses ("only claims to have tested
   full-length…", "didn't explicitly verify n=0").

2. **The summary brick truncates the evidence it does carry.**
   `renderTurns` (`internal/session/session.go`) truncates tool results at
   **500 chars**, collapses consecutive same-name calls to
   `[N more X calls collapsed]`; `buildBrick` drops **oldest turns first**
   under `context_budget_chars` (default 12,000) — i.e. the earliest work (the
   task setup) disappears first.

3. **The escalation history budget is smaller than the primary's.**
   `localRunner.Execute` (RoleEscalation) computes
   `msgBudget = AgentMessageBudget − (SystemOverheadChars + len(orientationText))`
   — orientation (brief/need/brick) eats the same budget the primary spends on
   transcript. `trimSplitIndex` then trims **drop-oldest-first**, so the
   original user framing is the first casualty.

4. **Own-trail replay is lossy too** (`internal/agent/local/trail.go`
   `ReplayTrails`): only `trailRecentTurns` (2) newest tool-using turns are
   expanded; older turns become one-line manifests; results beyond
   `trailReplayProtectBytes` (160KB, newest-first) become placeholders.
   Symmetric across roles — noted for completeness, not escalation-specific.

5. **Framing primes a reviewer, not an owner.** The hand-off blocks say
   "Escalation brief from primary agent", "Recent primary agent activity",
   "Prior escalation session summary — you previously worked with this user;
   this is what you last did together", and the identity block sets
   "Expect mid-conversation hand-offs". Combined with 1–2 (conclusions without
   evidence), a takeover/reviewing posture is the predictable output — the
   padding and "claims to have tested" findings.

Also relevant: on `ContextModeFirst` after prior escalation activity,
`escalationLocalHistoryFresh` scopes history to *since the last escalation
boundary* (older local turns survive only as brick summaries), and lazy mode
(`experimental_lazy_history_management`) collapses other-agent turns into
`(N turns omitted)` placeholders — opt-in pure loss.

Measured symptom (routing-reps, turn 1): escalation's request carried 8,483
input tokens vs 918 for direct flash — the orientation/re-injection machinery
dwarfs the actual request, while still omitting the primary's evidence.

## 3. Principle

**Summary as index, transcript as ground truth.** The brick stays — as
orientation (a TL;DR that tells the agent where to look). The transcript stops
being lossy for cross-agent work: the receiving agent gets *at least* the
evidence retention the originating agent keeps for itself, and the summary
never silently *replaces* evidence it could *point to*.

## 4. Proposed changes

### A. Evidence-preserving cross-agent rendering (the core fix)

Render the other agent's `Turn.Trail` in history as **narrated transcript**
instead of dropping it: e.g.

```
[mimo-local called read_file(path=eval/pricing.go)]
[result: …]        ← bounded by the same trail budgets as own replay
[mimo-local]: Added `Max` to … (final answer)
```

Role-safe for OpenAI-compat APIs (system/user-role narration, no
assistant/tool pairing required — the `ownTrailReplays` "nonsense" objection
applies only to replaying as *first-person tool calls*). Reuse the existing
budgets (`trailReplayProtectBytes`, `trailRecentTurns`, `TrailManifest` for old
turns) so cost is bounded and **symmetric**: same retention the agent has for
itself. Cheap first step: manifest lines (tool name + args) for all
other-agent turns + full results for the last K turns.

### B. Budget parity and head+tail trimming

- Don't let orientation steal transcript budget. Preferred shape: **shrink the
  brick to a fixed-size TL;DR** (it duplicates the transcript once A lands) and
  spend the recovered budget on the transcript. Net token cost roughly flat,
  information strictly greater.
- Replace drop-oldest (`trimSplitIndex`) with **head+tail** retention via the
  house helper `internal/textbudget`: keep the original user request + first
  plan (head) and the recent turns (tail), summarize/drop the middle. A
  takeover agent needs the task definition more than the middle of the work.
  (Benefits both roles.)

### C. The brick becomes an addition, not a filter

- Keep `[Recent primary agent activity]` as orientation.
- Replace silent 500-char truncation with an explicit pointer:
  `… (truncated — full result in transcript)`; raise the per-result cap or make
  it configurable (`limits.handoff_result_chars`).
- When the brick trims turns, say so: `(earlier activity omitted — N turns)` —
  never silently.

### D. Reframe the hand-off narration

Continuity instead of takeover: present the transcript as *the session's shared
conversation* (which it is), and state the operating expectation once:
"You have the complete conversation including tool activity — treat it as your
own working context and continue as the owner; don't recap unless asked."
This is a deliberate behavior change to the hand-off blocks (not to user or
scenario prompts) and must be validated by the eval (§5).

### E. Pin the parity requirement in tests

- other-agent trail evidence appears in escalation history (A),
- for the same session, escalation's rendering ⊇ primary's rendering modulo
  speaker labels (B),
- every brick truncation carries an explicit marker (C).

## 5. Validation

The eval now has exactly the instrument: re-run the routing scenarios (where
the failure is concentrated) with the current build vs the fixed build —
`milk-esc` arm before/after on the three failing criteria (proportional
padding, edge_cases demonstration, dependency_injection) plus in-token parity
vs `milk-auto`. Routing n=3 suffices (highly deterministic cells). Note for
the record: this change *is* the thing under test (milk's hand-off quality),
so an A/B through the harness is the correct validation, unlike the rejected
eval-isolation prompt guard (issue #355) which altered the *scenarios*.

## 6. Scope & non-goals

- **claude-cli path keeps ADR-0029's bounded bricks** (the double-injection
  constraint is real there). Optionally add the C markers there too. If a
  future parity pass is wanted for CLI, it belongs behind the resume boundary,
  not in the injected file.
- Lazy mode (`experimental_lazy_history_management`) stays opt-in lossy.
- `ReplayTrails` own-turn constants stay (symmetric, bounded by design).
- No change to scenario/user prompts; no change to what is *stored* (already
  sufficient).

## 7. Alternatives considered

- **Brick-only tuning** (raise `context_budget_chars`, raise the 500-char cap):
  improves symptoms but keeps claims-without-evidence — fails the requirement.
- **Full verbatim transcript of everything, no budgets**: recreates ADR-0029's
  blowup on long sessions; rejected.
- **Minimal A + D only**: the highest behavioral leverage for the least risk —
  a valid first PR if B is deferred.

## 8. Recommendation

Land **A + C + D** as the first PR (evidence rendering + honest truncation +
framing), with the §5 A/B study; then **B** (brick shrink + head+tail trim) as
the second, measured the same way. Both are `internal/` + `cmd/milk` changes
with testable seams (§4-E).
