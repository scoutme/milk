# 3. Claude via CLI Subprocess, not Direct API

Date: 2026-05-05

## Status

Accepted

## Context

The rich agent tier needs to use Claude. Integration options are: (a) call the Anthropic API directly, or (b) invoke the `claude` CLI binary as a subprocess.

## Decision

Invoke Claude via `claude --print --output-format stream-json` as a subprocess. Use `--resume <session-id>` for turn-level session continuity.

## Consequences

Claude Code CLI brings its own tool ecosystem (Bash, Edit, Read, MCP, file access) that would need to be reimplemented if using the raw API. The CLI handles authentication, session persistence, and context window management. The NDJSON stream-json format is machine-parseable for real-time token relay. The trade-off is a dependency on the `claude` binary being installed; mitigated by graceful degradation to local-only mode when unavailable.
