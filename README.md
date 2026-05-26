# milk

<p align="center">
  <img src="docs/images/milk.png" alt="milk" width="200"/>
</p>

Switch models, not context.

Start cheap. Go deep when you need it. Switch mid-workflow — the full conversation goes with you.

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
| Local agent (inference server or cloud) | local LLM inference | no (degrades to Claude-only) |
| [claude CLI](https://claude.ai/code) | rich agent | no (degrades to local-only) |
| Go 1.21+ | build only | yes |

The local agent supports multiple backends and auth transports — [llama.cpp](https://github.com/ggml-org/llama.cpp), [Ollama](https://ollama.com), [LM Studio](https://lmstudio.ai) and other OpenAI-compatible servers, plus cloud providers via native protocols: AWS Bedrock (SigV4 + Converse API), OpenRouter, Together.ai, Groq (Bearer token). The only requirement is that the model supports function/tool calling.

If no local agent is configured, milk starts and shows setup guidance. Use `/provider add` to configure a backend interactively, or edit `~/.milk/config.json` directly.

For a reference setup (NVIDIA GPU, Ubuntu/WSL2, llama.cpp from source) and local testing procedure see [docs/setup.md](docs/setup.md).

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
| `/escalate` | Pin all subsequent turns to Claude (until `/local`) |
| `/escalate <msg>` | Force this single turn to Claude, then resume normal routing |
| `/local` | Pin all subsequent turns to local model (until `/escalate`) |
| `/local <msg>` | Force this single turn to local model, then resume routing |
| `/learn <fact>` | Store a persistent memory (`Core=true`, W=1.0, global scope) |
| `/memory` | List all Percepts (global + session), sorted by confidence weight |
| `/memory global` | List only global Percepts |
| `/memory session` | List only session-scoped Percepts |
| `/memory <pattern>` | List Percepts whose content matches a substring |
| `/memory show <pat\|#id>` | Show full details of matching Percepts |
| `/panel memory` | Toggle the memory panel (right-side Percept viewer) |
| `/forget <pat>` | Delete a Percept by description or `#id` (confirms before deleting) |
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
| `/skip-permissions` | Show current `dangerously_skip_permissions` state |
| `/skip-permissions on\|off` | Enable / disable permission skip for this session |
| `/provider` | Show active local-agent provider (URL, model, auth) |
| `/provider list` | List all configured local-agent backends |
| `/provider switch <name>` | Switch active local agent to a named backend |
| `/provider add` | Add a new backend interactively (prompts for missing fields) |
| `/new` | Start a fresh session |
| `/drop` | Delete current session and start fresh |
| `/list` | List sessions for the current directory |
| `/help` | Show command reference |
| `/exit` | Quit |

**Tab completion:** `/` completes slash commands; `@` completes file paths from the current directory (e.g. `@src/main.go`).

**Keyboard shortcuts:**

| Key | Action |
| --- | --- |
| `Ctrl+C` | Cancel running turn (if busy); or exit (press twice on empty input) |
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
  "local_agent": "my-local",
  "local_agents": [
    {
      "name": "my-local",
      "url": "http://localhost:8080",
      "model": "qwen2.5-coder"
    },
    {
      "name": "haiku",
      "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
      "model": "arn:aws:bedrock:...",
      "provider": "bedrock",
      "aws_region": "eu-central-1"
    },
    {
      "name": "openrouter",
      "url": "https://openrouter.ai/api/v1",
      "model": "meta-llama/llama-3-70b-instruct",
      "provider": "bearer",
      "api_key": "sk-or-..."
    }
  ],
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

`local_agent` names the active backend from the `local_agents` list. Use `/provider switch <name>` to change it at runtime. `/provider add` configures a new backend interactively.

Supported `provider` values: omit (or `""`) for no-auth/local, `"bedrock"` (AWS SigV4), `"bearer"` (API key). For Azure OpenAI, omit `provider` and pass `"api-key": "..."` under `headers`.

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

| Local agent | claude CLI | behaviour |
| --- | --- | --- |
| available | installed | normal routing |
| unavailable | installed | warns once, routes all turns to Claude |
| available | not installed | warns once, stays local-only |
| not configured | not installed | starts in setup mode — splash shows `/provider add` guidance |
| unavailable | not installed | starts in setup mode — splash shows `/provider add` guidance |

## Documentation

- [docs/setup.md](docs/setup.md) — full setup guide and local testing procedure
- [docs/spec.md](docs/spec.md) — full architecture and design spec
- [docs/memory-design.md](docs/memory-design.md) — memory system design and phases
- [docs/observability-design.md](docs/observability-design.md) — OTel observability strategy
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch and commit conventions
