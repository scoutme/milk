# Implementation plan: escalation-mechanism and request-length enhancements

Status: **landed** — all five phases below shipped (per-phase status lines carry
the branch/commit); one Phase 3 sub-item, "auto-escalate on fail-closed", was
deliberately dropped after investigation. This file is kept as the design record,
not maintained in sync with the implementation.

## Overview

Follow-up to [`docs/internal/analysis/session-2026-10-01-primary-overload-analysis.md`](../analysis/session-2026-10-01-primary-overload-analysis.md),
which found a 3.5h dogfooding session where the primary model (`mimo-v2.6-flash`) carried a
complex, multi-hour task entirely alone — the router never escalated once — including a 16-minute
turn where the model got stuck repeating tool calls 5 separate times. Milk's loop detector and
payload trim both worked as designed, but neither one ever considers escalating to a different,
more-capable agent — they only ever crop/nudge/terminate the *same* stuck model. And the whole
investigation had to be done by grepping a process-wide text log, because none of this is
OTel-instrumented.

This plan has two independent tracks that can land separately:

- **Track A — escalation mechanism**: give the loop-recovery ladder and the long-running-turn
  case an actual path to a more-capable agent, instead of only ever re-trying the same one.
- **Track B — request-length / context management**: stop the payload-trim loop from thrashing
  on every iteration of a long tool-calling turn, and close the OTel visibility gap that made
  this analysis require manual log scraping instead of a structured query.

Track B's observability phase (Phase 1) should land first regardless of order otherwise chosen —
every later phase in both tracks becomes easier to validate once loop/trim events are visible as
structured data instead of only as `slog` text lines.

## Code map (verified by direct file:line lookup, not guessed)

| Concern | Location |
|---|---|
| Router decision chain | `internal/router/router.go:77-120`, `internal/router/rules.go:36-81` — pre-turn, one-shot, text-only classification; no duration/iteration/recovery signal |
| `ESCALATION_WAITING` state | `internal/session/session.go:21-24`; set in `cmd/milk/dispatch.go:564-569` (only after an escalation turn ends with an open question) |
| Model-initiated `escalate(reason)` tool | schema `internal/agent/local/tools.go:364-375`; dispatch `tools.go:708`; surfaced as `EscalationSignal` `internal/agent/local/local.go:2125-2126`; mid-turn state flip `cmd/milk/dispatch.go:307-313` |
| Forced escalation (existing precedent) | repeated-user-prompt detector, `internal/agent/local/local.go:1099-1169` (threshold `line 1125`) fires `&EscalationSignal{...}` at `local.go:1230-1241` **before the model is even called** — proof this pattern (milk-forced escalation, not model-initiated) already exists and works |
| Shared recovery ladder | `internal/agent/local/loop_streak.go:190-218` (`loopRecoveryAction`) — crop → mild nudge (`recoveryCount<2`) → strong nudge (`>=2`) → terminate (`>maxRecovery`); never touches `EscalationSignal` |
| Duplicate-tool-call detector | `internal/agent/local/local.go:1448-1489`; counter `duplicateRecoveryCount` feeds `loopRecoveryAction` at `local.go:1480` |
| Doom-loop gate | `internal/agent/local/local.go:1409-1446`; threshold `loop_streak.go:335` (`doomLoopThreshold = 3`); interactive ask via `a.permAsk("doom_loop", ...)`, fail-closed when `a.workflowRole \|\| a.jobID != "" \|\| a.permAsk == nil` |
| Byte-size payload trim | `internal/agent/local/local.go:2683-2704` inside `streamCompletionOnce`; limit `internal/config/config.go:392` (`DefaultMaxPayloadBytes = 900*1024`); fires **every HTTP request**, hard-drops via `dropOldestDroppableUnit`, no summarization |
| Char-budget compaction w/ summarization | `cmd/milk/main.go:1548` (`trimLocalMessagesWithCompaction`), trigger `trimSplitIndex`/`messagesCharCount` (`main.go:1513-1515`), budget from `cfg.AgentMessageBudget` (`config.go:1407`); called **once per turn boundary** from `cmd/milk/runner.go:265,297`, never from inside the per-iteration tool loop |
| OTel metrics that exist today | `milk.turns.*`, `milk.inference.*`, `milk.tools.*`, `milk.router.*`, `milk.tokens.*`, `milk.memory.*`, `milk.session.*` — full call-site list in the analysis doc's agent report; confirmed **no** `milk.loop.*` or trim-event metric/log anywhere (now closed by Track B Phase 1, see below) |

