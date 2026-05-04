# 2. Single llama.cpp Instance for Routing and Coding

Date: 2026-05-05

## Status

Accepted

## Context

The router needs a model to classify prompts when rules are inconclusive. A separate tiny classifier model (Gemma 2B, Phi-3 mini) would be faster, but requires a second llama.cpp process and a second model file in memory.

## Decision

Use one Qwen2.5-Coder instance for both routing classification and local coding/shell tasks. No separate classifier model or second llama.cpp process.

## Consequences

Baseline RAM is higher than a dedicated tiny classifier would require. Mitigated by the rules layer running first, so the model is only invoked when rules are inconclusive. Qwen2.5-Coder supports function calling, which enables the `escalate_to_claude(reason)` self-escalation mechanism — a tiny model cannot reliably do this.
