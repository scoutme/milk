# 6. CLAUDE_WAITING State for Turn-Level Routing

Date: 2026-05-05

## Status

Accepted

## Context

When Claude asks a follow-up question mid-conversation, the next user input must go back to Claude rather than being re-evaluated by the router. Without special handling, the router could send a short answer (e.g., "yes") to the local model, breaking conversational continuity.

## Decision

When Claude's response ends with a question, transition the session to `CLAUDE_WAITING` state. The next user turn bypasses the router and goes directly to `claude --resume <session-id>`. The state is broken only by an explicit `--local` flag from the user.

## Consequences

Conversational continuity is preserved within an escalated Claude session. The user retains explicit control to break the continuation via `--local`. The trade-off is that the router never gets a chance to demote back to local automatically mid-conversation; this is intentional and listed as a backlog item (automatic demotion).
