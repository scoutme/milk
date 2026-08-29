# milk — specification

## Overview

milk lets you switch between a primary inference agent and a configurable escalation agent mid-workflow, maintaining full session context across the switch. Any backend can serve either role — see [docs/providers.md](providers.md) for the full catalog and the one routing constraint milk expects you to honor (escalation should be smarter/pricier than primary).

The primary use case is code assistance and shell automation for a single user.

This page is the architecture/CLI reference. For task-oriented docs, start at [docs/getting-started.md](getting-started.md); see also [docs/workflows.md](workflows.md), [docs/tooling.md](tooling.md), [docs/operations.md](operations.md), and [docs/eval.md](eval.md).

---

## Architecture

### Components

```
milk [prompt | flags]
       │
       ▼
┌─────────────────────────────────────────────────────┐
│  Session State Machine                              │
│  ROUTING → LOCAL | ESCALATION | ESCALATION_WAITING  │
└──────────┬──────────────────────────────────────────┘
           │
           ▼
┌──────────────────────────┐
│  Router                  │
│  1. Explicit flags       │  --escalate, --primary
│  2. Session state check  │  ESCALATION_WAITING → bypass
│  3. Rules layer          │  heuristics + weighted scorer
│  4. Primary model        │  primary model self-classification
│  5. Default: try local   │
└────────┬─────────────────┘
         │
    ┌────┴──────────┐
    ▼               ▼
PRIMARY              ESCALATION
agent                agent
(any AgentConfig)    (any AgentConfig)
local / bedrock /    local / bedrock /
claude-cli / …       claude-cli / …
```

Routing mechanics, session states, and the native `/workflow` engine are covered in [docs/workflows.md](workflows.md).

---

## Project layout

```
cmd/milk/
  main.go           Cobra root command (single-prompt mode); builds TurnRunner instances
  repl.go           bubbletea TUI — transcript viewport, textarea, status bar, /agent switch
  runner.go         TurnRunner interface + three implementations (localRunner, cliRunner, subprocessRunner)
  dispatch.go       runPrimary / runEscalation — role-specific session bookkeeping shared by single-shot and TUI
  interactive.go    slash commands, tab completion, prompt label helpers
  ansi.go           ANSI colour helpers and activity spinner
  panel_memory.go   right-side memory panel (/panel memory)
  panel_workflow.go workflow progress panel (/panel workflow)
  workflow_cmd.go   /workflow slash command + interactive wizard
  workflow_runner.go workflowTurnRunner adapter (wraps TurnRunner with per-role session tracking)

internal/
  workflow/         workflow engine: Workflow interface, TurnRunner adapter interface, State/verdict/turn
  workflow/dev/     dev workflow: designer→generator→evaluator loop, sprint counting, prompt builders
  config/           config loading and defaults (~/.milk/config.json)
  session/          session state machine + JSON store (~/.milk/sessions/)
  router/           routing: rules layer → weighted scorer → primary-model classifier
  agent/local/      OpenAI-compat HTTP client; Bedrock Converse native path; SigV4/Bearer/token_cmd
                    auth transports; tool loop; streaming tool-format FSM detector
  agent/claude/     claude CLI subprocess driver; stream-json parser; permission-prompt protocol
  agent/subprocess/ generic subprocess agent: NDJSON stream protocol, tag interception
                    (<milk:need:>, <milk:percept:>, <milk:escalate:>)
  agent/aider/      aider-cli provider (wraps subprocess agent)
  agent/smolagent/  subprocess provider (wraps subprocess agent)
  escalation/       context builders: static instruction block + dynamic summary sent to escalation agent
  memory/           Percept store; NREM decay/prune/promote consolidation (~/.milk/memory/)
  obs/              OpenTelemetry file exporters (~/.milk/otel/)
  claudesettings/   ~/.claude/settings.json reader (allowed tools, directories, AWS refresh command)
  oversight/        remote oversight interface (Telegram notifier)
  tags/             milk tag constants (<milk:need:>, <milk:percept:>, <milk:escalate:>)
```

### Agent dispatch layers

```
TurnRunner.Execute()       provider-specific inference (one of three implementations)
       │
runPrimary / runEscalation role-specific session bookkeeping (dispatch.go)
       │
run() / runTurn()          single-shot or TUI entry point; builds runners, drives router
```

