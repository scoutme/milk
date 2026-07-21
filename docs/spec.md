# milk — specification

## Overview

milk lets you switch between a primary inference agent and a configurable escalation agent (Claude Code CLI or another inference backend) mid-workflow, maintaining full session context across the switch. The primary agent supports OpenAI-compatible servers (local or remote) and AWS Bedrock natively.

The primary use case is code assistance and shell automation for a single user.

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
PRIMARY         ESCALATION
agent           agent
OpenAI API      (any AgentConfig)
tool loop       claude-cli / bedrock / …
```

### Session state machine

```
States: ROUTING | PRIMARY | ESCALATION | ESCALATION_WAITING

ROUTING              → rules + primary model decision → PRIMARY or ESCALATION
PRIMARY            → --escalate OR primary model escalate() → ESCALATION
ESCALATION         → escalation agent ends turn with question → ESCALATION_WAITING
ESCALATION_WAITING → next user input bypasses router → direct --resume to escalation agent
ESCALATION_WAITING → user --primary flag → back to ROUTING
```

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

### Agent-as-Tool

Any agent in the `agents` list can be exposed as a callable tool to any other agent via the `agent_tools` global list (or per-agent `tools` overrides). When enabled, milk synthesises an OpenAI function-schema for each peer agent and injects it alongside the built-in tools; the primary agent can invoke a peer by name as it would any other tool call. The peer agent receives the caller's prompt and returns a text result that is fed back as a tool result, with no session state shared between peer calls. Configure tool-agents with the `agent_tools` config field or at runtime with `/agent tool`.

---

## Router

Decision order per turn:

1. **Explicit flags** — `--escalate` forces escalation agent; `--primary` forces primary (always wins)
2. **Session state** — if `ESCALATION_WAITING`, bypass router, send directly to escalation agent `--resume`
3. **Rules layer** — layered scorer:
   - Hard rules: token length above `escalate_above_tokens` → escalation; keyword match → escalation
   - Short-prompt shortcut: ≤ `local_below_tokens` tokens → conclusive local
   - Weighted signal scorer: local verbs, escalate verbs, path references, code blocks, open-question prefixes each contribute a signed score; conclusive if score reaches `escalate_threshold` or `local_threshold`; all lists are configurable (see `rules` field)
4. **Primary model classification** — when scorer is inconclusive, ask the primary model with minimal prompt, expect `route: local | escalate`; behaviour configurable via `classifier_fallback`
5. **Default** — attempt primary; escalate if primary returns `escalate(reason)`

The classifier uses the same model instance as the primary agent. No second model or second inference server instance.

---

## Primary Agent

- Backend: configurable via `agents` (the primary agent must be an inference-server backend) in `~/.milk/config.json`; multiple named backends can coexist and be switched at runtime with `/agent switch <name> as primary`
- Protocols: OpenAI-compatible Chat Completions API (llama.cpp, Ollama, LM Studio, vLLM, OpenRouter, Together.ai, Groq, Azure OpenAI) **or** AWS Bedrock Converse API natively (binary event-stream, SigV4 signing — not OpenAI-compat)
- Model: any tool-calling-capable model. Tested: Qwen2.5-Coder 7B/3B, Gemma 4 E4B, Claude Haiku (via Bedrock).

### Remote inference / authentication

The `provider` field in an `agents` entry selects the backend type and auth transport:

| `provider` | Backend | Auth mechanism | Required fields |
|---|---|---|---|
| `""` / `"local"` | OpenAI-compat HTTP | None (plain HTTP) | `url`, `model` |
| `"bedrock"` | AWS Bedrock Converse | AWS SigV4 | `url`, `model`, `aws_region` + credentials |
| `"claude-cli"` | Claude Code CLI subprocess | n/a | `bin` (optional, default `"claude"`) |
| any other string | OpenAI-compat HTTP | `Authorization: Bearer <api_key>` | `url`, `model`, `api_key` |

Extra headers for any provider (e.g. OpenRouter's `HTTP-Referer`) can be injected via `headers`.

**Dynamic tokens (`token_cmd`)**: set `token_cmd` to a shell command whose stdout is the Bearer token. milk runs it at startup and retries it automatically on 401/403. Takes precedence over `api_key`. Example: `"gh auth token --hostname myorg.ghe.com"`.

**Custom inference path (`chat_path`)**: override the endpoint path when the server does not follow the `/v1` prefix convention. Example: `"chat_path": "/chat/completions"`.

**AWS Bedrock credential resolution** (in order): explicit `aws_key_id` / `aws_secret` / `aws_token` config fields → `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` / `AWS_SESSION_TOKEN` env vars → region parsed from `url` when `aws_region` is empty.

**Automatic credential renewal (`aws_refresh_cmd`)**: set `aws_refresh_cmd` to a `credential_process`-compatible shell command (e.g. `aws sts get-session-token --duration-seconds 3600`). When a Bedrock request returns 403, the SigV4 transport runs the command, parses its `AccessKeyId` / `SecretAccessKey` / `SessionToken` JSON output, swaps the credentials atomically, and retries the request once — no agent restart needed. In TUI mode, the status bar shows `[refreshing AWS credentials…]` while the renewal is in flight, then `[AWS creds: ok]` or `[AWS creds failed: <error>]` on completion.

**TLS overrides**: `tls_skip_verify: true` disables cert verification (dev/self-signed only); `tls_ca_cert: "/path/to/ca.pem"` trusts a private CA.

**Azure OpenAI workaround**: Azure uses a non-standard URL path and an `api-key` header rather than Bearer auth. Set `url` to the full deployment endpoint, add `{"api-key": "<key>"}` to `headers`, and leave `provider` empty. A dedicated Azure provider with URL templating is tracked in GitHub Issues. See ADR 27.
- Tool loop: standard agentic loop — call → check tool calls → execute → feed result → repeat until final answer
- Built-in tools (implemented in Go, exposed as OpenAI function schemas):
  - `bash(command string) → stdout, stderr, exit_code`
  - `grep(pattern string, path string, recursive bool) → matches`
  - `read_file(path string, offset int, limit int) → content`
  - `write_file(path string, content string) → ok` — creates parent directories; expands `~`
  - `edit_file(path string, old_string string, new_string string, replace_all bool) → ok` — exact-string replacement; rejects ambiguous matches unless `replace_all=true`; expands `~`
  - `delete_file(path string) → ok` — removes a file from disk; permission-gated
  - `move_file(source string, destination string) → ok` — renames/relocates a file; creates destination parent directories; permission-gated
  - `list_dir(path string) → entries` — names, types, sizes; expands `~`
  - `http_get(url string, max_bytes int) → body` — bounded HTTP GET
  - `http_request(method string, url string, headers object, body string, max_bytes int) → body, status` — generic HTTP request; permission-gated
  - `get_session_context() → history` — returns the full shared session history (both agents) so the primary model can see prior escalation turns
  - `get_context_stats() → stats` — returns current history turn counts and total character size so the agent can self-regulate before hitting context limits
  - `open_file(path string) → ok` — opens a file in the configured editor (same resolution as `/config open` and `/open`); useful when the user asks the agent to open a file for review
- Self-escalation: primary model may return `escalate(reason string)` as a tool call to trigger promotion
- Role-aware system prompt: primary agent sees the `escalate` tool and is told to use it for tasks beyond its capabilities; escalation agent does not see the `escalate` tool and is told it is the escalation target

---

## Escalation Agent

The escalation agent is any entry in `agents` whose name matches `escalation_agent` in the config. It defaults to the built-in `claude-cli` entry (named `"claude"`). It can be:

- **Claude Code CLI** (`provider: "claude-cli"`): `claude --print --output-format stream-json`
- **Any inference-server backend**: same OpenAI-compat or Bedrock path as the primary agent, but with a role-aware system prompt (no `escalate` tool, knows it is the escalation target)

### Claude CLI escalation

- **AWS credential injection**: when `aws_auth_refresh: true` in `~/.milk/config.json`, milk reads the `awsAuthRefresh` command from `~/.claude/settings.json`, runs it before each turn to obtain fresh STS credentials, and injects them as explicit `AWS_*` env vars into the subprocess. Conflicting vars (`AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_DEFAULT_*_MODEL`, `AWS_PROFILE`, etc.) are stripped from the inherited environment to prevent wrong-account overrides. See ADR 23.
- Context is split across two `--append-system-prompt-file` flags to preserve Claude's prompt cache:
  - **Static file** (`BuildStaticContext`): per-session stable nonce tags (`NeedInstruction`, `MemoryInstruction`), remembered percepts. Byte-identical across turns → cache hit.
  - **Dynamic file** (`BuildDynamicContext`): identity block, escalation brief, current need, `LastLocalSummary`. Changes per turn; suppressed when content is unchanged.
- First escalation turn:
  ```
  claude --print --output-format stream-json \
         --session-id <new-uuid> \
         --append-system-prompt-file <static-ctx> \
         --append-system-prompt-file <dynamic-ctx> \
         -- "<user prompt>"
  ```
- Subsequent turns in same escalation (`ContextModeResume`):
  ```
  claude --print --output-format stream-json \
         --resume <escalation-session-id> \
         --append-system-prompt-file <dynamic-ctx-if-changed> \
         -- "<user prompt>"
  ```
- `session_id` is extracted from the first NDJSON message and persisted to the milk session file
- The escalation agent orients itself from the appended context — no separate reformulation step

### Permission prompt flow

milk passes `--permission-prompt-tool stdio` on every Claude CLI invocation. When Claude wants to use a tool that has not been pre-approved, it emits a `control_request` NDJSON event on stdout and pauses. milk intercepts this event and, in TUI mode, routes a blocking prompt through the bubbletea message queue (see ADR-0015):

1. The agent goroutine calls `tuiInputReader.readLine(prompt)` and blocks on a channel.
2. The TUI appends the prompt to the transcript and switches key events to `handlePermKey`.
3. The user types `y` (allow) or `n` (deny) and presses Enter; Ctrl-C sends `n`.
4. milk writes a `control_response` JSON to Claude's stdin; the agent goroutine unblocks.

The prompt shows the tool name, key arguments, and — for `workingDir` blocks — the restricted path. The session stays alive throughout; no `--resume` round-trip is needed.

`dangerously_skip_permissions` (field on the `claude-cli` AgentConfig entry) bypasses this flow entirely: Claude auto-approves all tool uses. `/skip-permissions on|off` overrides this setting per session without restarting.

Pre-approved tools and directories can be listed in `allowed_tools` and `add_dirs` fields on the `claude-cli` entry; they are passed as `--allowedTools` / `--add-dir` flags and never trigger a prompt.

#### Pre-flight "Stream closed" failures

A second class of permission failure sits *before* the `--permission-prompt-tool stdio` handler: Claude Code's directory-trust pre-flight check. When this fires, the tool returns `"Stream closed"` as its result — no `control_request` event is emitted, no prompt reaches the user, and the turn ends silently with partial output.

milk handles this with two layers:

1. **Baseline tool pre-approval**: `Bash`, `Read`, `Write`, and `Edit` are always merged into the `--allowedTools` flag on every `claude-cli` invocation (in addition to any user-configured `allowed_tools`). `/tmp` is always added to the trusted directory list alongside `cwd`. This eliminates the failure for the majority of fresh-workspace first turns with no user interaction.

2. **Post-turn detection and retry**: milk's stream parser tracks `type:"user"` NDJSON messages. A `tool_result` block whose content is `"Stream closed"` is correlated back to the tool name via a per-turn `toolRegistry` (built from `content_block_start` `id` fields during streaming), and recorded in `ParseResult.StreamClosedDenials`. After the turn, `handleStreamClosedDenials` presents each failed tool to the user, offers a tool grant and a directory grant, persists grants to `~/.claude/settings.json`, and retries the turn via `--resume`. See ADR-0035.

### Context handoff (escalation)

When promoting from the primary agent to the escalation agent, milk formats the local conversation history as a plain transcript and passes it via `--append-system-prompt-file` (for Claude CLI, split into static+dynamic files — see ADR-0004) or as the first system message (for inference-server escalation). Format:

```
[Context from primary agent session]
User: <turn>
Assistant: <turn>
[Tool: bash] <command>
[Tool result] <output>
...
User: <final prompt that triggered escalation>
```

---

## Session Model

### Storage layout

```
~/.milk/
├── config.json
└── sessions/
    ├── index.json          # cwd → [{id, name, last_used}] sorted by last_used desc
    └── <uuid>.json         # full session data
