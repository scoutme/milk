# ADR 0032 — Returning Fresh-Start on Stale Context

Date: 2026-06-08

## Status

Accepted

## Context

When the escalation agent (Claude CLI) is invoked in `ContextModeReturning` — that is, a prior
escalation session exists but the primary agent has completed some work in the interim — milk
currently always passes `--resume <escalation-session-id>`. This re-loads Claude's full stored
conversation: every assistant turn, tool call, and tool result from all prior escalation turns.

The motivation for `--resume` in `ContextModeReturning` is continuity: Claude can refer back to what
it previously did. However, the system context files injected via `--append-system-prompt-file`
already provide full re-orientation: identity block, escalation brief, current user need, and a
rolling summary of recent primary-agent activity. When the prior escalation context is no longer
relevant to the current task, `--resume` loads that full history at token cost with no benefit.

Two conditions identify when the prior escalation history is stale:

1. **Topic switched.** `Session.CurrentNeed` is written by `<milk:need:NONCE>` tags each time the
   user's goal changes. `Session.CurrentNeedSetAt` encodes the history position where it was last
   set. If `CurrentNeedSetAt` is newer than the last escalation turn, the user has switched topics
   since the escalation agent last spoke. The prior escalation turns are about a different task.

2. **Wide local-turn gap.** A large number of local-agent turns since the last escalation turn
   indicates that significant work has happened in the primary agent. The re-orientation context
   (brief + local summary) already captures this work in condensed form. The raw escalation turn
   history from before that gap is progressively irrelevant as the gap grows.

In either case, starting a **fresh Claude session** (dropping `--resume`) while still injecting the
full re-orientation context is cheaper and equally informed.

## Decision

Add a `returning_fresh_start_local_turns` config knob (global + per-agent via `AgentLimits`) with
a default of **8 local turns**. When dispatching a `ContextModeReturning` escalation, check two
conditions before calling `--resume`:

- **Need staleness:** `CurrentNeedSetAt` is set (non-zero) and greater than the history position of
  the last escalation assistant turn (i.e. the need changed after the last escalation turn).
- **Turn gap:** the number of local-agent assistant turns since the last escalation boundary exceeds
  `returning_fresh_start_local_turns` (0 = disabled).

If either condition holds, `runCLIEscalationAgent` downgrades `ContextModeReturning` to
`ContextModeFirst` before selecting the call path. This causes `RunFirst` to be used — no
`--resume` — and a fresh `EscalationSessionID` is stored.

The re-orientation context injected via `--append-system-prompt-file` (identity block, escalation
brief, current need, primary summary) is identical regardless of mode, so Claude receives
equivalent guidance in both cases.

The `returning_fresh_start_local_turns` threshold of 0 disables the turn-gap condition entirely.
The need-staleness condition is always active when `CurrentNeedSetAt` is set; it cannot be
disabled independently (the data is only present when a need tag was actually emitted).

## Consequences

- `ContextModeReturning` turns after a topic switch or a wide gap no longer pay to reload a stale
  prior escalation session. Token cost is the re-orientation context files only.
- When the prior escalation session was productive and the user immediately re-escalates after a
  small number of local turns on the same topic, `--resume` still fires and continuity is
  preserved.
- The stale `EscalationSessionID` is overwritten when `RunFirst` succeeds, so subsequent turns
  continue from the new session.
- The new config knob lives alongside the existing `memory_reinjection_turns` /
  `memory_reinjection_bytes` thresholds in `AgentLimits`, following the same nil/negative/zero/
  positive semantics. Default 8 is intentionally conservative: most topic switches emit a
  `<milk:need:NONCE>` tag within one or two turns, so the turn-gap condition is a backstop for
  cases where the need tag was not emitted.
