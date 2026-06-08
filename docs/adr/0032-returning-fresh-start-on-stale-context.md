# ADR 0032 — Returning Fresh-Start on Stale Context

Date: 2026-06-08

## Status

Accepted

## Context

### Claude CLI path

When the escalation agent (Claude CLI) is invoked in `ContextModeReturning` — that is, a prior
escalation session exists but the primary agent has completed some work in the interim — milk
previously always passed `--resume <escalation-session-id>`. This re-loads Claude's full stored
conversation: every assistant turn, tool call, and tool result from all prior escalation turns.

The motivation for `--resume` in `ContextModeReturning` is continuity: Claude can refer back to what
it previously did. However, the system context files injected via `--append-system-prompt-file`
already provide full re-orientation: identity block, escalation brief, current user need, and a
rolling summary of recent primary-agent activity. When the prior escalation context is no longer
relevant to the current task, `--resume` loads that full history at token cost with no benefit.

### Local-provider path

Non-CLI escalation agents (plain HTTP, Bedrock, OpenRouter, etc.) have two additional gaps that
the CLI path does not:

1. **No structural orientation.** The CLI path injects an identity block, escalation brief, current
   need, and local-activity summary via `BuildDynamicContext` before every non-resume turn. The
   local-provider path passed raw undifferentiated history with no framing. The agent had to infer
   its role and the current task from conversation context alone, which is a quality regression on
   every escalation turn.

2. **Budget trimmer as history policy.** When accumulated history exceeded `MessageBudgetChars`,
   the trimmer dropped oldest-first. For a returning turn this is the worst heuristic: it discards
   the oldest escalation work (which may still be thematically relevant) before the stale local
   turns in the gap between escalation sessions.

3. **No percept injection.** The CLI path injects relevant percepts from the memory store via
   `BuildStaticContext`. The local-provider path did not inject any percepts.

### Staleness conditions (both paths)

Two conditions identify when the prior escalation history is stale:

1. **Topic switched.** `Session.CurrentNeed` is written by `<milk:need:NONCE>` tags each time the
   user's goal changes. `Session.CurrentNeedSetAt` encodes the history position where it was last
   set. If `CurrentNeedSetAt` is newer than the last escalation turn, the user has switched topics
   since the escalation agent last spoke. The prior escalation turns are about a different task.

2. **Wide local-turn gap.** A large number of local-agent turns since the last escalation turn
   indicates that significant work has happened in the primary agent. The re-orientation context
   already captures this work in condensed form. The raw escalation turn history is progressively
   irrelevant as the gap grows.

## Decision

### Config knob

Add `returning_fresh_start_local_turns` (global + per-agent via `AgentLimits`, default **8**).
When dispatching any escalation returning turn, check:

- **Need staleness:** `CurrentNeedSetAt` is set and newer than the last escalation assistant turn.
- **Turn gap:** local turns since the last escalation boundary ≥ `returning_fresh_start_local_turns`
  (0 = disabled).

### Claude CLI path

If either condition holds, `runCLIEscalationAgent` downgrades `ContextModeReturning` to
`ContextModeFirst`. This causes `RunFirst` to be used — no `--resume` — and a fresh
`EscalationSessionID` is stored. The re-orientation context files carry equivalent guidance.

### Local-provider path

`runEscalationLocal` gains mode-awareness (first vs returning, detected from history via
`EscalationEverActive`) and applies the same staleness check. On every turn, regardless of
staleness:

- **Orientation message:** `BuildDynamicContext` is called with the appropriate mode (first or
  returning) and prepended to the history array as a `role: system` message. This gives the agent
  the same identity/brief/need/local-summary framing the CLI path has always had.
- **Percepts message:** `FormatPercepts` renders the relevant memory store entries as a
  `[Remembered facts]` block, also prepended as a `role: system` message. The Bedrock
  `convertMessagesToConverse` path already extracts system-role messages into the separate `system`
  array, so this works identically on both HTTP and Bedrock backends.
- **Budget accounting:** the orientation message length is added to the `SystemOverheadChars`
  estimate before the budget trimmer runs, so the trimmer has an accurate view of what will be sent.

On stale-returning turns, the history is additionally scoped to turns since
`LastEscalationBoundary` via `escalationLocalHistoryFresh`, excluding the stale prior escalation
turns from the messages array entirely. The orientation message already provides equivalent
high-level context.

`EscalationEverActive` and `LastEscalationBoundary` are added to `session.Session` as exported
helpers for use by the non-CLI path (which does not persist `EscalationSessionID`).

`escalation.FormatPercepts` is exported from the escalation package (previously unexported) to
allow direct percept injection without routing through `BuildStaticContext`.

## Consequences

- Both CLI and local-provider paths now apply identical staleness logic. The shared config knob
  (`returning_fresh_start_local_turns`) controls both.
- Local-provider escalation agents now receive the same structural orientation on every turn that
  Claude has always received. Quality of first-turn responses after a topic switch or a returning
  escalation improves.
- Percepts from the memory store are now injected for local-provider escalation agents, matching
  the CLI path.
- The budget trimmer for local providers now has accurate overhead accounting, reducing the chance
  of unexpected context overruns after the orientation message is added.
- When the prior escalation session was productive and the user immediately re-escalates on the
  same topic within the turn-gap threshold, the full prior history is still passed and continuity
  is preserved.
- The need-staleness condition is always active when `CurrentNeedSetAt` is set and cannot be
  disabled independently. The turn-gap condition is disabled by setting the threshold to 0.
- Default of 8 local turns is intentionally conservative: most topic switches emit a
  `<milk:need:NONCE>` tag within one or two turns, so the turn-gap check is a backstop for cases
  where the need tag was not emitted.