```

### Session file schema

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "optional-user-name",
  "cwd": "/absolute/path/to/project",
  "created_at": "2026-05-05T10:00:00Z",
  "last_used": "2026-05-05T11:32:00Z",
  "state": "ESCALATION_WAITING",
  "escalation_session_id": "abc123",
  "escalation_nonce": "x7k2mq",
  "history": [
    {
      "role": "user | assistant | tool_result",
      "agent": "local | escalation",
      "content": "...",
      "thinking": "...",
      "tool_calls": [],
      "timestamp": "2026-05-05T10:01:00Z"
    }
  ]
}
```

### Session index file schema

```json
{
  "/absolute/path/to/project": [
    {"id": "uuid", "name": "refactor-auth", "last_used": "2026-05-05T11:32:00Z"},
    {"id": "uuid", "name": "", "last_used": "2026-05-05T10:00:00Z"}
  ]
}
```

### Session lookup on invocation

1. `milk <prompt>` → most recent session for cwd → resume (or create new if none)
2. `milk --session refactor-auth <prompt>` → find by name within cwd → resume or create
3. `milk --new <prompt>` → always create fresh session
4. `milk --session refactor-auth --new <prompt>` → create new named session

Names are cwd-scoped. Same name can exist in different projects.

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

**Memory commands:** `/learn <statement>`, `/memory [global|session|<pattern>]`, `/memory show <pattern or #id>`, `/forget <pattern or #id>`, `/export [json|<path>]`