Any agent can be exposed as a callable tool to any other agent (Agent-as-Tool) — see [docs/tooling.md](tooling.md#agent-as-tool).

---

## Claude CLI escalation mechanics

Implementation detail for the `claude-cli` provider specifically — see [docs/providers.md — Claude Code CLI](providers.md#claude-code-cli) for configuration, and [docs/workflows.md — Context handoff](workflows.md#context-handoff) for the cross-provider handoff format.

- **AWS credential injection**: when `aws_auth_refresh: true`, milk reads the `awsAuthRefresh` command from `~/.claude/settings.json`, runs it before each turn for fresh STS credentials, and injects them as explicit `AWS_*` env vars into the subprocess. Conflicting inherited vars (`AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_DEFAULT_*_MODEL`, `AWS_PROFILE`, etc.) are stripped to prevent wrong-account overrides. See ADR 23.
- **First escalation turn**:
  ```
  claude --print --output-format stream-json \
         --session-id <new-uuid> \
         --append-system-prompt-file <static-ctx> \
         --append-system-prompt-file <dynamic-ctx> \
         -- "<user prompt>"
  ```
- **Subsequent turns** (`--resume`):
  ```
  claude --print --output-format stream-json \
         --resume <escalation-session-id> \
         --append-system-prompt-file <dynamic-ctx-if-changed> \
         -- "<user prompt>"
  ```
- `session_id` is extracted from the first NDJSON message and persisted to the milk session file.

### Permission prompt flow

milk passes `--permission-prompt-tool stdio` on every invocation. When Claude wants to use a tool that isn't pre-approved, it emits a `control_request` NDJSON event and pauses; milk intercepts it and, in TUI mode, routes a blocking prompt through the bubbletea message queue (ADR-0015):

1. The agent goroutine calls `tuiInputReader.readLine(prompt)` and blocks on a channel.
2. The TUI appends the prompt to the transcript and switches key events to `handlePermKey`.
3. The user types `y`/`n` and Enter; Ctrl-C sends `n`.
4. milk writes a `control_response` JSON to Claude's stdin; the agent goroutine unblocks.

The prompt shows the tool name, key arguments, and — for `workingDir` blocks — the restricted path. The session stays alive throughout; no `--resume` round-trip needed.

`dangerously_skip_permissions` bypasses this flow entirely (auto-approve all); `/skip-permissions on|off` overrides it per session without restarting. `allowed_tools`/`add_dirs` pre-approve specific tools/directories without ever prompting.

#### Pre-flight "Stream closed" failures

A second failure class sits *before* the permission-prompt handler: Claude Code's directory-trust pre-flight check. When it fires, the tool returns `"Stream closed"` — no `control_request` event, no prompt, the turn ends silently with partial output. milk handles this in two layers:

1. **Baseline pre-approval**: `Bash`, `Read`, `Write`, `Edit` are always merged into `--allowedTools` on every invocation; `/tmp` is always added to the trusted directory list alongside `cwd`. Eliminates the failure for most fresh-workspace first turns.
2. **Post-turn detection and retry**: milk's stream parser tracks `type:"user"` NDJSON messages; a `tool_result` block whose content is `"Stream closed"` is correlated back to the tool name via a per-turn registry and recorded. After the turn, milk presents each failed tool, offers a tool/directory grant, persists it to `~/.claude/settings.json`, and retries via `--resume`. See ADR-0035.

---

## CLI Interface

### Usage

```
milk                          # interactive REPL mode
milk [flags] <prompt>         # single-prompt mode
```

### Interactive mode

`milk` with no prompt argument starts a REPL built on charmbracelet/bubbletea. The input prompt uses `❯` as the prefix. The status bar reflects the current routing state and active agent.

**Slash commands:** `/escalate`, `/primary`, `/new`, `/drop`, `/list`, `/paste`, `/skip-permissions`, `/agent`, `/colorize`, `/think`, `/need`, `/workflow`, `/config`, `/open`, `/update`, `/help`, `/exit`

**Memory commands:** `/learn <statement>`, `/memory [global|session|<pattern>]`, `/memory show <pattern or #id>`, `/forget <pattern or #id>`, `/export [json|<path>]` — see [docs/operations.md — Memory](operations.md#memory).

**Panel commands:** `/panel memory`, `/panel workflow`, `/panel tasks` — see [docs/operations.md](operations.md) and [docs/workflows.md](workflows.md#the-native-workflow-engine).

**/skip-permissions** toggles `dangerously_skip_permissions` for the current session: `on` makes the escalation agent auto-approve all tool uses; `off` (default) re-enables per-tool prompting. Alone, shows current state. A red warning banner appears at startup if already on via config.

**/agent** manages agent backends at runtime:

| Subcommand | Action |
|---|---|
| `/agent` | Show active primary and escalation backends |
| `/agent list` | List all configured backends; active marked with `*` |
| `/agent switch <name> as primary\|escalation` | Switch role to the named backend (prompts if args missing) |
| `/agent add` | Add a backend via interactive wizard |
| `/agent add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…]` | Add inline |
| `/agent tool list [<agent>\|global]` | List tool-agents (effective merged for primary by default) |
| `/agent tool enable\|disable <tool> [for <agent>\|global]` | Enable/disable a tool-agent entry |
| `/agent tool add <tool> description=<desc> [for <agent>\|global]` | Add a new tool-agent entry |
| `/agent tool remove <tool> [for <agent>\|global]` | Remove a tool-agent entry |

New backends are appended to `agents` in `~/.milk/config.json` immediately; `/agent switch` assigns a role in the current session.

**/colorize** controls transcript syntax and Markdown rendering:

| Subcommand | Action |
|---|---|
| `/colorize` | Show current mode |
| `/colorize off` | Disable all colorization |
| `/colorize fenced` | Highlight fenced code blocks only |
| `/colorize balanced` | Fenced blocks + inline Markdown (default) |
| `/colorize full` | Full glamour Markdown render — experimental |

Persisted to config immediately, effective on next render.

**/think** controls reasoning/thinking token visibility: `/think` (show current), `/think on` (show inline), `/think off` (show `[thinking…]` placeholder). Retroactive — both transcript variants are maintained in parallel during streaming, so toggling is instant. Default via `show_reasoning` in config (default `true`). Applies to both primary-model `<think>` blocks and Claude extended thinking.

**/need** sets the current session goal (`/need <one-sentence goal>`); the primary agent calls this automatically when the user states a new objective. Shown in the memory panel and injected into escalation context.

**/workflow** runs a named multi-agent pipeline — see [docs/workflows.md — The native `/workflow` engine](workflows.md#the-native-workflow-engine).

**/config** manages configuration:

| Subcommand | Action |
|---|---|
| `/config` | Print current config JSON in the transcript |
| `/config init` | Run the interactive setup wizard |
| `/config open` | Open `~/.milk/config.json` in the configured editor |

Same commands on the CLI as `milk config`, `milk config init`, `milk config open`. Editor selection uses `config_editors` (see below).

**`milk otel`** manages observability settings — see [docs/operations.md — Observability](operations.md#observability).

**/open** opens any file in the configured editor: `/open <path>` (or `/open @<path>`, `@` stripped automatically). The agent can also open files via the `open_file` tool.

**/update** checks GitHub for newer milk releases, compares versions, and (with confirmation) downloads and installs the appropriate binary for the current platform.

**Multi-line input**: Shift+Enter/Alt+Enter inserts a newline; Enter submits. Bracketed paste is handled transparently.

**Clipboard binary paste**: when a bracketed paste yields empty text, milk probes the system clipboard (`xclip`/`wl-paste`) for non-text content (e.g. `image/png`); found content is saved to a temp file and staged as a pending attachment, same as `/attach`. No special key combination beyond a normal paste gesture.

**Keyboard**: Up/Down navigates input history (single-line mode); Ctrl-C clears a pending force-mode flag or exits; Ctrl-D exits.

### Flags

| Flag | Description |
|---|---|
| `--escalate` | Force route to escalation agent for this turn |
| `--primary` | Force route to primary agent for this turn; breaks ESCALATION_WAITING state |
| `--new` | Start a new session (old sessions for cwd untouched) |
| `--session <name>` | Target session by name (resume or create) |
| `--continue` | Alias for default resume behavior (explicit) |
| `--list` | List sessions for current cwd |
| `--list --all` | List all sessions across all directories |
| `--drop` | Delete current session |

---

## Configuration

`~/.milk/config.json` — see [docs/providers.md](providers.md) for the full `agents`/provider field reference, [docs/workflows.md](workflows.md#customizing-the-rules-block) for the `rules` block, and [docs/tooling.md](tooling.md) for `agent_tools`/`mcp_servers`. This section covers the remaining root-level behavior fields.

```json
{
  "agent": "local",
  "agents": [ { "name": "local", "url": "http://localhost:8080", "model": "qwen2.5-coder", "provider": "local" } ],
  "escalation_agent": "claude",
  "default_route": "local",
  "colorization": "balanced",
  "show_reasoning": true,
  "sticky_escalation": true,
  "experimental_lazy_history_management": false,
  "aws_auth_refresh": false
}
```

`agent` names the active primary backend; empty defaults to the first non-`claude-cli` entry. `escalation_agent` selects which entry handles escalated turns (default `"claude"`) — a built-in `claude-cli` entry named `"claude"` is always available even if unlisted.

### `colorization` field

See [/colorize](#interactive-mode) above for the four modes and their behavior.

### `show_reasoning` field

Default visibility of thinking/reasoning tokens; see [/think](#interactive-mode) above.

### `config_editors` field

Ordered list of editor commands tried by `/config open` and `/open`; first found on `$PATH` wins. Env vars (`$EDITOR`, `$VISUAL`) are expanded before lookup. Default: `["$EDITOR", "$VISUAL", "nano", "vim", "vi"]`.

```json
"config_editors": ["code --wait", "$EDITOR", "nano"]
```

### `experimental_lazy_history_management` field

When `true`, agent context is built lazily: only the current agent's own turns and user turns directed at it are included verbatim; contiguous blocks of other-agent turns collapse into a placeholder (e.g. `[escalation]: (14 turns omitted)`), retrievable on-demand via `get_session_context`. Significantly reduces context size for sessions with heavy cross-agent activity. User turns always show `[user to <agent>]` labels regardless. Default: `false`.

---

## Streaming

Both agents stream output in real time: SSE from OpenAI-compat APIs (`stream: true`), AWS binary event-stream from Bedrock Converse, or NDJSON from Claude's `--output-format stream-json`. milk relays tokens to stdout as they arrive.

---

## Backlog

- Planning mode (offline, no LLM execution)
- Demotion from escalation back to primary mid-session
- Web UI / TUI
- MCP stdio transport for local tools ✓ (done)
- Multi-user / daemon mode
