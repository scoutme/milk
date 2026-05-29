# 28. Multi-Provider Local-Agent Configuration

Date: 2026-05-25

Status: Superseded by [ADR-0030](0030-agent-flavours-unified-config.md)

## Context

The original config schema used a flat set of `llama_*` fields to describe a single local inference backend. As remote providers (Bedrock, OpenRouter, etc.) joined the picture the flat schema became unwieldy: a second backend required either a full restart with different config or manual JSON surgery.

Two concrete needs drove the refactor:

1. A user needs to switch between a cloud Bedrock backend and a local llama.cpp server within the same session.
2. The `/provider` command needed a named, addressable list to populate `list` and `switch` subcommands.

## Decision

Replace the flat `llama_*` fields with a `local_agents` array and a `local_agent` selector:

```json
{
  "local_agent": "haiku",
  "local_agents": [
    { "name": "haiku", "url": "...", "model": "...", "provider": "bedrock", "aws_region": "eu-central-1" },
    { "name": "local", "url": "http://localhost:8080", "model": "gemma4" }
  ]
}
```

`ActiveLocalAgent()` on `Config` resolves the active backend: match `local_agent` name (case-insensitive) → first entry → backward-compat flat `llama_*` fields → built-in defaults.

`NewFromConfig` in `internal/agent/local` now takes a `LocalAgentConfig` directly instead of the full `Config`, so the construction path is stateless and testable.

## Consequences

**Good:**
- Named, switchable backends with no restart required (`/provider switch <name>`)
- Interactive wizard for adding new backends at runtime (`/provider add`)
- Clean construction: callers resolve the active config once and pass it down
- Full backward compatibility: old configs with flat `llama_*` fields continue to work

**Neutral:**
- Both schema variants must be maintained; `ActiveLocalAgent()` carries the promotion logic permanently

**Bad:**
- Two config representations for the same concept; `LocalAgentConfig` and the flat `llama_*` fields can drift if new fields are added only to one

## Alternatives considered

**Single-entry swap** — replace the flat fields with a single `local_agent` object (not a list). Simpler, but `/provider list` and multi-backend switching would require a second refactor.

**Runtime-only switching without config persistence** — keep the single flat schema but allow in-memory override. The `/provider add` command's value (adding and persisting a new backend) would not be achievable.

## Superseded by

[ADR-0030](0030-agent-flavours-unified-config.md) extended this decision further: `local_agents` was renamed to `agents`, `local_agent` to `agent`, `LocalAgentConfig` to `AgentConfig`, and the Claude CLI was moved from root config fields (`claude_bin`, `dangerously_skip_permissions`, etc.) into a first-class `agents` entry with `provider: "claude-cli"`. The old schema is automatically migrated on first load.