The `#id` form in `/forget` and `/memory show` accepts a short hex prefix (4–64 chars). The `#` prefix is optional — bare hex like `a1b2c3d4` also works. The local agent can also delete percepts directly via the `forget_memory` tool (same short-ID resolution, same `#` handling).

**Panel commands:** `/panel memory` — toggle the right-side memory panel (open by default); `/panel workflow` — toggle the workflow progress panel (auto-opens when a workflow starts)

**/skip-permissions** toggles `dangerously_skip_permissions` for the current session: `on` makes the escalation agent auto-approve all tool uses without prompting; `off` (default) re-enables the per-tool permission flow. The current state is shown with `/skip-permissions` alone. A red warning banner is printed at startup if the flag is already on via config.

**/agent** manages agent backends at runtime:

| Subcommand | Action |
|---|---|
| `/agent` | Show active primary and escalation backends |
| `/agent list` | List all configured backends; active marked with `*` |
| `/agent switch <name> as primary\|escalation` | Switch role to the named backend (prompts if args missing) |
| `/agent add` | Add a backend via interactive wizard (prompts for each field) |
| `/agent add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…]` | Add inline |
| `/agent tool list [<agent>\|global]` | List tool-agents (effective merged for primary by default) |
| `/agent tool enable <tool> [for <agent>\|global]` | Enable a tool-agent entry |
| `/agent tool disable <tool> [for <agent>\|global]` | Disable a tool-agent entry |
| `/agent tool add <tool> description=<desc> [for <agent>\|global]` | Add a new tool-agent entry |
| `/agent tool remove <tool> [for <agent>\|global]` | Remove a tool-agent entry |

