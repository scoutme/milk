# 1. Go + Cobra CLI

Date: 2026-05-05

## Status

Accepted

## Context

milk needs a CLI interface for a single user. Language and framework choices affect subprocess control, streaming I/O, distribution, and availability of agent libraries.

## Decision

Implement milk in Go using the Cobra framework.

## Consequences

No mature Go agent framework exists, so agent orchestration logic must be implemented from scratch. Go gives strong subprocess control, native streaming I/O, and a single static binary for distribution. Python alternatives (LangChain, AutoGen) are opinionated frameworks that do not integrate cleanly with the Claude Code CLI subprocess model.
