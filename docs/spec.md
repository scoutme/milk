# milk — specification

## Overview

milk lets you switch between a local LLM agent and a rich cloud agent (Claude Code CLI) mid-workflow, maintaining full session context across the switch. The local agent supports OpenAI-compatible servers (local or remote) and AWS Bedrock natively.

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
│  ROUTING → LOCAL | CLAUDE | CLAUDE_WAITING          │
└──────────┬──────────────────────────────────────────┘
           │
           ▼
┌──────────────────────────┐
│  Router                  │
│  1. Explicit flags       │  --escalate, --local
│  2. Session state check  │  CLAUDE_WAITING → bypass
│  3. Rules layer          │  heuristics + weighted scorer
│  4. Local model          │  local model self-classification
│  5. Default: try local   │
└────────┬─────────────────┘
         │
    ┌────┴─────┐
    ▼           ▼
LOCAL           CLAUDE
agent           agent
OpenAI API      claude --print
OpenAI API      --output-format stream-json
tool loop       --session-id / --resume
```

### Session state machine

```
States: ROUTING | LOCAL | CLAUDE | CLAUDE_WAITING

ROUTING        → rules + local model decision → LOCAL or CLAUDE
LOCAL          → --escalate OR local model escalate_to_claude() → CLAUDE
CLAUDE         → Claude ends turn with question → CLAUDE_WAITING
CLAUDE_WAITING → next user input bypasses router → direct --resume to Claude
CLAUDE_WAITING → user --local flag → back to ROUTING
```

---

## Router

Decision order per turn:

1. **Explicit flags** — `--escalate` forces Claude; `--local` forces local (always wins)
2. **Session state** — if `CLAUDE_WAITING`, bypass router, send directly to `claude --resume`
3. **Rules layer** — layered scorer:
   - Hard rules: token length above `escalate_above_tokens` → Claude; keyword match → Claude
   - Short-prompt shortcut: ≤ `local_below_tokens` tokens → conclusive local
   - Weighted signal scorer: local verbs, escalate verbs, path references, code blocks, open questions each contribute a signed score; conclusive if score reaches `escalate_threshold` or `local_threshold`
4. **Local model classification** — when scorer is inconclusive, ask the local model with minimal prompt, expect `route: local | escalate`; behaviour configurable via `classifier_fallback`
5. **Default** — attempt local; escalate if local returns `escalate_to_claude(reason)`

The classifier uses the same model instance as the local coding agent. No second model or second inference server instance.

---

## Local Agent

- Backend: configurable via `local_agents` in `~/.milk/config.json`; multiple named backends can coexist and be switched at runtime with `/provider switch <name>`
- Protocols: OpenAI-compatible Chat Completions API (llama.cpp, Ollama, LM Studio, vLLM, OpenRouter, Together.ai, Groq, Azure OpenAI) **or** AWS Bedrock Converse API natively (binary event-stream, SigV4 signing — not OpenAI-compat)
- Model: any tool-calling-capable model. Tested: Qwen2.5-Coder 7B/3B, Gemma 4 E4B, Claude Haiku (via Bedrock).

### Remote inference / authentication

The `provider` field in a `local_agents` entry selects the auth transport:

| `provider` | Auth mechanism | Required fields |
|---|---|---|
| `""` / `"local"` | None (plain HTTP) | — |
| `"bedrock"` | AWS SigV4 | `aws_region` + credentials (config or env vars) |
| `"bearer"` or any other string | `Authorization: Bearer <api_key>` | `api_key` |

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
  - `write_file(path string, content string) → ok` — creates parent directories
  - `edit_file(path string, old_string string, new_string string) → ok` — exact-string replacement, rejects ambiguous matches
  - `list_dir(path string) → entries` — names, types, sizes
  - `http_get(url string, max_bytes int) → body` — bounded HTTP fetch
  - `get_session_context() → history` — returns the full shared session history (both agents) so the local model can see prior Claude turns
- Self-escalation: local model may return `escalate_to_claude(reason string)` as a tool call to trigger promotion

---

## Claude Agent

- Interface: `claude` CLI subprocess
- **AWS credential injection**: when `aws_auth_refresh: true` in `~/.milk/config.json`, milk reads the `awsAuthRefresh` command from `~/.claude/settings.json`, runs it before each turn to obtain fresh STS credentials, and injects them as explicit `AWS_*` env vars into the subprocess. Conflicting vars (`AWS_BEARER_TOKEN_BEDROCK`, `ANTHROPIC_DEFAULT_*_MODEL`, `AWS_PROFILE`, etc.) are stripped from the inherited environment to prevent wrong-account overrides. See ADR 23.
- First escalation turn:
  ```
  claude --print --output-format stream-json \
         --session-id <new-uuid> \
         --append-system-prompt "<formatted transcript + context>"
  ```
- Subsequent turns in same escalation:
  ```
  claude --print --output-format stream-json \
         --resume <claude-session-id> \
         "<user prompt>"
  ```
- `session_id` is extracted from the first NDJSON message and persisted to the milk session file
- Claude orients itself from the appended context — no separate reformulation step

### Permission prompt flow

milk passes `--permission-prompt-tool stdio` on every Claude invocation. When Claude wants to use a tool that has not been pre-approved, it emits a `control_request` NDJSON event on stdout and pauses. milk intercepts this event and, in TUI mode, routes a blocking prompt through the bubbletea message queue (see ADR-0015):

1. The agent goroutine calls `tuiInputReader.readLine(prompt)` and blocks on a channel.
2. The TUI appends the prompt to the transcript and switches key events to `handlePermKey`.
3. The user types `y` (allow) or `n` (deny) and presses Enter; Ctrl-C sends `n`.
4. milk writes a `control_response` JSON to Claude's stdin; the agent goroutine unblocks.

The prompt shows the tool name, key arguments, and — for `workingDir` blocks — the restricted path. The session stays alive throughout; no `--resume` round-trip is needed.

`dangerously_skip_permissions` (config field) bypasses this flow entirely: Claude auto-approves all tool uses. `/skip-permissions on|off` overrides this setting per session without restarting.

Pre-approved tools and directories can be listed in `allowed_tools` and `add_dirs` config fields; they are passed as `--allowedTools` / `--add-dir` flags and never trigger a prompt.

### Context handoff (escalation)

When promoting from local to Claude, milk formats the local conversation history as a plain transcript and passes it via `--append-system-prompt`. Format:

```
[Context from local agent session]
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
  "state": "CLAUDE_WAITING",
  "claude_session_id": "abc123",
  "history": [
    {
      "role": "user | assistant | tool_result",
      "agent": "local | claude",
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

**Slash commands:** `/escalate`, `/local`, `/new`, `/drop`, `/list`, `/paste`, `/skip-permissions`, `/provider`, `/colorize`, `/think`, `/help`, `/exit`

**Memory commands:** `/learn <statement>`, `/memory [global|session|<pattern>]`, `/memory show <pattern or #id>`, `/forget <pattern or #id>`, `/export [json|<path>]`

The `#id` form in `/forget` and `/memory show` accepts a short hex prefix (4–64 chars). The `#` prefix is optional — bare hex like `a1b2c3d4` also works. The local agent can also delete percepts directly via the `forget_memory` tool (same short-ID resolution, same `#` handling).

**Panel commands:** `/panel memory` — toggle the right-side memory panel (open by default)

**/skip-permissions** toggles `dangerously_skip_permissions` for the current session: `on` makes Claude auto-approve all tool uses without prompting; `off` (default) re-enables the per-tool permission flow. The current state is shown with `/skip-permissions` alone. A red warning banner is printed at startup if the flag is already on via config.

**/provider** manages local-agent backends at runtime:

| Subcommand | Action |
|---|---|
| `/provider` | Show active backend (URL, model, auth method) |
| `/provider list` | List all configured backends; active marked with `*` |
| `/provider switch <name>` | Switch to the named backend (rebuilds agent, pings) |
| `/provider add` | Add a backend via interactive wizard (prompts for each field) |
| `/provider add name=… url=… model=… [provider=…] [api_key=…] [aws_region=…]` | Add inline |

New backends are appended to `local_agents` in `~/.milk/config.json` immediately. Use `/provider switch` to activate a newly added backend in the current session.

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

The toggle is retroactive — both transcript variants (full and no-think) are maintained in parallel during streaming, so switching is instantaneous with no rebuild. The default is configurable via `show_reasoning` in `~/.milk/config.json` (default: `true`). Applies to both local model `<think>` blocks and Claude extended thinking tokens.

**Multi-line input:** Shift+Enter or Alt+Enter inserts a newline; Enter submits. Bracketed paste is handled transparently — multi-line pastes are sent as a single block.

**Keyboard:** Up/Down navigates input history (single-line mode only); Ctrl-C clears a pending force-mode flag or exits; Ctrl-D exits.

**Memory panel:** A 34-column right-side panel shows SESSION / GLOBAL / GLOBAL (core) percept sections in real time (polls every 5s). Each percept displays a short `#<6hex>` ID (dim), content wrapped to 2 lines, and weight right-aligned. Percepts updated within the last 60s are highlighted bold+yellow. Toggle with `/panel memory`.

### Flags

| Flag | Description |
|------|-------------|
| `--escalate` | Force route to Claude for this turn |
| `--local` | Force route to local model for this turn; breaks CLAUDE_WAITING state |
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
  "local_agent": "haiku",
  "local_agents": [
    {
      "name": "haiku",
      "url": "https://bedrock-runtime.eu-central-1.amazonaws.com",
      "model": "arn:aws:bedrock:...:application-inference-profile/...",
      "provider": "bedrock",
      "aws_region": "eu-central-1"
    },
    {
      "name": "local",
      "url": "http://localhost:8080",
      "model": "gemma4"
    }
  ],
  "claude_bin": "claude",
  "default_route": "local",
  "colorization": "balanced",
  "show_reasoning": true,
  "aws_auth_refresh": false,
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
    "classifier_fallback": "local",
    "local_verbs": ["grep", "find", "list", "run", "read", "fix", "debug", "show", "cat", "ls", "check", "print", "count", "search"],
    "escalate_verbs": ["architect", "design", "refactor entire", "explain why", "compare", "evaluate", "plan", "propose", "summarize", "review"]
  }
}
```

`local_agent` names the active backend from `local_agents`. If empty, the first entry is used.

### `local_agents` entry fields

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

**Azure workaround:** Azure OpenAI uses a non-standard URL path (`/openai/deployments/<deployment>/chat/completions?api-version=…`) and an `api-key` header rather than Bearer auth. Set `url` to the full deployment endpoint and add `{"api-key": "<key>"}` to `headers`. A dedicated Azure provider with URL templating is tracked in GitHub Issues.

---

## Graceful Degradation

| Inference server | claude CLI | behavior |
| --- | --- | --- |
| up | available | normal routing |
| down | available | warn once per session, route all to Claude |
| up | unavailable/not installed | warn once per session, stay local-only |
| down | unavailable | error + exit |

---

## Streaming

Both agents stream output in real time:

- **Local agent**: SSE from OpenAI-compat API (`stream: true`), or AWS binary event-stream from Bedrock Converse API (provider-specific frame decoder)
- **Claude agent**: NDJSON from `--output-format stream-json`, parsed line by line

milk relays tokens to stdout as they arrive.

---

## Backlog

- Planning mode (offline, no LLM execution)
- Demotion from Claude back to local mid-session
- Web UI / TUI
- MCP server integration for local tools
- Multi-user / daemon mode