New backends are appended to `agents` in `~/.milk/config.json` immediately. Use `/agent switch` to assign a role to a newly added backend in the current session.

**/colorize** controls transcript syntax and Markdown rendering:

| Subcommand | Action |
|---|---|
| `/colorize` | Show current mode |
| `/colorize off` | Disable all colorization |
| `/colorize fenced` | Highlight fenced code blocks only (chroma) |
| `/colorize balanced` | Fenced blocks + inline Markdown (bold, headings, bullets, inline code) |
| `/colorize full` | Full glamour Markdown render — experimental |

The mode is persisted to `~/.milk/config.json` immediately and takes effect on the next render (no restart needed). Default is `balanced`.

**/think** controls reasoning/thinking token visibility:

| Subcommand | Action |
|---|---|
| `/think` | Show current reasoning visibility (on/off) |
| `/think on` | Show thinking/reasoning tokens inline in the transcript |
| `/think off` | Hide thinking tokens; a `[thinking…]` placeholder is shown instead |

The toggle is retroactive — both transcript variants (full and no-think) are maintained in parallel during streaming, so switching is instantaneous with no rebuild. The default is configurable via `show_reasoning` in `~/.milk/config.json` (default: `true`). Applies to both primary model `<think>` blocks and Claude extended thinking tokens.

**/need** sets the current goal for the session. The primary agent is instructed to call this tool automatically when the user states a new objective:

```
/need <one-sentence goal>
```

The goal is shown in the memory panel and injected into escalation context so the escalation agent knows what is being worked on.

**/workflow** runs a named multi-agent pipeline:

```
/workflow                                         # list available workflows
/workflow dev <task>                              # run dev workflow (wizard for missing args)
/workflow dev <task> --designer <agent> \
              --generator <agent> \
              --evaluator <agent>                 # inline agent assignment
/workflow resume                                  # resume workflow from last sprint/pass checkpoint
/workflow clear                                   # delete saved state for this session (with confirmation)
```

The `dev` workflow implements a designer → generator → evaluator loop across one or more sprints:

- **designer** — reads the task description, produces a spec and sprint plan (`<session-id>.workflow.plan.md`).
- **generator** — executes each sprint according to the plan, writes output to `<session-id>.workflow.sprint<N>.md`.
- **evaluator** — reviews the sprint output and returns a structured verdict: `good_to_go`, `needs_refinement`, or `next_sprint`. Findings are written to `<session-id>.workflow.findings<N>.md`.

Loop semantics: `needs_refinement` re-runs the generator for the same sprint (up to a configurable pass limit, default 3); `next_sprint` advances; `good_to_go` on the final sprint ends the workflow. Pass limit exceeded halts with an error showing the last findings path.

Agent specifiers accept any name from `config.agents`, plus aliases `primary` (the currently assigned primary agent) and `escalation` (the currently assigned escalation agent). Aliases are resolved once at start; mid-workflow `/agent switch` does not affect a running workflow.

Workflow state is persisted to `~/.milk/sessions/<session-id>.workflow.json` after each evaluator call. `/workflow resume` re-launches the workflow from the last checkpointed sprint/pass, skipping the designer (plan file is reused), using the same agents recorded in the state file. `/workflow clear` deletes the state file after a confirmation prompt (type `clear` to confirm, anything else cancels).

The workflow progress panel (toggled with `/panel workflow`, auto-opens at start) shows the current sprint, pass, role, and verdict history.

**/config** manages the milk configuration:

| Subcommand | Action |
|---|---|
| `/config` | Print current config JSON in the transcript |
| `/config init` | Run the interactive setup wizard (create or update `~/.milk/config.json`) |
| `/config open` | Open `~/.milk/config.json` in the configured editor |

The editor used by `/config open` is selected from the `config_editors` list (see Configuration). The same commands are available on the CLI as `milk config`, `milk config init`, `milk config open`.

**`milk otel`** manages observability settings from the CLI (no TUI required):

| Command | Action |
|---|---|
| `milk otel debug enable` | Enable full debug logging: `otel.log_context=true`, `otel.log_level=DEBUG`, `debug_claude_code=true`, `debug_local=true`, `debug_subprocess=true` |
| `milk otel debug disable` | Disable debug logging: restores `otel.log_context=false`, `otel.log_level=INFO`, `debug_claude_code=false`, `debug_local=false`, `debug_subprocess=false` |

`milk otel debug enable` prints the paths where each debug stream is written:
- Claude subprocess NDJSON → `~/.milk/claude_debug.ndjson`
- Local agent SSE → `~/.milk/local_debug.log`
- Subprocess agent stdout → `~/.milk/subprocess_debug.log`
- Request payloads (log_context) → `~/.milk/otel/logs.jsonl`

