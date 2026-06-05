# milk

![milk](docs/images/milk.png)

Switch models, not context.

Start cheap. Go deep when you need it. Switch mid-workflow — the full conversation goes with you.

## How it works

Each prompt is routed through a decision chain:

1. Explicit flags (`--escalate`, `--primary`) override everything
2. Session state — if the escalation agent asked a follow-up question, the next turn goes directly back to it
3. Rules layer — hard thresholds (token length, keywords) then a weighted signal scorer
4. Primary model classifier — the primary model decides `local` or `escalate` when the scorer is inconclusive
5. Default: local

When the primary model cannot handle a task, it calls `escalate(reason)` and milk reformats the primary conversation history as context for the escalation agent, which orients itself without a separate reformulation step.

## Prerequisites

| Dependency | Purpose | Required |
| --- | --- | --- |
| Primary agent (inference server or cloud) | primary LLM inference | no (degrades to escalation-agent-only) |
| Escalation agent (any `agents` entry) | deep reasoning / rich tooling | no (degrades to local-only) |
| Go 1.21+ | build from source only | no (pre-built binaries available) |

The primary agent supports multiple backends: [llama.cpp](https://github.com/ggml-org/llama.cpp), [Ollama](https://ollama.com), [LM Studio](https://lmstudio.ai), AWS Bedrock, OpenRouter, Together.ai, Groq, and any OpenAI-compatible server. The escalation agent can be any of the above, or the [Claude Code CLI](https://claude.ai/code) (`provider: "claude-cli"`) — a powerful option when available but not required.

If no agent is configured, milk starts in setup mode. Use `/agent add` to configure a backend interactively.

For a reference setup (NVIDIA GPU, Ubuntu/WSL2, llama.cpp from source) see [docs/setup.md](docs/setup.md). For provider-specific setup (Bedrock, OpenRouter, Groq, Copilot, Azure, etc.) see [docs/providers.md](docs/providers.md).

## Installation

### Linux / macOS

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

Downloads a pre-built binary for your OS/arch and installs it to `~/.local/bin/milk`. No Go required.

To install a specific version:

```sh
MILK_VERSION=v0.2.0 curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

### Windows (native)

```powershell
irm https://raw.githubusercontent.com/scoutme/milk/main/install.ps1 | iex
```

Installs to `%LOCALAPPDATA%\milk\bin\milk.exe` and adds it to your user PATH.

### From source

Requires Go 1.21+ and Git.

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install-from-source.sh | sh
```

Or with `go install`:

```sh
go install github.com/scoutme/milk/cmd/milk@latest
```

### Windows

Native Windows is not yet fully supported. The recommended path is **WSL2** (Windows Subsystem for Linux), which gives you a full Linux environment where milk runs without modification.

1. [Install WSL2](https://learn.microsoft.com/en-us/windows/wsl/install) and a Linux distribution (Ubuntu 22.04 or 24.04 recommended)
2. Inside a WSL2 terminal, follow the Linux installation steps above

**Known limitations on native Windows (without WSL2):**
- `go build ./...` compiles, but the `bash` local-agent tool hard-codes `sh -c` and will fail
- `install.sh` requires a POSIX shell; there is no `.ps1` equivalent yet
- `scripts/llama-serve.sh` has no PowerShell equivalent

See [docs/setup.md](docs/setup.md#windows-and-wsl2) for the full Windows/WSL2 setup walkthrough.

## Usage

### Interactive mode

```sh
milk
```

Starts an interactive session. The status bar shows the current routing state, active agent, and availability.

**Slash commands:**

| Command | Description |
| --- | --- |
| `/escalate` | Pin all subsequent turns to escalation agent (until `/primary`) |
| `/escalate <msg>` | Force this single turn to escalation agent, then resume routing |
| `/primary` | Pin all subsequent turns to primary agent (until `/escalate`) |
| `/primary <msg>` | Force this single turn to primary agent, then resume routing |
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
| `/usage` | Token usage report (cumulative, this session, since start) |
| `/metrics` | Show the most recent values of all observability metrics |
| `/otel` | Show OTel signal file sizes and record counts |
| `/otel trim` | Archive current OTel files and start fresh |
| `/otel off` / `/otel on` | Disable / re-enable OTel for this session |
| `/setup telegram` | Configure Telegram remote oversight interactively |
| `/setup telegram on\|off` | Enable / disable Telegram (credentials preserved) |
| `/history` | Show current history mode and entry counts |
| `/history global` | Switch input navigation to global history (all sessions) |
| `/history session` | Switch input navigation to session history (default) |
| `/skip-permissions` | Show current `dangerously_skip_permissions` state |
| `/skip-permissions on\|off` | Enable / disable permission skip for this session (all agents) |
| `/agent` | Show active primary and escalation agents |
| `/agent list` | List all configured agents (`[P]` = primary, `[E]` = escalation) |
| `/agent switch <name> [as primary\|escalation]` | Switch agent role (prompts if args missing) |
| `/agent add` | Add a new agent backend interactively |
| `/colorize` | Show current transcript colorization mode |
| `/colorize off\|fenced\|balanced\|full` | Switch colorization mode live (`balanced` = default; `full` = experimental glamour) |
| `/think on\|off` | Show or hide reasoning/thinking tokens in transcript |
| `/new` | Start a fresh session |
| `/drop` | Delete current session and start fresh |
| `/list` | List sessions for the current directory |
| `/help` | Show categorised command reference |
| `/exit` | Quit |

**Tab completion:** `/` completes slash commands; `@` completes file paths from the current directory (e.g. `@src/main.go`). Tab cycles forward through matches entry by entry — within a command's variants first, then to the next command. Shift+Tab cycles backward. The hint panel below the transcript shows all matches with the active entry highlighted. Completion works on the token under the cursor, not just at end of line.

**Keyboard shortcuts:**

| Key | Action |
| --- | --- |
| `Ctrl+C` | Cancel running turn (if busy); clear input or exit (on empty) |
| `Ctrl+D` | Exit (on empty input) |
| `Ctrl+T` | Toggle thinking/reasoning token visibility |
| `Ctrl+R` / `Ctrl+S` | Reverse / forward incremental history search |
| `Up` / `Down` | Navigate input history (at first/last line); move cursor otherwise |
| `Ctrl+Up` / `Ctrl+Down` | Navigate input history (always) |
| `Shift+Left/Right/Up/Down` | Extend keyboard selection by character |
| `Ctrl+Shift+Left` / `Ctrl+Shift+Right` | Extend keyboard selection by word |
| `Tab` / `Shift+Tab` | Cycle tab completion forward / backward |
| `Ctrl+Z` / `Ctrl+Y` | Undo / redo in input area |
| `Ctrl+N` / `Shift+Alt+Enter` / `Alt+Enter` | Insert newline (multi-line input) |
| `PgUp` / `Ctrl+U` | Scroll transcript up |
| `PgDn` / `Ctrl+F` | Scroll transcript down |

All navigation, editing, history search, and undo/redo work while an agent turn is in progress. Only `Enter` (submit) is blocked until the turn completes.

**Input history** is persisted per session (`~/.milk/sessions/<id>.history`) and globally (`~/.milk/input_history`). Navigation defaults to session history; use `/history global` to switch.

### Single-prompt mode

```sh
milk [flags] <prompt>
```

### Flags

| Flag | Description |
| --- | --- |
| `--escalate` | Force this turn to the escalation agent |
| `--primary` | Force this turn to the primary agent |
| `--new` | Start a fresh session for the current directory |
| `--session <name>` | Resume or create a named session |
| `--list` | List sessions for the current directory |
| `--list --all` | List all sessions across all directories |
| `--drop` | Delete the current session |

### Examples

```sh
# Interactive session
milk

# Simple shell automation — routed to primary (local) model
milk "list all Go files modified in the last week"

# Force escalation agent for architecture decisions
milk --escalate "design a caching layer for this service"

# Named session for a specific feature
milk --session auth-refactor "what does the current middleware do?"

# Continue after escalation agent asks a follow-up
milk "yes, use Redis"

# Force back to primary agent
milk --primary "grep for TODO comments"

# Inspect all active sessions
milk --list --all
```

### Configuration

milk reads `~/.milk/config.json` on startup, falling back to defaults if absent.

```json
{
  "agent": "my-local",
  "agents": [
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
    },
    {
      "name": "claude",
      "provider": "claude-cli"
    }
  ],
  "escalation_agent": "claude",
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

`agent` names the active primary backend. `escalation_agent` names which backend handles escalated turns — any entry in `agents`, not necessarily the Claude CLI. Use `/agent switch <name> as primary|escalation` to change roles at runtime.

Supported `provider` values: omit (or `""`) for no-auth/local, `"bedrock"` (AWS SigV4), `"bearer"` (API key), `"claude-cli"` (Claude Code CLI subprocess). For Azure OpenAI, omit `provider` and pass `"api-key": "..."` under `headers`.

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

## Primary agent tools

The primary agent has access to these built-in tools:

| Tool | Description |
| --- | --- |
| `bash` | Run a shell command, returns stdout/stderr/exit code |
| `grep` | Search file contents by pattern |
| `find_files` | Find files by name pattern or glob |
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
| `escalate` | Hand off the current task to the escalation agent with a reason |

Side-effecting tools (`bash`, `write_file`, `edit_file`, `http_get`) require user approval on first use per project. Grants persist to `~/.milk/permissions/<project-hash>.json`. Use `/skip-permissions on` to bypass all prompts.

## Graceful degradation

| Primary agent | Escalation agent | Behaviour |
| --- | --- | --- |
| available | available | normal routing |
| unavailable | available | warns once, routes all turns to escalation agent |
| available | unavailable | warns once, stays primary-only |
| not configured | not configured | setup mode — splash shows `/agent add` guidance |

## Debugging

Two opt-in flags in `~/.milk/config.json` capture raw protocol streams to disk:

| Config key | Log file | Content | Use for |
| --- | --- | --- | --- |
| `"debug_claude_code": true` | `~/.milk/claude_debug.ndjson` | Every raw NDJSON line from the Claude CLI subprocess | Claude CLI protocol issues, unexpected event types, streaming gaps |
| `"debug_local": true` | `~/.milk/local_debug.log` | Every raw SSE line from the local agent HTTP stream (including skipped/unparsed lines) | Dropped tokens, unknown SSE events, parser mismatches |

Both flags default to `false`. The file extensions reflect content format: `.ndjson` is valid Newline-Delimited JSON (pipe through `jq -c`); `.log` is mixed SSE framing text with `data:` / `event:` prefixes and blank separators.

## Documentation

- [docs/setup.md](docs/setup.md) — full setup guide and local testing procedure
- [docs/spec.md](docs/spec.md) — full architecture and design spec
- [docs/providers.md](docs/providers.md) — provider configuration guides
- [docs/memory-design.md](docs/memory-design.md) — memory system design and phases
- [docs/observability-design.md](docs/observability-design.md) — OTel observability strategy
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch and commit conventions
