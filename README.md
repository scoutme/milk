# milk

![milk](docs/images/milk.png)

Switch models, not context.

milk is a terminal AI assistant that routes each prompt between a fast primary agent and a deep escalation agent — keeping the full conversation in sync across both. Start cheap. Go deep when you need it. Switch mid-workflow.

## What it does

- **Automatic routing** — each prompt is classified and sent to the right agent without you changing tools
- **Context handoff** — when escalation fires, the primary conversation is reformatted as context; the escalation agent orients itself without a separate setup step
- **Persistent memory** — a Percept store survives across sessions; key facts are reinforced, decay, and promote to long-term memory over time (NREM consolidation)
- **Built-in tools** — the primary agent has bash, file read/write/edit, grep, find, HTTP GET, session access, and memory tools without any extra configuration
- **Streaming TUI** — bubbletea terminal UI with a scrollable transcript, live memory panel, status bar, and input history
- **Aider and smolagents** — plug in aider-chat or smolagents as either the primary or escalation agent

## Backends

Both the primary and escalation roles support any of these backends — there is no backend tied exclusively to one role:

| Provider value | Backend |
| --- | --- |
| `"claude-cli"` | **Claude Code CLI** — runs `claude` as a subprocess; full tool access, session continuity, permission management |
| omit / `""` / `"local"` | Any OpenAI-compatible server (llama.cpp, Ollama, LM Studio, Azure OpenAI, …) |
| `"bedrock"` | AWS Bedrock — native Converse API, SigV4 signing, credential auto-refresh |
| `"aider-cli"` | aider — milk calls the `aider` binary directly, no adapter needed |
| `"subprocess"` | Generic NDJSON subprocess (milk-smolagent adapter, bundled automatically) |
| anything else | Bearer-token HTTP (OpenRouter, Groq, Together.ai, GitHub Models, …) |

> If no agent is configured, milk starts in setup mode. Use `/agent add` to configure a backend interactively.

## How routing works

Each prompt passes through a decision chain:

1. **Explicit flags** — `--escalate` or `--primary` override everything
2. **Session state** — if the escalation agent asked a follow-up, the next turn goes directly back to it
3. **Rules layer** — hard thresholds (token length, keywords) then a weighted signal scorer
4. **Primary model classifier** — the primary model decides `local` or `escalate` when the scorer is inconclusive
5. **Default** — local

When the primary model cannot handle a task, it calls `escalate(reason)` and milk reformats the conversation history as context for the escalation agent.

Once escalation fires, **auto-sticky** keeps subsequent turns on the escalation agent (shown as `<agent> (sticky)` in the status bar) — avoiding the "cold-start" penalty on each turn. Use `/primary` to return to the primary agent.

## Prerequisites

- Go 1.21+ (build from source only; pre-built binaries available)
- At least one configured agent backend (primary and/or escalation — each is optional; milk degrades gracefully if either is absent)
- `aider-chat` pip package — only if using the `aider-cli` provider
- `smolagents[litellm]` pip package — only if using the `subprocess`/smolagent provider

For a reference local setup (NVIDIA GPU, Ubuntu/WSL2, llama.cpp from source) see [docs/setup.md](docs/setup.md). For provider-specific configuration see [docs/providers.md](docs/providers.md).

## Installation

### Linux / macOS

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

Installs to `~/.local/bin/milk`. To install a specific version:

```sh
MILK_VERSION=v0.2.0 curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install.sh | sh
```

### Windows (PowerShell)

```powershell
irm https://raw.githubusercontent.com/scoutme/milk/main/install.ps1 | iex
```

