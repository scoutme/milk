# 6. ESCALATION_WAITING State for Turn-Level Routing

Date: 2026-05-05

## Status

Accepted. Renamed in [ADR-0030](0030-agent-flavours-unified-config.md): `CLAUDE_WAITING` → `ESCALATION_WAITING` to reflect that the escalation agent is no longer necessarily Claude.

## Context

When the escalation agent asks a follow-up question mid-conversation, the next user input must go back to the escalation agent rather than being re-evaluated by the router. Without special handling, the router could send a short answer (e.g., "yes") to the local model, breaking conversational continuity.

## Decision

When the escalation agent's response ends with a question, transition the session to `ESCALATION_WAITING` state. The next user turn bypasses the router and goes directly to the escalation agent via `--resume <session-id>` (or the equivalent for non-CLI escalation). The state is broken only by an explicit `--local` flag from the user.

## Consequences

Conversational continuity is preserved within an escalated session. The user retains explicit control to break the continuation via `--local` (`--primary` in the TUI). The trade-off is that the router never gets a chance to demote back to local automatically mid-conversation; this is intentional and listed as a backlog item (automatic demotion).