---

## Track B, Phase 1 — Close the OTel visibility gap (land first)

**Status: landed** (branch `feat/loop-trim-observability`). Implemented, tested (including a
live `obs.Init`-backed wiring check, not just unit tests — see commit), and build-clean. One
deliberate deviation from the sketch below: metric tag is called `detector` (free-text detector
name, e.g. `"duplicate tool call"`, `"doom_loop"`) rather than a closed `kind` enum, since
`loopRecoveryAction` already receives `detectorName` as a string from each of its four callers —
reusing it avoided a second parallel taxonomy. `session_id` was added to the plain-text `slog`
lines (not a new OTel log record) as the lowest-churn way to close the attribution gap; OTel
*metric* attributes deliberately stay low-cardinality (`model`/`agent`/`detector`/`outcome`
only — no `session_id`, which would make the counter's time series grow unbounded forever).

**Why first:** every other phase below changes behavior inside the recovery/trim paths; without
structured events, validating "did this actually reduce recovery/trim frequency" means repeating
the same manual `milk.log` grep done for the analysis doc. This phase only adds observation, no
behavior change — lowest risk, enables everything else to be measured.

**Files:** `internal/agent/local/local.go`, `internal/agent/local/loop_streak.go`, `internal/obs/obs.go` (or wherever metric name constants live today).

1. Add two new metric names: `milk.loop.recovery` (counter, tags: `kind` = `duplicate_tool_call` / `ngram` / `text_loop` / `streak` / `doom_loop`, `agent`, `model`) and `milk.inference.payload_trimmed` (counter, tags: `agent`, `model`).
2. Single choke point for the first one: inside `loopRecoveryAction` (`loop_streak.go:190`), right alongside the existing crop/nudge `a.logWarn` calls (lines ~204, ~213) — this one function is already called by all four detector types, so one hook here covers duplicate-tool-call, n-gram, text-loop, and streak recoveries without touching four call sites separately. The doom-loop gate (`local.go:1409-1446`) is a separate code path (never calls `loopRecoveryAction`) and needs its own `obs.Inc(..., "kind", "doom_loop")` call at the point it decides `failClosed` or records a denial/approval.
3. Second one: `streamCompletionOnce`, `local.go:2702`, next to the existing `a.logWarn("payload after trimming", ...)`.
4. Also tag the existing plain-text `slog` WARN lines (`loopRecoveryAction`'s crop/nudge logs, the doom-loop messages, the payload-trim logs) with `session_id` the same way `internal/memory`'s OTel logs already are — addresses the analysis doc's "concurrent session attribution" gap, where a second session's background job (`job=job_4`) WARN lines were interleaved with the target session's own entries in the same process-wide `milk.log`, with nothing but the `job=` field to disambiguate.
5. Tests: unit test asserting `loopRecoveryAction` emits exactly one `milk.loop.recovery` counter increment per call, tagged with the `detectorName` passed in; same for the payload-trim site.

---

## Track B, Phase 2 — Stop the byte-trim loop from thrashing on long turns

**Status: landed** (branch `feat/payload-trim-compaction-fallback`). Implementation diverged
from the sketch below in one load-bearing way, discovered mid-implementation: the per-request
trim in `streamCompletionOnce` is *request-local* — it builds a trimmed copy of `msgs` for one
outgoing HTTP call and never returns it to the caller, so `runToolLoop`'s own `msgs` (which
grows every tool-call iteration) was never actually shrunk by it. A fix placed inside
`streamCompletionOnce` (as originally sketched) would therefore have made exactly one outgoing
request richer and changed nothing about trim *frequency* — the real bug. The actual fix lives in
`runToolLoop` instead: a proactive, cheap char-count heuristic (`messagesContentBytes`) checked
before each iteration's `streamCompletion` call, incrementing a local `payloadTrimCountThisTurn`
counter; at `a.payloadCompactionThreshold` (config: `payload_trim_compaction_threshold`, default
3, wired via `cmd/milk/runner.go`), it calls the new `(*Agent).compactForPayloadSize` — which
collapses *all* prior turns in one shot when any exist, or the *older half* of the current turn's
own accumulated tool-call tail when none do (protecting the most recent iterations) — and
**reassigns `runToolLoop`'s own `msgs`**, persisting the reduction for every subsequent iteration
instead of re-deriving a throwaway copy each time. `streamCompletionOnce`'s original per-request
hard-drop trim is untouched and still runs as the final safety net on every request regardless.
Verified safe to mutate mid-turn: `runToolLoop`'s returned `[]Message` only ever feeds
`TurnResult.Text` (the last assistant message) — it is never written back into persisted session
history, so a mid-turn compaction can only affect the rest of *this* turn, never future ones.
Added `milk.inference.payload_compacted` counter + a `logWarn` line (both following Phase 1's
convention) alongside the existing `milk.inference.payload_trimmed` one.

**Why:** the analysis doc's 2h21m turn re-exceeded the 900KB cap and got re-trimmed on nearly
every iteration for ~1h45m straight (02:05→03:50), each time hard-dropping oldest content with
`dropOldestDroppableUnit` — no summarization, unlike the turn-boundary compaction path that
*does* exist (`trimLocalMessagesWithCompaction`) but never runs mid-turn.

**Files:** `internal/agent/local/local.go`.

1. Add a per-turn counter `a.payloadTrimCountThisTurn` (reset at the start of each `Run` call, alongside existing per-turn counters like `duplicateRecoveryCount`).
2. In the trim block (`local.go:2686-2704`), increment it each time the block fires. On the **Nth** occurrence within a single turn (configurable, default e.g. 3 — needs a config knob, call it `payload_trim_compaction_threshold`), instead of continuing to loop `dropOldestDroppableUnit` on every subsequent request, call the same summarization primitive `trimLocalMessagesWithCompaction` already uses (`Agent.Summarize`, `cmd/milk/main.go:1569`) to collapse the dropped span into one message once, then continue with hard-drop trimming only if that single collapse still isn't enough to fit.
3. This reuses an existing, already-tested code path (`Agent.Summarize`) rather than inventing a new compaction strategy — the only new logic is the per-turn counter and the threshold check that decides *when* to reach for it instead of the cheaper hard-drop.
4. Tests: a long synthetic turn whose history keeps growing past the byte cap on each iteration should trigger exactly one `Summarize` call after the configured threshold, not one per iteration; a short turn that trims once or twice should never call `Summarize` at all (preserves current cheap behavior for the common case).
5. Depends on Phase 1's `milk.inference.payload_trimmed` counter to validate in a live session that trim frequency actually drops after this change.

---

## Track A, Phase 3 — Give the recovery ladder an escalation option

**Status: landed** (branch `feat/forced-escalation-after-recoveries`). Two scope reductions
from the sketch below, both discovered mid-implementation and necessary for correctness — not
just simplifications:

- **No "is an escalation agent configured" check inside `internal/agent/local`.** The existing
  precedent (`isRepeatedPrompt`'s forced escalation) doesn't check for one either — it just
  returns `&EscalationSignal{...}` unconditionally and lets `cmd/milk/dispatch.go` handle graceful
  degradation (per CLAUDE.md's "Graceful degradation" table). This phase follows the same
  pattern rather than inventing agent-level awareness of session/config state it doesn't have.
- **"Auto-escalate on fail-closed" (item 3 below) is dropped**, not implemented. Investigation
  found background jobs have *no real escalation path at all*: `escalate` is deliberately excluded
  from their tool schema (ADR-0043's cost-risk isolation), and a background job's `run` closure
  just records any returned error — including an `EscalationSignal` — as a plain job failure, not
  an actual hand-off. A stateless tool-agent call (`RunToolCall`) has no session/runner behind it
  to escalate into either (its own "defense in depth" comment in `cmd/milk/runner.go` says so
  explicitly). And a workflow step's `EscalationSignal` would reach the interpreter with no
  handling for it at all. Auto-escalating any of these three would silently degrade to a confusing
  result instead of the plain crop/nudge/terminate ladder they already handle correctly today — so
  a new `(*Agent).canEscalate()` guard (false for workflow role, background jobs, and tool-agent
  calls) gates **every** new escalation decision in this phase, and the fail-closed doom-loop branch
  is untouched. What *did* ship for the doom-loop gate: a **denied** interactive ask now escalates
  instead of terminating (when `canEscalate()`) — a user explicitly saying "no, don't keep doing
  that" is itself a strong signal the primary model isn't handling the task, and handing off is
  strictly better than giving up. This needed no new three-way prompt UI: the existing allow/deny
  ask is unchanged, only what happens on *deny* changed.

Everything else matches the sketch: a per-turn aggregate `totalRecoveryCount` local var in
`runToolLoop`, threaded into `loopRecoveryAction` (now returning a third `escalate bool`, checked
*before* the max-recovery-exceeded terminate check — the two conditions can coincide, and
escalating is strictly better than either continuing or terminating), config
`escalate_after_recoveries` (default 4, `AgentEscalateAfterRecoveries` accessor,
`WithEscalateAfterRecoveries` builder, wired in `cmd/milk/runner.go` alongside Phase 1/2's
builders), and the same `milk.loop.recovery`/`session_id`-tagged-logWarn conventions
(`outcome=escalated`).

**Why:** `loopRecoveryAction` and the doom-loop gate currently only ever crop/nudge/terminate the
*same* stuck model — confirmed by direct code read, no exception. The repeated-user-prompt
detector (`local.go:1099-1169`) already proves the pattern of milk forcing an `EscalationSignal`
without waiting for the model to ask — this phase applies that same proven pattern to intra-turn
model-stuck signals instead of only user-frustration signals.

**Files:** `internal/agent/local/local.go`, `internal/agent/local/loop_streak.go`, `internal/config/config.go`.

1. Add a per-turn aggregate counter across *all* detector types (`a.totalRecoveriesThisTurn`), incremented wherever `duplicateRecoveryCount`, `ngramRecoveryCount`, `streak.recoveryCount`, or `textLoopRecoveryCount` is incremented today.
2. Add config `escalate_after_recoveries` (default e.g. 4 — one more than the analysis session's 5 recoveries would still be a close call; needs tuning against real data once Phase 1 lands and more sessions are observable). When `a.totalRecoveriesThisTurn` crosses this threshold **and an escalation agent is configured**, return `&EscalationSignal{Reason: "primary model required N loop recoveries in a single turn"}` from the current call site instead of (or immediately after) the next `loopRecoveryAction` call — reusing the exact mid-turn state-flip path already wired at `cmd/milk/dispatch.go:307-313` for model-initiated `escalate()`.
3. Doom-loop gate (`local.go:1409-1446`) gets a third option alongside allow/deny in the interactive ask, when an escalation agent is configured and idle: "escalate instead of retrying" — maps to the same `EscalationSignal` path rather than either continuing or terminating. For the fail-closed case (background/workflow role, no `permAsk`), auto-escalate instead of terminating outright, if an escalation agent is configured — terminating a background job after 3 identical calls currently wastes the job entirely; handing it to a more-capable agent is strictly better than discarding it when one is available.
4. Tests: a synthetic turn that crosses `escalate_after_recoveries` produces an `EscalationSignal` with the expected reason string; a turn below threshold behaves exactly as today (crop/nudge only); doom-loop gate with no escalation agent configured falls back to exactly today's behavior (no regression for the fail-closed/no-agent-configured case).
5. Depends on Phase 1 to measure, across future sessions, whether this materially reduces time-to-resolution versus letting the ladder keep nudging the same model.

---

## Track A, Phase 4 — Cheap self-escalation hinting (no new state machinery)

**Status: landed** (`03d80af` — escalate hint appended to the strong nudge tier only,
with the tier-split assertion test).

**Why:** lowest-risk complement to Phase 3 — makes the *existing* model-initiated `escalate`
tool more likely to actually get used, without adding any new forced-escalation logic.

**Files:** `internal/agent/local/loop_streak.go`.

1. The mild/strong nudge text already injected by `loopRecoveryAction` (constants referenced at
   `local.go:1480`, e.g. `recoveryDuplicateToolMild`/`Strong`) gets one appended sentence on the
   **strong** nudge tier only (`recoveryCount >= 2`): something like "If this keeps happening,
   call `escalate(reason)` instead of retrying again." The `escalate` tool and its guidance text
   already exist (`local.go:1069`) and are purely model-initiated — this just surfaces the option
   at the exact moment milk's own telemetry (the recovery counter) shows the model is already
   struggling, which today it has no way to know about itself.
2. Tests: nudge text assertion for the strong tier includes the escalate hint; mild tier
   (`recoveryCount < 2`) does not (avoid suggesting escalation on the first, often-harmless, nudge).

---

## Track A, Phase 5 — Long-but-not-looping turns: make `token_velocity` actionable

**Status: landed** (branch `feat/token-velocity-actionable`). The product question was asked
directly: suggestion-only, confirmed — no auto-escalation. Implementation notes:

- `token_velocity` is only ever computed once a turn has already finished (`Detector.Feed` is
  called from `handleAgentDone` after the turn's final token counts are known — there is no
  mid-stream path for it), so this can only ever inform the *next* turn, exactly as planned.
- `autoStickyEscalate` turned out not to be cleanly reusable for a mere suggestion — it's defined
  as a side effect of the router having *actually* escalated that turn, not a standalone flag. The
  implementation just checks `!m.st.stickyEscalate && !m.st.autoStickyEscalate` (nothing new to
  suggest if the session is already headed to escalation) and otherwise appends `" — consider
  /escalate"` to the verdict's message — no new session state needed.
- `token_velocity`'s confidence is always ≥ 0.8 (ratio-scaled, capped at 0.9 — see
  `checkTokenVelocity`), which in the existing `handleAgentDone` branch structure meant it never
  reached `m.loopWarning` (the actual status-bar field) at all — only the transcript line. Fixed
  as part of this change: the token_velocity case now also populates `m.loopWarning`, which is
  what the plan's "status-bar hint" actually required.
- **Testing boundary, disclosed rather than skipped:** `token_velocity` is a TUI-only signal
  (`cmd/milk/repl.go`'s `handleAgentDone`, part of the bubbletea `Update` cycle) — unlike the
  agent-internal detectors Phases 1/3/4 touched, it has no code path reachable from single-prompt
  CLI mode at all, and each single-prompt CLI invocation is a fresh OS process with a brand-new,
  empty-history `loop.Detector` anyway, so the cross-turn window this signal depends on can never
  accumulate that way. A true interactive-TUI live test (scripted keystrokes across multiple
  real turns) was not attempted. Verified instead by driving the actual production
  `(model).handleAgentDone` function directly with a real `loop.Detector` and the real
  `internal/obs` token accumulator (not mocks) — the full test suite passes, but this is not the
  same as a human (or scripted) multi-turn TUI session.

**Why:** the analysis doc's 2h21m turn never triggered any loop/duplicate detector (it was
legitimate incremental progress, not repetition) — the only signal that fired was the **existing**
TUI-level `token_velocity` cross-turn signal (`internal/loop/detector.go`), and only *after* the
turn had already finished, as a pure warning nobody could act on retroactively.

**Files:** `internal/loop/detector.go`, `cmd/milk/repl.go`.

1. This is a smaller, lower-confidence change than Phases 3-4 (open design question, not yet a
   concrete code plan): when `token_velocity` fires, instead of only logging a warning, surface a
   status-bar hint suggesting `/escalate` for the *next* turn if the session isn't already sticky-
   escalated — reusing the existing sticky-escalation UI affordance (`autoStickyEscalate`,
   documented in `CLAUDE.md`'s Session states section) rather than inventing a new one.
2. Needs product-level discussion before implementation: should this ever *auto*-escalate (risk:
   false positives on legitimately long but fine local turns), or always stay a human-directed
   suggestion? Recommend starting suggestion-only, given `token_velocity`'s `interrupt=false`
   default and the existing `auto_interrupt: false` config convention for loop signals.

---

## Suggested landing order

1. **Track B Phase 1** (observability) — always first, no behavior change, de-risks everything after it.
2. **Track A Phase 4** (nudge hint) — trivial, no new state, immediately useful.
3. **Track B Phase 2** (trim→compaction fallback) — contained to `local.go`, reuses existing `Summarize`.
4. **Track A Phase 3** (forced escalation after N recoveries) — the highest-value, highest-risk change; wants real data from Phase 1 against more sessions before picking a good default threshold.
5. **Track A Phase 5** (token_velocity actionability) — open design question, do last / separately.

## Testing & rollout notes

- Each phase needs the usual live-test-before-merge pass in the TUI against a real local model
  session (not just unit tests) — per project convention, build+unit-tests-green is not sufficient
  sign-off for a change in this hot path.
- Phase 3's default threshold should not be hard-committed from this single session's data point
  (N=5 recoveries in one turn) — treat it as a starting guess, tune once Phase 1's
  `milk.loop.recovery` counter gives real distribution data across more sessions.
- `feat/<scope>` branch per phase, conventional commits, no direct commits to `main`.