**/open** opens any file in the configured editor:

```
/open <path>
/open @<path>   (@ prefix is stripped automatically)
```

The same editor resolution as `/config open` is used. The agent can also open files via the `open_file` tool when asked to do so.

**/update** checks for new milk releases on GitHub, compares the running version against the latest published release, and prompts the user to download and install:

```
/update
```

If a newer release is available, milk shows the current and latest versions and asks for confirmation before downloading the appropriate binary for the current platform. If already up to date, a confirmation message is shown and no download occurs.

**Multi-line input:** Shift+Enter or Alt+Enter inserts a newline; Enter submits. Bracketed paste is handled transparently — multi-line pastes are sent as a single block.

**Keyboard:** Up/Down navigates input history (single-line mode only); Ctrl-C clears a pending force-mode flag or exits; Ctrl-D exits.

**Memory panel:** A 34-column right-side panel shows SESSION / GLOBAL / GLOBAL (core) percept sections in real time (polls every 5s). Each percept displays a short `#<6hex>` ID (dim), content wrapped to 2 lines, and weight right-aligned. Percepts updated within the last 60s are highlighted bold+yellow. Toggle with `/panel memory`.

### Flags

| Flag | Description |
|------|-------------|
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

`~/.milk/config.json`:

```json
{
  "agent": "local",
  "agents": [
    {
      "name": "local",
      "url": "http://localhost:8080",
      "model": "qwen2.5-coder",
      "provider": "local"
    },
    {
      "name": "haiku-aws",
      "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
      "model": "arn:aws:bedrock:...:application-inference-profile/...",
      "provider": "bedrock",
      "aws_region": "eu-central-1"
    },
    {
      "name": "claude",
      "provider": "claude-cli",
      "bin": "claude"
    }
  ],
  "escalation_agent": "claude",
  "default_route": "local",
  "colorization": "balanced",
  "show_reasoning": true,
  "sticky_escalation": true,
  "aws_auth_refresh": false,
  "rules": {
    "escalate_above_tokens": 2000,
    "local_below_tokens": 30,
    "escalate_keywords": ["refactor entire", "context brick", "memory panel", "panel memory"],
    "escalate_threshold": 6,
    "local_threshold": -4,
    "local_verb_weight": -3,
    "escalate_verb_weight": 4,
    "path_ref_weight": -2,
    "code_block_weight": -2,
    "open_question_weight": 3,
    "classifier_fallback": "local",
    "local_verbs": [
      "grep", "find", "list", "run", "read", "fix", "debug", "show", "cat", "ls",
      "check", "print", "count", "search", "add", "create", "write", "implement",
      "rename", "delete", "move",
      "aggiungi", "crea", "scrivi", "implementa", "rinomina", "elimina", "sposta",
      "cerca", "mostra", "controlla", "esegui", "leggi"
    ],
    "escalate_verbs": [
      "architect", "design", "refactor", "explain why", "compare", "evaluate",
      "plan", "propose", "summarize", "review", "analyze", "describe",
      "progetta", "refactorizza", "spiega perché", "confronta", "valuta",
      "pianifica", "proponi", "riassumi", "revisiona", "analizza", "descrivi"
    ],
    "open_question_prefixes": [
      "what", "why", "how", "when", "where", "who", "which",
      "could you", "can you", "would you", "should", "is it", "are there", "do you", "does",
      "cosa", "come", "perché", "quando", "dove", "chi", "quale", "quali",
      "potresti", "puoi", "dovresti", "è possibile", "ci sono", "sai"
    ]
  }
}
```

`agent` names the active primary backend from `agents`. If empty, the first non-`claude-cli` entry is used.

`escalation_agent` selects which `agents` entry handles escalated turns. Defaults to `"claude"` (the built-in `claude-cli` entry). Set to the name of any `agents` entry — including another inference-server backend — to route escalated turns there instead. Change at runtime with `/agent switch <name> as escalation`.

A built-in `claude-cli` entry named `"claude"` is always available even if not listed explicitly in `agents`. When absent from the file, it is injected in-memory with `bin: "claude"`. 

### `rules` field

Controls the layered routing scorer. All fields have built-in defaults; only the fields you want to override need to be present.

