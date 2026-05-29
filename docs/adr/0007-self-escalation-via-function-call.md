# 7. Local Model Self-Escalation via Function Call

Date: 2026-05-05

## Status

Accepted

## Context

Rules-based routing cannot cover all cases where a task exceeds the local model's capability. The system needs a mechanism for the local model to signal that it cannot handle a prompt and escalation is needed.

## Decision

The local model can trigger escalation by returning a function call `escalate(reason: string)` rather than a text response. This is exposed to the model as a standard OpenAI function schema alongside the other tools (bash, grep, read_file). The tool was originally named `escalate_to_claude`; it was renamed to `escalate` in [ADR-0030](0030-agent-flavours-unified-config.md) to reflect that the escalation target is not necessarily Claude.

## Consequences

The mechanism is unambiguous — no parsing of hedging language or quality assessment of the output is needed. It requires the local model to reliably support function calling. Qwen2.5-Coder does. If a different local model is configured without function calling support, self-escalation falls back silently to text output and the rules layer is the only escalation trigger.