Installs to `%LOCALAPPDATA%\milk\bin\milk.exe`. Native Windows support is partial — WSL2 is the recommended path. See [docs/setup.md](docs/setup.md#windows-and-wsl2).

### From source

```sh
go install github.com/scoutme/milk/cmd/milk@latest
```

Or with the install script (requires Go 1.21+ and Git):

```sh
curl -fsSL https://raw.githubusercontent.com/scoutme/milk/main/install-from-source.sh | sh
```

## Usage

### Interactive mode

```sh
milk
```

![milk TUI](docs/images/milk-tui-compressed.gif)

The TUI shows a scrollable transcript, a live memory panel on the right, a status bar with the active agent and routing state, and a multi-line input area at the bottom. All navigation, history search, and undo/redo work while a turn is in progress.

### Single-prompt mode

```sh
milk [flags] <prompt>
```

### Flags

| Flag | Description |
| --- | --- |
| `--escalate` | Force this turn to the escalation agent |
| `--primary` | Force this turn to the primary agent |
| `--new` | Start a fresh session |
| `--session <name>` | Resume or create a named session |
| `--list` | List sessions for the current directory |
| `--list --all` | List all sessions across all directories |
| `--drop` | Delete the current session |

### Examples

```sh
# Simple query — routed to primary (local) model
milk "list all Go files modified in the last week"

# Force escalation for a complex task
milk --escalate "design a caching layer for this service"

# Named session for a feature branch
milk --session auth-refactor "what does the current middleware do?"

# Continue after escalation agent asks a follow-up
milk "yes, use Redis"

# Force back to primary agent
milk --primary "grep for TODO comments"
```

## Configuration

milk reads `~/.milk/config.json` on startup. Run `/config init` in the TUI for an interactive wizard, or `/config open` to edit the file directly.

```json
{
  "agent": "my-local",
  "escalation_agent": "claude",
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
  "default_route": "local",
  "rules": {
    "escalate_above_tokens": 2000,
    "local_below_tokens": 30,
    "escalate_keywords": ["architect", "refactor entire", "design", "explain why", "analyze", "describe", "summarize"],
    "escalate_threshold": 6,
    "local_threshold": -4
  }
}
```

`agent` names the active primary backend. `escalation_agent` names the escalation backend — any entry in `agents`. Use `/agent switch <name> as primary|escalation` to change roles at runtime.

Full configuration reference, all provider options, and auth setup: [docs/providers.md](docs/providers.md).

## Slash commands

| Command | Description |
| --- | --- |
| `/escalate` | Pin all subsequent turns to escalation agent (until `/primary`) |
| `/escalate <msg>` | Force this single turn to escalation agent, then resume routing |
| `/primary` | Pin all subsequent turns to primary agent (until `/escalate`) |
| `/primary <msg>` | Force this single turn to primary agent, then resume routing |
| `/agent` | Show active primary and escalation agents |
| `/agent list` | List all configured agents |
| `/agent switch <name> [as primary\|escalation]` | Switch agent role |
| `/agent add` | Add a new agent backend interactively |
| `/learn <fact>` | Store a persistent memory |
| `/memory [pattern]` | List Percepts (global + session) |
| `/memory show <pat\|#id>` | Show full details of a Percept |
| `/forget <pat>` | Delete a Percept |
| `/panel memory` | Toggle the memory panel |
| `/export` | Print session transcript (colorized if terminal) |
| `/export json` | Print session as raw JSON |
| `/export <path>` | Write transcript to a file |
| `/need <goal>` | Set the current session goal |
| `/think on\|off` | Show or hide reasoning/thinking tokens |
| `/colorize off\|fenced\|balanced\|full` | Switch transcript colorization mode |
| `/skip-permissions on\|off` | Enable / disable tool permission prompts |
| `/usage` | Token usage report |
| `/metrics` | Latest observability metric values |
| `/otel` | OTel signal file sizes and record counts |
| `/otel trim` | Archive current OTel files and start fresh |
| `/history global\|session` | Switch input navigation history |
| `/config` | Print effective config JSON |
| `/config init` | Run interactive setup wizard |
| `/config open` | Open config file in `$EDITOR` |
| `/open <path>` | Open any file in the configured editor |
| `/setup telegram` | Configure Telegram remote oversight |
| `/update` | Check for new milk releases, show current vs latest version, prompt to download |
| `/new` | Start a fresh session |
| `/drop` | Delete current session and start fresh |
| `/list` | List sessions for the current directory |
| `/help` | Show categorised command reference |
| `/exit` | Quit |

**Tab completion:** `/` completes slash commands; `@` completes file paths from the current directory. Tab cycles forward through matches; Shift+Tab cycles backward. The hint panel shows all matches with the active entry highlighted.

## Keyboard shortcuts

| Key | Action |
| --- | --- |
| `Ctrl+C` | Cancel running turn (if busy); clear input or exit (on empty) |
| `Ctrl+D` | Exit (on empty input) |
| `Ctrl+T` | Toggle thinking/reasoning token visibility |
| `Ctrl+R` / `Ctrl+S` | Reverse / forward incremental history search |
| `Up` / `Down` | Navigate input history (at first/last line); move cursor otherwise |
| `Ctrl+Up` / `Ctrl+Down` | Navigate input history (always) |
| `Shift+Left/Right/Up/Down` | Extend selection by character |
| `Ctrl+Shift+Left` / `Ctrl+Shift+Right` | Extend selection by word |
| `Tab` / `Shift+Tab` | Cycle tab completion forward / backward |
| `Ctrl+Z` / `Ctrl+Y` | Undo / redo in input area |
| `Ctrl+N` / `Shift+Alt+Enter` / `Alt+Enter` | Insert newline |
| `PgUp` / `Ctrl+U` | Scroll transcript up |
| `PgDn` / `Ctrl+F` | Scroll transcript down |

## Memory

milk maintains a persistent Percept store at `~/.milk/memory/`. Percepts are atomic natural-language assertions with a confidence weight. At session end, NREM consolidation runs: weights decay, low-weight Percepts are pruned, and high-weight ones are promoted to the global store. The memory panel (`/panel memory`) shows SESSION / GLOBAL / GLOBAL(core) sections in real time.

## Primary agent tools

The primary agent has access to these built-in tools (no extra configuration needed):

| Tool | Description |
| --- | --- |
| `bash` | Run a shell command |
| `grep` | Search file contents by pattern |
| `find_files` | Find files by name or glob |
| `read_file` | Read a file with optional offset and limit |
| `write_file` | Write content to a file |
| `edit_file` | Exact-string replacement within a file |
| `list_dir` | List directory contents |
| `http_get` | Fetch a URL |
| `get_session_context` | Read shared session history |
| `record_memory` | Store a Percept |
| `get_memory` | Retrieve Percepts by keyword |
| `list_memory` | List all Percepts |
| `export_session` | Export the session transcript |
| `get_metrics` | Show observability metrics |
| `search_signals` | Search OTel signal files |
| `escalate` | Hand off to the escalation agent |

Side-effecting tools (`bash`, `write_file`, `edit_file`, `http_get`) require user approval on first use per project. Grants persist to `~/.milk/permissions/<project-hash>.json`. Use `/skip-permissions on` to bypass prompts.

## Observability

OTel signal files are written to `~/.milk/otel/`:

| File | Contents |
| --- | --- |
| `logs.jsonl` | Structured event logs (Percept records, consolidation runs, recalls) |
| `traces.jsonl` | Span traces per memory operation |
| `metrics.jsonl` | Counters and gauges |

Use `/otel` to inspect sizes, `/otel trim` to archive and reset, and `/metrics` to see current values inline.

## Graceful degradation

| Primary agent | Escalation agent | Behaviour |
| --- | --- | --- |
| available | available | normal routing |
| unavailable | available | warns once, routes all turns to escalation agent |
| available | unavailable | warns once, stays primary-only |
| not configured | not configured | setup mode — `/agent add` guidance shown |

## Debugging

Two opt-in flags in `~/.milk/config.json` capture raw protocol streams to disk:

| Config key | Log file | Content |
| --- | --- | --- |
| `"debug_claude_code": true` | `~/.milk/claude_debug.ndjson` | Raw NDJSON from the Claude CLI subprocess |
| `"debug_local": true` | `~/.milk/local_debug.log` | Raw SSE lines from the local agent HTTP stream |

## Documentation

- [docs/setup.md](docs/setup.md) — full setup guide and local testing procedure
- [docs/providers.md](docs/providers.md) — all provider configuration guides
- [docs/spec.md](docs/spec.md) — full architecture and design spec
- [docs/memory-design.md](docs/memory-design.md) — memory system design
- [docs/observability-design.md](docs/observability-design.md) — OTel observability strategy
- [docs/adr/README.md](docs/adr/README.md) — architecture decision records
- [docs/branching-strategy.md](docs/branching-strategy.md) — branch and commit conventions
