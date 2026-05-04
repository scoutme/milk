# 4. Context Handoff via --append-system-prompt

Date: 2026-05-05

## Status

Accepted

## Context

When escalating from the local agent to Claude, the local conversation history must be transferred. Options are: (a) a separate LLM reformulation call, (b) template-based summarization, or (c) passing the raw transcript to Claude directly.

## Decision

Pass the formatted local conversation transcript via `--append-system-prompt` on the first escalation turn. Claude orients itself from this context without a separate reformulation step.

## Consequences

Claude receives more tokens (full transcript vs. summary), which increases cost on escalation. This is acceptable because escalation is the exception rather than the rule, and Claude's comprehension is sufficient to parse a raw transcript. A separate reformulation call would add latency and cost on every escalation.
