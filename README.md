# milk

Local-first agentic orchestrator CLI. Routes prompts between a local LLM (Gemma 4 via llama.cpp) and Claude Code, maintaining session state across turns and promoting context on escalation.

## How it works

Each prompt is routed through a decision chain:

1. Explicit flags (`--escalate`, `--local`) override everything
2. Session state — if Claude asked a follow-up question, the next turn goes directly back to Claude
3. Rules layer — hard thresholds (token length, keywords) then a weighted signal scorer
4. Local model classifier — Gemma 4 decides `local` or `escalate` when the scorer is inconclusive
5. Default: local

When the local model cannot handle a task, it calls `escalate_to_claude()` and milk reformats the local conversation history as context for Claude, which orients itself without a separate reformulation step.

## Prerequisites

| Dependency | Purpose | Required |
|---|---|---|
| [llama.cpp server](https://github.com/ggml-org/llama.cpp) | local LLM inference | no (degrades to Claude-only) |
| [claude CLI](https://claude.ai/code) | rich agent | no (degrades to local-only) |
| Go 1.21+ | build only | yes |

milk communicates with the local model via the OpenAI-compatible API (default `http://localhost:8080`). Any compatible server works — [llama.cpp](https://github.com/ggml-org/llama.cpp), [Ollama](https://ollama.com), [LM Studio](https://lmstudio.ai), or similar — as long as the loaded model supports function/tool calling. Gemma 4 E4B is the reference model.

For a reference setup (NVIDIA GPU, Ubuntu/WSL2, llama.cpp from source) and local testing procedure see [docs/setup.md](docs/setup.md).

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

### Interactive mode

```sh
milk
```

Starts a REPL session with colored prompt, Tab completion, and command history.

```
[local] > fix the off-by-one in parser.go
[claude] > design an alternative approach
```

The prompt label reflects the current routing state: `[local]` (local model), `[claude]` (Claude), `[claude:waiting]` (Claude asked a follow-up — next turn goes directly back to Claude via `--resume`).

**Slash commands:**

| Command | Description |
|---|---|
| `/escalate` | Force next turn to Claude |
| `/local` | Force next turn to local model |
| `/new` | Start a fresh session |
| `/drop` | Delete current session and start fresh |
| `/list` | List sessions for the current directory |
| `/help` | Show command reference |
| `/exit` | Quit |

**Tab completion:** `/` completes slash commands; `@` completes file paths from the current directory (e.g. `@src/main.go`).

**Keyboard shortcuts:** Ctrl-C clears a pending `/escalate` or `/local` flag (or exits if none is set); Ctrl-D exits.

### Single-prompt mode

```sh
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
| `--list --all` | List all sessions across all directories (use with `--list`) |
| `--all` | Modifier for `--list`: show sessions for all directories |
| `--drop` | Delete the current session |

### Examples

```sh
# Interactive session
milk

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
  "llama_model": "gemma-4-e4b",
  "claude_bin": "claude",
  "default_route": "local",
  "rules": {
    "escalate_above_tokens": 2000,
    "local_below_tokens": 30,
    "escalate_keywords": ["architect", "refactor entire", "design", "explain why", "analyze", "describe", "summarize"],
    "escalate_threshold": 6,
    "local_threshold": -4,
    "local_verb_weight": -3,
    "escalate_verb_weight": 4,
    "path_ref_weight": -2,
    "code_block_weight": -2,
    "open_question_weight": 3,
    "classifier_fallback": "local"
  }
}
```

**`milk config`** — print the effective configuration (merged defaults + `~/.milk/config.json`).

### Session storage

Sessions persist under `~/.milk/sessions/`. By default, `milk` resumes the most recent session for the current directory. Multiple named sessions can coexist in the same directory.

## Local agent tools

The local model has access to these built-in tools:

| Tool | Description |
|---|---|
| `bash` | Run a shell command, returns stdout/stderr/exit code |
| `grep` | Search file contents by pattern |
| `read_file` | Read a file with optional offset and line limit |
| `write_file` | Write content to a file (creates parent directories) |
| `edit_file` | Exact-string replacement within a file |
| `list_dir` | List directory contents with type and size |
| `http_get` | Fetch a URL, returns body as text |

## Graceful degradation

| llama.cpp | claude CLI | behaviour |
|---|---|---|
| running | installed | normal routing |
| down | installed | warns once, routes all turns to Claude |
| running | not installed | warns once, stays local-only |
| down | not installed | exits with error |

## Documentation

- [docs/setup.md](docs/setup.md) — full setup guide and local testing procedure
- [docs/spec.md](docs/spec.md) — full architecture and design spec
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch and commit conventions
