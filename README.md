# milk

Local-first agentic orchestrator CLI. Routes prompts between a local LLM (Qwen2.5 via llama.cpp) and Claude Code, maintaining session state across turns and promoting context on escalation.

## How it works

Each prompt is routed through a decision chain:

1. Explicit flags (`--escalate`, `--local`) override everything
2. Session state — if Claude asked a follow-up question, the next turn goes directly back to Claude
3. Rules layer — token length threshold and keyword patterns
4. Local model classifier — Qwen2.5 decides `local` or `escalate`
5. Default: local

When the local model cannot handle a task, it calls `escalate_to_claude()` and milk reformats the local conversation history as context for Claude, which orients itself without a separate reformulation step.

## Prerequisites

| Dependency | Purpose | Required |
|---|---|---|
| [llama.cpp server](https://github.com/ggml-org/llama.cpp) | local LLM inference | no (degrades to Claude-only) |
| [claude CLI](https://claude.ai/code) | rich agent | no (degrades to local-only) |
| Go 1.21+ | build only | yes |

llama.cpp must expose an OpenAI-compatible API (default `http://localhost:8080`) loaded with a Qwen2.5-Coder model.

## Install

```sh
go install github.com/scoutme/milk/cmd/milk@latest
```

Or build from source:

```sh
git clone https://github.com/scoutme/milk
cd milk
go build -o milk ./cmd/milk
```

## Usage

```
milk [flags] <prompt>
```

### Flags

| Flag | Description |
|---|---|
| `--escalate` | Force this turn to Claude |
| `--local` | Force this turn to the local model; breaks a CLAUDE_WAITING session |
| `--new` | Start a fresh session for the current directory |
| `--session <name>` | Resume or create a named session |
| `--list` | List sessions for the current directory |
| `--list --all` | List all sessions across all directories |
| `--drop` | Delete the current session |

### Examples

```sh
# Simple shell automation — routed to local model
milk "list all Go files modified in the last week"

# Force Claude for architecture decisions
milk --escalate "design a caching layer for this service"

# Named session for a specific feature
milk --session auth-refactor "what does the current middleware do?"

# Continue a session after Claude asks a follow-up
milk "yes, use Redis"

# Break out of a Claude conversation back to local
milk --local "grep for TODO comments"

# Inspect all active sessions
milk --list --all
```

### Configuration

milk reads `~/.milk/config.json` on startup, falling back to defaults if absent.

```json
{
  "llama_url": "http://localhost:8080",
  "llama_model": "qwen2.5-coder",
  "claude_bin": "claude",
  "default_route": "local",
  "rules": {
    "escalate_above_tokens": 2000,
    "escalate_keywords": ["architect", "refactor entire", "design", "explain why"]
  }
}
```

Print the effective configuration:

```sh
milk config
```

### Session storage

Sessions persist under `~/.milk/sessions/`. By default, `milk` resumes the most recent session for the current directory. Multiple named sessions can coexist in the same directory.

## Graceful degradation

| llama.cpp | claude CLI | behaviour |
|---|---|---|
| running | installed | normal routing |
| down | installed | warns once, routes all turns to Claude |
| running | not installed | warns once, stays local-only |
| down | not installed | exits with error |

## Documentation

- [docs/spec.md](docs/spec.md) — full architecture and design spec
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch and commit conventions