| Field | Type | Default | Description |
|---|---|---|---|
| `escalate_above_tokens` | int | 2000 | Prompt exceeding this approximate token count is unconditionally escalated |
| `local_below_tokens` | int | 30 | Prompt at or below this approximate token count is unconditionally kept local |
| `escalate_keywords` | array of string | see below | Substring matches that unconditionally escalate (hard, conclusive). Keep this list short and specific — use `escalate_verbs` for soft signals |
| `escalate_threshold` | int | 6 | Soft score ≥ this → conclusive escalation |
| `local_threshold` | int | -4 | Soft score ≤ this → conclusive local |
| `local_verb_weight` | int | -3 | Score delta per `local_verbs` match (negative = towards local) |
| `escalate_verb_weight` | int | 4 | Score delta per `escalate_verbs` match (positive = towards escalation) |
| `path_ref_weight` | int | -2 | Score delta when the prompt contains a path that resolves on disk |
| `code_block_weight` | int | -2 | Score delta when the prompt contains a fenced code block |
| `open_question_weight` | int | 3 | Score delta when the prompt starts with an open-question prefix |
| `classifier_fallback` | string | `"local"` | What to do when the scorer is inconclusive: `"local"` calls the primary model as a classifier; `"escalation"` escalates directly |
| `local_verbs` | array of string | see below | Words/phrases (substring match) that contribute `local_verb_weight` to the score. One match per prompt (first hit wins) |
| `escalate_verbs` | array of string | see below | Words/phrases (substring match) that contribute `escalate_verb_weight` to the score. One match per prompt (first hit wins) |
| `open_question_prefixes` | array of string | see below | Words/phrases (case-insensitive prefix match with word-boundary check) that trigger the open-question soft signal |

#### Keyword design guidelines

**`escalate_keywords` (hard, conclusive)** — only add multi-word or highly specific phrases that unambiguously signal a complex conceptual task, such that routing to local would always be wrong. Single common words (e.g. `design`, `analyze`) are too broad: *"the design looks off"* or *"analyze this traceback"* are local tasks. When in doubt, put the term in `escalate_verbs` instead.

**`escalate_verbs` and `local_verbs` (soft signals)** — these contribute a signed score rather than making a binding decision. One match per prompt is counted (first hit wins), so the lists' relative weights and the `escalate_threshold` / `local_threshold` values control how much weight a single verb match carries. Adding more terms to these lists makes routing more decisive; raising the thresholds makes it more conservative.

**`open_question_prefixes`** — prefix-matched (word boundary required) against the start of the trimmed prompt. A match adds `open_question_weight` to the soft score. This is typically combined with an `escalate_verbs` hit to cross the `escalate_threshold`.

#### Adding language or domain-specific terms

The built-in lists cover English and Italian. To extend coverage for other languages or domain-specific vocabulary, add terms directly to the arrays in your `~/.milk/config.json`. The lists are fully replaced by whatever you provide — there is no merge with the built-in defaults; copy the full default set and extend it.

Example — adding French question starters and domain verbs:

```json
"open_question_prefixes": [
  "what", "why", "how", "when", "where", "who", "which",
  "could you", "can you", "would you", "should", "is it", "are there", "do you", "does",
  "cosa", "come", "perché", "quando", "dove", "chi", "quale", "quali",
  "potresti", "puoi", "dovresti", "è possibile", "ci sono", "sai",
  "quoi", "pourquoi", "comment", "quand", "où", "qui", "quel", "quelle",
  "pourriez-vous", "pouvez-vous", "devriez-vous", "est-ce possible"
],
"escalate_verbs": [
  "architect", "design", "refactor", "explain why", "compare", "evaluate",
  "plan", "propose", "summarize", "review", "analyze", "describe",
  "progetta", "refactorizza", "spiega perché", "confronta", "valuta",
  "pianifica", "proponi", "riassumi", "revisiona", "analizza", "descrivi",
  "concevoir", "évaluer", "planifier", "proposer", "résumer", "analyser"
]
```

#### Default keyword lists

`escalate_keywords` (conclusive hard triggers):
```
"refactor entire", "context brick", "memory panel", "panel memory"
```

`escalate_verbs` (soft, +4 each):
```
English: architect, design, refactor, explain why, compare, evaluate,
         plan, propose, summarize, review, analyze, describe
Italian: progetta, refactorizza, spiega perché, confronta, valuta,
         pianifica, proponi, riassumi, revisiona, analizza, descrivi
```

`local_verbs` (soft, −3 each):
```
English: grep, find, list, run, read, fix, debug, show, cat, ls,
         check, print, count, search, add, create, write, implement,
         rename, delete, move
Italian: aggiungi, crea, scrivi, implementa, rinomina, elimina, sposta,
         cerca, mostra, controlla, esegui, leggi
```

`open_question_prefixes`:
```
English: what, why, how, when, where, who, which,
         could you, can you, would you, should, is it, are there, do you, does
Italian: cosa, come, perché, quando, dove, chi, quale, quali,
         potresti, puoi, dovresti, è possibile, ci sono, sai
```

### `agents` entry fields

#### Inference-server fields (all providers except `claude-cli`)

