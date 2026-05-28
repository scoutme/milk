# ADR 0029 — Summary Bricks for Escalation Context

## Status

Accepted

## Context

When milk escalates to Claude Code, it injects the local session history via
`--append-system-prompt-file` (ADR 0004). The previous implementation serialised
the full `sess.History` into a flat transcript. For long sessions this exceeded
165K tokens — leaving fewer than 35K tokens for actual work. Claude Code's
autocompact would trigger, refill within 2–3 turns, and trip the `rapid_refill_breaker`
circuit breaker, aborting the session entirely.

Two additional problems compounded this:
- On `--resume`, the same full history was re-injected on top of Claude's own
  accumulated conversation, doubling the context load.
- Tool-result verbosity was unbounded (a per-result 500-char cap existed but
  there was no cap on the number of turns).

## Decision

Replace the raw-history injection with **summary bricks**: four maintained fields
on `Session`, rebuilt eagerly after each turn.

### New Session fields

| Field | Description |
|---|---|
| `current_need` | One-sentence description of what the user is trying to accomplish. Updated by `<milk:need:NONCE>` tag from either agent on context switch. |
| `escalation_brief` | Tactical reason set by `escalate_to_claude(reason)`. Overwritten on each agent-triggered escalation. |
| `last_local_summary` | Pre-rendered, sanitized, budget-capped summary of local-agent turns **since the last Claude turn**. |
| `last_claude_summary` | Pre-rendered, sanitized, budget-capped summary of Claude turns (reserved for future demotion). |

### Context injected per situation

| Situation | Injected |
|---|---|
| First escalation, user `/escalate` | identity + `current_need` + `last_local_summary` + need/percept instructions + percepts |
| First escalation, agent `escalate_to_claude()` | identity + `escalation_brief` + `current_need` + `last_local_summary` + need/percept instructions + percepts |
| `--resume` (any trigger) | identity + `current_need` + `last_local_summary` + need/percept instructions + percepts |

On `--resume`, Claude already has its own conversation history — only the local
activity since the last Claude session plus the current goal are injected.

### Sanitization in brick rebuild

- Consecutive identical tool calls (same name, across consecutive turns) are
  collapsed: first call shown + `[N more X calls collapsed]`.
- Tool results are truncated at 500 characters.
- Turns are included newest-first; the oldest are dropped when the character
  budget is exhausted.
- An empty Claude session (0 tool calls AND < 200 chars of assistant text) does
  not update `last_claude_summary`.

### `<milk:need:NONCE>` tag

Both agents are instructed (via `NeedInstruction` in the system prompt) to emit:

```
<milk:need:NONCE>one-sentence user goal</milk:need:NONCE>
```

when the user switches context, or on the first user message when no goal is set.
The stream parser intercepts and strips this tag (same pattern as percept tags).
`Session.CurrentNeed` is updated in-place; last value wins.

### Budget configuration

`context_budget_chars` in `~/.milk/config.json` (default: 12000). Applied
per brick (`last_local_summary`, `last_claude_summary` independently).

## Consequences

- System prompt size at escalation is now bounded regardless of session length.
- Claude receives only what it doesn't already know: what the local model has
  done since Claude was last active.
- The `rapid_refill_breaker` failure mode is eliminated for normal sessions.
- A session with a very long single local turn (e.g. a huge tool result as the
  most recent turn) may still approach the budget, but cannot exceed it.
- `last_claude_summary` is maintained but not yet used (pending demotion feature).
- Existing sessions without brick fields load fine; bricks are rebuilt on the
  next turn.
