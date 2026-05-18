# milk

Local-first agentic orchestrator CLI. Routes prompts between a local LLM and Claude Code, maintaining session state across turns and promoting context on escalation.

The local agent speaks the OpenAI-compatible API — any compliant inference server works, local or remote (llama.cpp, Ollama, LM Studio, vLLM, or any hosted endpoint). Tested models: Qwen2.5-Coder (7B / 3B) and Gemma 4 E4B.

## Installation

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

Requires Go 1.21+ and Git. Installs the `milk` binary to `~/.local/bin/milk`.

To install a specific version:

```sh
MILK_VERSION=v0.1.0 curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

## How it works

Each prompt is routed through a decision chain:

1. Explicit flags (`--escalate`, `--local`) override everything
2. Session state — if Claude asked a follow-up question, the next turn goes directly back to Claude
3. Rules layer — hard thresholds (token length, keywords) then a weighted signal scorer
4. Local model classifier — the local model decides `local` or `escalate` when the scorer is inconclusive
5. Default: local

When the local model cannot handle a task, it calls `escalate_to_claude()` and milk reformats the local conversation history as context for Claude, which orients itself without a separate reformulation step.

## Prerequisites

| Dependency | Purpose | Required |
| --- | --- | --- |
| Inference server | local LLM inference | no (degrades to Claude-only) |
| [claude CLI](https://claude.ai/code) | rich agent | no (degrades to local-only) |
| Go 1.21+ | build only | yes |

milk communicates with the local model via the OpenAI-compatible API (default `http://localhost:8080`). Any compatible server works — [llama.cpp](https://github.com/ggml-org/llama.cpp), [Ollama](https://ollama.com), [LM Studio](https://lmstudio.ai), or any hosted endpoint — as long as the loaded model supports function/tool calling.

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

```text
[local] > fix the off-by-one in parser.go
[claude] > design an alternative approach
```

The prompt label reflects the current routing state: `[local]` (local model), `[claude]` (Claude), `[claude:waiting]` (Claude asked a follow-up — next turn goes directly back to Claude via `--resume`).

**Slash commands:**

| Command | Description |
| --- | --- |
| `/escalate` | Force next turn to Claude |
| `/local` | Force next turn to local model |
| `/learn <fact>` | Store a persistent memory (`Core=true`, W=1.0, global scope) |
| `/memory` | List all Percepts (global + session), sorted by confidence weight |
| `/memory global` | List only global Percepts |
| `/memory session` | List only session-scoped Percepts |
| `/memory <pattern>` | List Percepts whose content matches a substring |
| `/export` | Print the current session transcript |
| `/export json` | Print the current session as raw JSON |
| `/export <path>` | Write the session transcript to a file |
| `/metrics` | Show the most recent values of all observability metrics |
| `/otel` | Show OTel signal file sizes and record counts |
| `/otel trim` | Archive current OTel files and start fresh |
| `/otel off` / `/otel on` | Disable / re-enable OTel for this session |
| `/history` | Show current history mode and entry counts |
| `/history global` | Switch input navigation to global history (all sessions) |
| `/history session` | Switch input navigation to session history (default) |
| `/new` | Start a fresh session |
| `/drop` | Delete current session and start fresh |
| `/list` | List sessions for the current directory |
| `/help` | Show command reference |
| `/exit` | Quit |

**Tab completion:** `/` completes slash commands; `@` completes file paths from the current directory (e.g. `@src/main.go`).

**Keyboard shortcuts:**

| Key | Action |
| --- | --- |
| `Ctrl+C` | Cancel running turn (if busy); clear `/escalate`/`/local` flag; or exit |
| `Ctrl+D` | Exit (on empty input) |
| `Ctrl+R` | Reverse incremental search through input history |
| `Ctrl+S` | Forward incremental search through input history |
| `Up` / `Down` | Navigate input history (single-line input only) |
| `Ctrl+Up` / `Ctrl+Down` | Navigate input history (any input height) |
| `Ctrl+N` / `Shift+Alt+Enter` | Insert newline (multi-line input) |
| `PgUp` / `Ctrl+U` | Scroll transcript up |
| `PgDn` / `Ctrl+F` | Scroll transcript down |

**Input history** is persisted per session (`~/.milk/sessions/<id>.history`) and globally (`~/.milk/input_history`). Navigation defaults to session history; use `/history global` to switch.

### Single-prompt mode

```sh
milk [flags] <prompt>
```

### Flags

| Flag | Description |
| --- | --- |
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
  "llama_model": "qwen2.5-coder",
  "claude_bin": "claude",
  "default_route": "local",
  "otel": {
    "enabled": true,
    "traces": true,
    "metrics": true,
    "warn_mb": 50,
    "max_mb": 200,
    "metrics_flush_minutes": 5
  },
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

### Memory

milk maintains a persistent Percept store at `~/.milk/memory/`. Percepts are atomic natural-language assertions with a confidence weight that decays each session and rises when reinforced. At session end, NREM consolidation runs: decay → prune → promote high-weight Percepts to the global store.

### Observability

OTel signal files are written to `~/.milk/otel/`:

| File | Contents |
| --- | --- |
| `logs.jsonl` | Structured event logs (Percept records, consolidation runs, recalls) |
| `traces.jsonl` | Span traces per memory operation |
| `metrics.jsonl` | Counters and gauges (Percept counts, decay/prune/promote totals, file sizes) |

Use `/otel` to inspect sizes and `/otel trim` to archive and reset. Use `/metrics` to see the latest metric values inline.

## Local agent tools

The local model has access to these built-in tools:

| Tool | Description |
| --- | --- |
| `bash` | Run a shell command, returns stdout/stderr/exit code |
| `grep` | Search file contents by pattern |
| `read_file` | Read a file with optional offset and line limit |
| `write_file` | Write content to a file (creates parent directories) |
| `edit_file` | Exact-string replacement within a file |
| `list_dir` | List directory contents with type and size |
| `http_get` | Fetch a URL, returns body as text |
| `get_session_context` | Read the shared session history (filterable by agent, pattern, recency) |
| `record_memory` | Store a Percept in the memory store |
| `get_memory` | Retrieve Percepts matching a keyword query |
| `list_memory` | List all Percepts with optional scope/producer/pattern filters |
| `export_session` | Export the current session transcript as text or JSON |
| `get_metrics` | Show the latest observability metric values |
| `search_signals` | Search OTel signal files (logs/traces/metrics) for a pattern |
| `escalate_to_claude` | Hand off the current task to Claude with a reason |

## Graceful degradation

| Inference server | claude CLI | behaviour |
| --- | --- | --- |
| running | installed | normal routing |
| unreachable | installed | warns once, routes all turns to Claude |
| running | not installed | warns once, stays local-only |
| unreachable | not installed | exits with error |

## Documentation

- [docs/setup.md](docs/setup.md) — full setup guide and local testing procedure
- [docs/spec.md](docs/spec.md) — full architecture and design spec
- [docs/memory-design.md](docs/memory-design.md) — memory system design and phases
- [docs/observability-design.md](docs/observability-design.md) — OTel observability strategy
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch and commit conventions