| Field | Type | Description |
|---|---|---|
| `name` | string | Display name; used as selector key |
| `url` | string | Base URL of the inference server |
| `model` | string | Model name or ARN |
| `provider` | string | Auth transport: `""` / `"local"` = none; `"bedrock"` = AWS SigV4; anything else = Bearer token |
| `api_key` | string | Static Bearer token; superseded by `token_cmd` when both are set |
| `token_cmd` | string | Shell command whose stdout is the Bearer token; re-run on 401/403 |
| `chat_path` | string | Override inference path (default `/v1/chat/completions`) |
| `headers` | object | Extra HTTP headers (e.g. `"api-key"` for Azure, `"HTTP-Referer"` for OpenRouter) |
| `tls_skip_verify` | bool | Disable TLS cert verification (dev/self-signed only) |
| `tls_ca_cert` | string | Path to PEM CA cert for private endpoints |
| `aws_region` | string | AWS region for Bedrock (fallback: `AWS_REGION` env, then parsed from `url`) |
| `aws_key_id` | string | AWS access key ID (fallback: `AWS_ACCESS_KEY_ID` env) |
| `aws_secret` | string | AWS secret key (fallback: `AWS_SECRET_ACCESS_KEY` env) |
| `aws_token` | string | AWS session token (fallback: `AWS_SESSION_TOKEN` env) |
| `aws_service` | string | SigV4 service name (default `"bedrock"`) |
| `aws_refresh_cmd` | string | `credential_process`-compatible command; on 403 the SigV4 transport runs it, swaps credentials, and retries once |

#### Claude CLI fields (`provider: "claude-cli"`)

| Field | Type | Description |
|---|---|---|
| `name` | string | Display name; used as selector key |
| `provider` | string | Must be `"claude-cli"` |
| `bin` | string | Path to the `claude` binary (default `"claude"`) |
| `dangerously_skip_permissions` | bool | Auto-approve all tool uses without prompting |
| `allowed_tools` | array of string | Tools pre-approved; passed as `--allowedTools` |
| `add_dirs` | array of string | Extra directories; passed as `--add-dir` |

#### Common fields (all providers)

| Field | Type | Description |
|---|---|---|
| `tools` | array of AgentToolEntry | Per-agent overrides/extensions of the global `agent_tools` list. An entry whose `agent` name matches a global entry replaces it; new names are appended. |

### `agent_tools` field

Global list of peer agents that can be called as tools by any agent. Each entry is an `AgentToolEntry` object:

| Field | Type | Description |
|---|---|---|
| `agent` | string | Name of the agent to expose as a tool (must match a name in `agents`) |
| `description` | string | Description shown to the calling agent as the tool's purpose |
| `enabled` | bool | Whether the tool is active (default `true` when omitted) |

Per-agent entries in `AgentConfig.tools` shadow or extend this global list (same `agent` name = replace; new name = append). Cycle guard: an agent cannot call itself as a tool. Unknown agent names are silently dropped.

Example:
```json
"agent_tools": [
  { "agent": "haiku-aws", "description": "Fast summarization and classification agent." },
  { "agent": "claude", "description": "Full-capability Claude Code escalation agent.", "enabled": false }
]
```

Use `/agent tool` subcommands to manage tool-agents at runtime.

### `mcp_servers` field

Global list of MCP (Model Context Protocol) servers that agents can connect to. Each entry is an `MCPServerConfig` object:

| Field | Type | Description |
|---|---|---|
| `name` | string | Unique identifier referenced from `AgentConfig.mcp_servers` |
| `url` | string | MCP endpoint. Required for `http` transport (e.g. `"http://localhost:3000/mcp"`). For Streamable HTTP transport this is a single POST+GET endpoint |
| `transport` | string | Wire protocol: `"http"` (default) uses Streamable HTTP with SSE fallback; `"stdio"` launches a subprocess and communicates over its stdin/stdout |
| `command` | string | Executable path. Required when `transport` is `"stdio"` |
| `args` | string[] | Command-line arguments for the stdio subprocess |
| `enabled` | bool | Whether the server is active (default `true` when omitted) |

Reference servers from an agent entry via `"mcp_servers": ["my-mcp"]` in the `agents` list.

#### `mcp_connect_timeout_secs`

Per-server startup connect timeout in seconds. Default: `5`.

```json
"mcp_connect_timeout_secs": 10
```

If a server does not respond within the timeout at startup, milk logs a warning and continues. The server's tools are still registered; the client reconnects lazily on the first tool call that targets the server.

#### Lazy reconnect

When a startup connection times out or fails, milk defers the live connection rather than aborting the session. On the first tool call targeting the server, milk retries the connection automatically. If reconnect succeeds the call proceeds normally; if it fails the tool returns an error result to the agent. Each lazy reconnect attempt is recorded as an `mcp.lazy_reconnect` span in `~/.milk/otel/traces.jsonl`.

#### `--mcp-config` generation (`claude-cli` and `aider-cli` agents)

For `claude-cli` and `aider-cli` agents, milk translates the applicable `mcp_servers` entries into a JSON config file and passes it via the `--mcp-config` flag. milk acts as an MCP proxy: each server entry is re-exported as a stdio-transport entry pointing back to milk's internal MCP proxy process, so Claude Code and aider see MCP tools natively without requiring direct network access to the upstream server.

#### Context injection (`subprocess` agents)

For `subprocess` agents (smolagents and compatible adapters), MCP tool schemas are serialised and injected into the agent's context files alongside the built-in tool descriptions. The subprocess agent sees MCP tools as additional callable functions in its context.

#### OTel observability

The MCP client emits spans and counters to `~/.milk/otel/`:

| Signal | Type | Description |
|---|---|---|
| `mcp.connect` | span | One span per server per connect attempt; `status` attribute is `ok` or `error` |
| `mcp.tool_call` | span | One span per tool invocation; includes `server`, `tool`, and `status` attributes |
| `mcp.lazy_reconnect` | span | Emitted when a deferred reconnect is triggered on first tool call |
| `mcp.connect_failures` | counter | Incremented on each failed connect or lazy reconnect failure |
| `mcp.tool_calls` | counter | Total tool calls dispatched through the MCP client |

### `colorization` field

Controls transcript syntax and Markdown rendering. Applied per turn to avoid ANSI contamination across turns.

| Value | Behavior |
|---|---|
| `"off"` | No colorization — raw text, ANSI from agent labels preserved |
| `"fenced"` | Syntax-highlight fenced code blocks only (chroma); default |
| `"balanced"` | Fenced blocks + inline Markdown: bold, inline code, headings, bullets, blockquotes, HR |
| `"full"` | Full Markdown render via glamour (reflows prose, all Markdown elements) |

### `show_reasoning` field

Controls whether thinking/reasoning tokens are shown in the transcript by default. Can be overridden live with `/think on|off`. When `false`, thinking blocks are replaced with a `[thinking…]` placeholder. Omit or set to `true` to show reasoning (default).

### `config_editors` field

Ordered list of editor commands tried by `/config open` and `/open`. The first command found on `$PATH` is used. Environment variables (e.g. `$EDITOR`, `$VISUAL`) are expanded before lookup.

Default (when omitted): `["$EDITOR", "$VISUAL", "nano", "vim", "vi"]`

Example — prefer VS Code, fall back to `$EDITOR`:
```json
"config_editors": ["code --wait", "$EDITOR", "nano"]
```

### `sticky_escalation` field

When `true` (default), the first router-triggered escalation automatically keeps subsequent turns on the escalation agent — shown as `<agent> (sticky)` in the status bar. Cleared by `/primary` or a single-turn `/primary <prompt>` override. Set to `false` to re-evaluate routing on every turn. Explicit `/escalate` pinning is unaffected by this setting.

### `debug_claude_code` field

When `true`, every raw NDJSON line emitted by the Claude CLI subprocess is appended to `~/.milk/claude_debug.ndjson`. The `.ndjson` extension reflects the content: each line is a self-contained JSON object, making the file valid Newline-Delimited JSON suitable for `jq` or any NDJSON-aware tool. Useful for diagnosing Claude CLI protocol issues, unexpected event types, or streaming gaps. Default: `false`.

### `debug_local` field

When `true`, every raw SSE line received from the local agent's HTTP stream is appended to `~/.milk/local_debug.log` — including lines that are skipped, blank separator lines, and lines that fail to parse. The `.log` extension reflects the content: SSE frames include `data:` and `event:` prefixes, blank separators, and other protocol framing that is not pure JSON. Useful for diagnosing dropped tokens, unknown event types, or SSE parser mismatches. Default: `false`.

### `debug_subprocess` field

When `true`, every raw stdout line emitted by subprocess agents (aider-cli, smolagent) is appended to `~/.milk/subprocess_debug.log`. Useful for diagnosing NDJSON parse errors, unexpected agent output, or protocol mismatches. Default: `false`.

### `otel.log_context` field

When `true`, the full content of every request payload is logged via `obs.LogPayload` at DEBUG level to `~/.milk/otel/logs.jsonl`. This covers:

- **claude-cli agent**: static context file, dynamic context file, prompt, and MCP config JSON (the `--mcp-config` temp file passed to the subprocess)
- **local/Bedrock agent**: full serialised inference request body sent to the HTTP endpoint, plus classifier request bodies
- **subprocess agents (aider, smolagents)**: static context, dynamic context, and prompt passed as temp files

Requires `otel.log_level: "DEBUG"` to appear in the log output. Default: `false`.

Use `milk otel debug enable` to turn on the full debug bundle in one command.

**Azure workaround:** Azure OpenAI uses a non-standard URL path (`/openai/deployments/<deployment>/chat/completions?api-version=…`) and an `api-key` header rather than Bearer auth. Set `url` to the full deployment endpoint and add `{"api-key": "<key>"}` to `headers`. A dedicated Azure provider with URL templating is tracked in GitHub Issues.

---

## Graceful Degradation

| Primary agent | Escalation agent | behavior |
| --- | --- | --- |
| up | available (any provider) | normal routing |
| down | available | warn once per session, route all to escalation agent |
| up | unavailable/not installed | warn once per session, stay primary-only |
| down | unavailable | error + exit |

---

## Streaming

Both agents stream output in real time:

- **Primary agent**: SSE from OpenAI-compat API (`stream: true`), or AWS binary event-stream from Bedrock Converse API (provider-specific frame decoder)
- **Claude agent**: NDJSON from `--output-format stream-json`, parsed line by line

milk relays tokens to stdout as they arrive.

---

## Backlog

- Planning mode (offline, no LLM execution)
- Demotion from escalation back to primary mid-session
- Web UI / TUI
- MCP stdio transport for local tools ✓ (done)
- Multi-user / daemon mode
