# Tooling, MCP, and tool-agents

milk gives any agent — primary or escalation, on any provider — three ways to act beyond generating text: built-in tools, other configured agents exposed as callable tools, and MCP servers.

---

## Built-in tools

The primary agent (HTTP or Bedrock backends) has these tools available with no configuration:

| Tool | Parameters | Returns |
|---|---|---|
| `bash(command, workdir?, run_in_background?)` | `command string` | stdout, stderr, exit_code — or a `shell_id` with `run_in_background` (see [Background shells](#background-shells)) |
| `bash_output(shell_id, tail_lines?)` | shell ID from `bash run_in_background` | status line (running/exited/killed) + retained output |
| `kill_bash(shell_id)` | shell ID | ok — stops the shell's process group; output stays readable until reaped |
| `find_files(path, pattern)` | root dir + glob (`*`, `?`, `**` — e.g. `*_test.go`, `src/**/*.ts`) | matching file paths, newest-first, capped at 500 with a truncation marker — honors `.gitignore` via git when available; use `grep` for contents |
| `project_map(path?, max_files?, max_chars?)` | optional subtree + caps (window-scaled defaults: floors 200 files / 6000 chars, caps 1000 / 50000 — call args override) | token-cheap structural map: a framing header (project identity, docs and build/config landmarks) plus files grouped by directory with symbols and **signatures only** (Go via `go/ast`; JS/TS, Python, Java, Rust via heuristic scanners — never bodies); other files listed by name. Output degrades to fit the budget (signatures → names → reference-ranked names → bare file tree) and per-file summaries are cached in-process *and* on disk (`~/.milk/outline-cache/`, mtime-keyed) so repeat calls don't re-parse. Use it to orient before reading files |
| `grep(pattern, path, recursive, ignore_case?, context?, include?, max_results?)` | pattern/path + optional filters | matching lines (extended regex, e.g. `foo\|bar`) — ripgrep when available, else `git grep` inside a git work tree (both honor `.gitignore`), else a built-in Go search that needs no external binary (Go regex syntax incl. `\d`/`\w`/`(?i)`; skips hidden, vendored and binary files; no ignore-file semantics); capped at 200 matching lines and 2000 characters per line, with truncation markers; use `find_files` for filenames |
| `read_file(path, offset, limit)` | path + optional range (`offset` is a 1-based line number matching the output numbering) | content |
| `write_file(path, content)` | — | ok — creates parent directories; expands `~`; writes atomically (temp file + rename) |
| `edit_file(path, old_string, new_string, replace_all)` | — | ok — exact-string replacement first; on a miss falls back to a whitespace/indentation/escape-tolerant match (uniqueness still required, reported as `fuzzy match: <strategy>`), and on total failure returns the closest matching block as a hint that is **never** applied silently. Writes atomically (temp file + rename) and preserves CRLF |
| `multiedit(path, edits)` | `[{old_string, new_string, replace_all?}]` applied in order | ok — all-or-nothing batch: every edit is validated in memory (each sees the previous edit's result, exactly `edit_file`'s matching rules and hints) and the file is written once at the end; any failing edit writes nothing and names itself plus its closest-match block. One round trip instead of a burst of `edit_file` calls |
| `delete_file(path)` | — | ok — permission-gated |
| `move_file(source, destination)` | — | ok — permission-gated; creates destination parent directories |
| `list_dir(path)` | — | entries (names, types, sizes) |
| `web_fetch(url, format?, max_bytes?)` | `url` + optional `format` (`auto`\|`markdown`\|`text`\|`html`\|`raw`) | page body — HTML converted to Markdown by default, `http://` upgraded to `https://` for non-local hosts, body capped at 256 KB (5 MB max) with a truncation marker; oversized bodies spill to a file like other tool output. Web **search** is not built in — use an MCP search server |
| `http_request(method, url, headers, body, max_bytes)` | — | body, status — permission-gated |
| `get_session_context()` | — | full shared session history (both agents), so the primary model can see prior escalation turns |
| `get_context_stats()` | — | current history turn counts and total character size, so the agent can self-regulate before hitting context limits |
| `open_file(path)` | — | ok — opens the file: the configured editor in the TUI, the platform opener over ACP; refused (TUI-only note) where no opener is wired, e.g. single-prompt mode or background jobs |
| `current_need(goal)` | one-sentence goal | ok — same effect as the user typing `/need <goal>` |
| `export_session(format, output_path)` | `"text"`\|`"json"`, optional file path | transcript inline, or written to `output_path` — reasoning included by default (`<reasoning>` fences / JSON `thinking`), bounded at 8 KB per block with a head+tail note (issue #277) |
| `ask_user(question, options?)` | question + optional suggested choices | the user's answer — or explicit "nobody answered" guidance. Races direct input and remote oversight; unattended asks are bounded by the timed answer (see [Asking the human mid-turn](#ask_user-asking-the-human-mid-turn)) |
| `milk_config_help(topic)` | e.g. `"mcp add"`, `"agent add"` | a section of milk's own embedded reference docs — lets the agent look up how to manage milk's own config instead of guessing the schema; omit `topic` to list what's available. Backed by `internal/selfdocs`, which indexes `docs/reference/spec.md`/`providers.md`/`workflows.md`/`tooling.md`/`operations.md` by heading — same content this site is built from |
| `spawn_background_agent(task, label, full_context?)` | self-contained task description + short display label + optional full-context flag | an immediate acknowledgement (job ID) — the eventual result arrives asynchronously, injected into the agent's next turn and shown live in the TUI. See [ADR-0043](../adr/0043-background-subagents.md) |
| `cancel_background_agent(job_id)` | a job ID returned by `spawn_background_agent` | confirmation, or an error if the job is already finished/unknown — the model-facing equivalent of `/bg stop` |
| `status_background_agent(job_id?)` | optional job ID | one job's state, task and result in full; without an ID, every job as a one-line summary — the model-facing counterpart to `/bg` |
| `board_get()` | — | the shared workflow board rendered compact (items + latest findings) — **only** in a `board: true` workflow turn (see below) |
| `board_claim(item)` | item index | CAS: mark the item `claimed` as your identity; held/done items come back with the current holder (idempotent for you) — `board: true` turns only |
| `board_update(item, status?, notes?)` | item index + `todo`/`in_progress`/`blocked`/`done` + notes | move an item (release by returning it to `todo`; `claimed` only via `board_claim`) — `board: true` turns only |
| `board_post(text, item?)` | finding text + optional item index | post a short cross-role finding teammates read via `{{.board}}`/`board_get` — `board: true` turns only |
| `wait_background_agent(job_id, timeout_secs?)` | job ID + optional wait bound (default 60s, max 600s) | the job's result once finished; a "still running" note at the timeout (never an error) — blocks the turn, so use only when genuinely dependent on the answer |
| `send_background_agent(job_id, message)` | job ID + follow-up input | ok — the input reaches a *running* job as an extra user message at its next step ("fix the green ball" without respawning); finished jobs cannot receive input |

Self-escalation (`escalate(reason)`) is covered in [docs/guides/workflows.md](workflows.md#routing). Memory tools (`record_memory`, `get_memory`, `list_memory`, `forget_memory`) and task tools (`create_task`, `update_task`, `list_tasks`, `complete_task`) are covered in [docs/guides/operations.md — Memory tools](operations.md#memory-tools). The memory tools are registered wherever milk's own tool loop runs — live turns, workflow roles, agent-as-tool peers, and background subagent forks (the last two scoped to their invoker's memory visibility) — and honor `limits.included_tools`/`limits.excluded_tools` like the rest of the built-in set.

### `spawn_background_agent` — forking yourself for background research

Unlike Agent-as-Tool below (which calls a *different*, specifically configured peer agent synchronously, blocking the caller's turn until the peer finishes, and with no access to the caller's conversation), `spawn_background_agent` by default forks the *calling* agent's own config — same model, same built-in tools — to research a self-contained question asynchronously, with its own full tool loop, without spending the caller's own context. Only available when the calling agent is backed by an inference-server provider (not `claude-cli`, which already has an equivalent fork/Task tool natively, and not subprocess agents, which don't use milk's tool loop at all). A forked job cannot itself spawn further background agents (depth is capped at 1), and its internal tool activity never appears in the *main* transcript — its final result does: it lands in the transcript the moment the job finishes (capped to this same delivery budget; `/bg show <id>` prints it in full) and is injected into the agent's next turn once drained — but is watchable live on demand by double-clicking the job in the background panel (ADR-0047; see [docs/guides/operations.md — Background sub-agents](operations.md#background-sub-agents)). When the caller has Agent-as-Tool peers (below), `spawn_background_agent` also takes an optional `agent` argument naming one of them: the job then runs on that peer — its model, tools and MCP servers — without blocking the caller. The allow-list is exactly the caller's enabled `agent_tools` entries (a name outside it is refused), the peer needs to be inference-server-backed, the job is still isolated and depth-1, and its tokens are tagged with the *spawner's* role (`primary:subagent`) against the peer's model. This is what makes a good `description` on an `agent_tools` entry matter twice: it guides both the synchronous `agent_<name>` call and the background hand-off. Concurrency is bounded per session by `max_background_agents` (default 3; see [docs/guides/operations.md](operations.md)). A job that runs long gets a **soft deadline** first ([ADR-0068](../adr/0068-background-job-soft-deadline.md)): at 80% of its execution timeout (`background_agent_timeout_minutes`) it is told through its inbox to stop exploring and write its final answer, and a job that still hits the hard timeout hands back its last written conclusions alongside the tool trail instead of a bare failure.

Restrict or extend the set per agent with `limits.included_tools` / `limits.excluded_tools` — see [docs/guides/providers.md — Per-agent limit overrides](providers.md#per-agent-limit-overrides).

**Shared-board tools (issue #197, ADR-0071)**: `board_get`, `board_claim`, `board_update`, and `board_post` exist **only** while the current turn runs a workflow definition with `board: true` — they are registered per call off the turn's context (like the task/memory backing stores) and never appear otherwise. `board_claim` is compare-and-set inside the process (first claimer wins, idempotent for the same actor), findings and statuses are attributed to the acting identity (`worker·Item-2`), and every mutation is written through to the workflow's `<checkpoint>.board.json` sidecar so resume keeps live claims. CLI-provider roles cannot receive these tools — they see the `{{.board}}` snapshot in their prompts instead.

**Job lifecycle tools (issue #250)**: `status_background_agent`, `wait_background_agent`, and `send_background_agent` complete the spawn/cancel pair. Checking on a job is cheap and unbounded; `wait_background_agent` blocks the turn (bounded by `timeout_secs`, then a non-error "still running" note) and is for work that genuinely cannot proceed without the answer — normally the completion notification is how results arrive; `send_background_agent` delivers follow-up input (a correction, a new constraint) to a *running* job, injected as an extra user message at the job's next tool-loop iteration. A job's inbox holds at most 8 pending messages; finished jobs can't receive input (spawn a new job). All three are gated exactly like spawn/cancel: only where a background manager exists, never in a job's own tool list.

**Structured result (optional)**: a job's final answer may end with a machine-readable tag on its own line, `<result status="ok|error|partial" files_touched="path1,path2"/>`, instead of leaving the caller to parse free-form prose for that information. Purely opt-in — the background system prompt mentions the convention but the job is never required to use it; when absent, the result is delivered exactly as written.

**`full_context` (optional, default `false`)**: a job is isolated by design — no session history and no injected percepts, just the task text (the memory tools are still available when the calling agent has them, so a job can look things up deliberately). Setting `full_context: true` additionally injects the spawning agent's own recent-activity summary (the same rolling, already-budget-capped summary handed to the escalation agent on hand-off) as extra orientation, for a task that genuinely depends on what the caller has been doing rather than a self-contained question. This makes the job more expensive and defeats most of the point of forking — prefer putting everything the task needs directly in `task` instead; reach for `full_context` only when that's not practical.

Claude CLI and subprocess (aider, smolagents) agents use their own native tool sets instead of this list.

### `ask_user` — asking the human mid-turn

The agent's one way to ask a clarifying question instead of proceeding on a guess when the request is ambiguous in a way that changes what to do. The question goes through the same three-source ask layer as permission prompts (ADR-0052), with free text instead of y/n: **direct input** (the TUI's prompt queue as a free-text prompt — the `a`/`d` bulk keys don't apply, any typed line is the answer — or over ACP a one-field form dialog on form-capable clients, and on clients without form dialogs a typed-chat floor: the question is announced in chat and the next message is its answer), **remote oversight** (Telegram relays the question and its options and takes free-text replies), and, in unattended contexts only, a timed **no answer** at the background-ask deadline. Silence is never an answer: only a real reply, the timed answer, or cancellation resolve the race.

- Foreground questions (ordinary turns) wait for a human; unattended ones (background jobs, workflow steps) are bounded exactly like background permission asks (`permissions.background_timeout_secs`), so a silent question can never hold a job's concurrency slot.
- ACP typed-chat floor (clients without form dialogs, issue #266): the next `session/prompt` is consumed as the answer — never routed to a model — even mid-turn (it is taken before the turn lock, so answering can't deadlock against the turn that asked). A `cancel`/`quit`/`abort` word withdraws the question unanswered; a recognized `/command` withdraws it and runs instead; synthetic follow-up prompts are never answers.
- An unanswered question comes back as guidance ("proceed on your own best judgment; don't ask again"), never as an error — the model continues instead of retrying the call.
- In non-interactive embedders (single-prompt mode, tool agents with no host ask) the tool fails with a clear "not available" error, exactly like `open_file`.
- `ask_user` is in the default `protected_tools` set (ADR-0058) — a question survives context pressure like `bash` and `read_file`.

---

## Background shells

Long-running processes (dev servers, watchers, REPLs) go through `bash` with `run_in_background: true` instead of blocking the turn ([ADR-0063](../adr/0063-background-shells.md)):

```json
{"name": "bash", "arguments": {"command": "npm run dev", "run_in_background": true}}
→ started background shell s1 — read its output with bash_output(shell_id: "s1"), stop it with kill_bash(shell_id: "s1")

{"name": "bash_output", "arguments": {"shell_id": "s1", "tail_lines": 20}}
{"name": "kill_bash", "arguments": {"shell_id": "s1"}}
```

- A shell **outlives the turn** that started it and is killed when milk exits; it is never persisted across restarts.
- Bounded: max 16 concurrent shells, auto-killed after 1 hour, output retained in a 256 KB buffer (oldest dropped). A finished shell's output stays readable for 30 minutes, then is reaped.
- `bash` also takes `workdir` for a per-command working-directory override (default: the session cwd).
- Permissions: `run_in_background` rides the normal `bash` permission ask; `bash_output` is read-only; `kill_bash` can only stop shells this agent started.

Shell rows are first-class in the background surfaces ([ADR-0064](../adr/0064-background-shell-surface.md)): the `background` panel (F3 / `/panel background`) lists them under `shells (n)` next to background agents, double-clicking a row attaches the live output view, and `/bg` handles both kinds of IDs — `/bg list` shows both tables, `/bg show <id>` prints a shell's state and current output, `/bg stop <id>` doubles as `kill_bash`. Over ACP each shell is one tool-call row (`background shell: <command>`) with live output streamed into it, `completed`/`failed`/`cancelled` at exit.


## Post-edit check

Per-agent opt-in formatter/compiler feedback right in the edit result: set `post_edit_command` on an agent entry (for example `"gofmt -l ."`, `"go vet ./..."`, `"tsc --noEmit"`) and milk runs it — in the session working directory, 60 s cap, output truncated to 4 KB — after every **successful** `edit_file`/`write_file`, appending its output and exit code to the tool result as a `[post-edit check: …]` block. Failed edits skip it. Nothing is configured by default.

```json
{
  "name": "local",
  "url": "http://localhost:8080",
  "model": "qwen2.5-coder",
  "post_edit_command": "go vet ./..."
}
```

## Agent-as-Tool

Any agent in the `agents` list can be exposed as a callable tool to any other agent via the global `agent_tools` list (or per-agent `tools` overrides). When enabled, milk synthesises an OpenAI function-schema for each peer agent and injects it alongside the built-in tools; the calling agent invokes a peer by name as it would any other tool call. The peer agent receives the caller's prompt and returns a text result fed back as a tool result — **no session state is shared** between peer calls; each call is a stateless, fresh first-turn for the peer.

### `agent_tools` field

```json
"agent_tools": [
  { "agent": "haiku-aws", "description": "Fast summarization and classification agent." },
  { "agent": "claude", "description": "Full-capability Claude Code escalation agent.", "enabled": false }
]
```

| Field | Type | Description |
|---|---|---|
| `agent` | string | Name of the agent to expose as a tool (must match a name in `agents`) |
| `description` | string | Description shown to the calling agent as the tool's purpose |
| `enabled` | bool | Whether the tool is active (default `true` when omitted) |

An agent entry's own `tools` field shadows or extends this global list for that agent specifically (same `agent` name = replace; new name = append). A cycle guard prevents an agent from calling itself as a tool. Unknown agent names are silently dropped.

Manage at runtime with `/agent tool list|enable|disable|add|remove` — see [docs/reference/spec.md — CLI Interface](../reference/spec.md#cli-interface).

### Worked example: real config using aider as a tool-agent

From a live setup where a Bearer-auth enterprise Copilot proxy is the primary agent, with `aider` available as a tool for direct file edits:

```json
{
  "agents": [
    {
      "name": "copilot-enterprise",
      "provider": "bearer",
      "url": "https://copilot-api.your-enterprise-ghe.example.com",
      "model": "claude-sonnet-4.6",
      "token_cmd": "gh auth token --hostname your-enterprise-ghe.example.com",
      "tools": [
        {
          "agent": "aider",
          "description": "aider is a coding agent that directly reads source code files and applies the requested changes"
        }
      ]
    },
    {
      "name": "aider",
      "provider": "aider-cli",
      "model": "openai/claude-sonnet-4.6",
      "url": "https://copilot-api.your-enterprise-ghe.example.com",
      "token_cmd": "gh auth token --hostname your-enterprise-ghe.example.com"
    }
  ]
}
```

### claude-cli as a tool-agent

A `claude-cli` agent can be a tool-agent too — called inline during another agent's tool loop, running the Claude CLI subprocess headlessly for each invocation.

**Requirement**: `dangerously_skip_permissions: true` is mandatory. Tool-agent calls have no interactive permission back-channel — Claude Code's `--permission-prompt-tool stdio` mode cannot be used headlessly. Without this flag, milk returns an error rather than risk a silent hang.

```json
{
  "agents": [
    {
      "name": "local",
      "url": "http://localhost:8080",
      "model": "qwen2.5-coder",
      "tools": [
        { "agent": "claude-tool", "description": "Call Claude Code for complex reasoning or code generation tasks" }
      ]
    },
    {
      "name": "claude-tool",
      "provider": "claude-cli",
      "bin": "claude",
      "dangerously_skip_permissions": true
    }
  ]
}
```

**Limitations**: no permission prompts (all tool uses auto-approved); each call is stateless (no session history); the tool-agent does not see the calling agent's session history. For testing without a live `claude` binary, point `bin` at a `milk-mock claude` wrapper — see [docs/internal/mock-setup.md](https://github.com/scoutme/milk/blob/main/docs/internal/mock-setup.md).

**Known limitation, not planned to change**: when a `claude-cli` agent (in *any* role) uses Claude Code's own Agent-tool (Task-tool) subagents or background workflows internally, milk's stream-json parser sees only their aggregated token counts (`subagent_usage`/`workflow_usage` on the final `result` event) — no content field exists there to surface, so there is no live-attach view for those, unlike `spawn_background_agent` jobs and milk's native `/workflow` engine (ADR-0047). This is a `stream-json` protocol limitation, not a milk parsing gap.

---

## MCP servers

milk connects to Model Context Protocol servers and makes their tools available to any agent assigned to them.

### `mcp_servers` field

Global list of servers; reference them from an agent entry via `"mcp_servers": ["my-server"]`.

```json
"mcp_servers": [
  { "name": "internal-docs", "url": "https://mcp.your-internal-host.example.com/api/v1/mcp", "auth": "none", "timeout": "30s", "connect_timeout": "5s" },
  { "name": "cloudflare", "url": "https://mcp.cloudflare.com/mcp", "auth": "oauth" },
  { "name": "atlassian", "url": "https://your-atlassian-mcp.example.com/mcp", "auth": "token_cmd", "token_cmd": "your-token-fetch-command" },
  { "name": "local-tool", "url": "http://localhost:3333/mcp", "enabled": true }
]
```

| Field | Type | Description |
|---|---|---|
| `name` | string | Unique identifier referenced from `AgentConfig.mcp_servers` |
| `url` | string | MCP endpoint. Required for `http` transport (Streamable HTTP with SSE fallback) |
| `transport` | string | `"http"` (default) or `"stdio"` (launches a subprocess, communicates over stdin/stdout) |
| `command` | string | Executable path. Required when `transport` is `"stdio"` |
| `args` | string[] | Command-line arguments for the stdio subprocess |
| `auth` | string | `"none"` (default), `"oauth"`, or `"token_cmd"` |
| `token_cmd` | string | Shell command whose stdout is the Bearer token (`auth: "token_cmd"`) |
| `enabled` | bool | Whether the server is active (default `true` when omitted) |

`mcp_connect_timeout_secs` (default `5`) bounds the per-server startup connect timeout. If a server doesn't respond in time, milk logs a warning and continues — the server's tools are still registered, and the client reconnects lazily on the first tool call targeting it (recorded as an `mcp.lazy_reconnect` span).

Manage at runtime with `/mcp`, or `milk config mcp add|remove|assign|unassign`.

### MCP servers with OAuth

milk has native OAuth 2.0 support — it discovers the server's OAuth metadata, registers itself as a client, and runs the Authorization Code + PKCE flow itself. No manual app registration for spec-compliant servers.

The resolved token is shared across every agent, not just the primary agent: if a server is assigned to the escalation (`claude-cli`) agent's `mcp_servers` too, the same token is forwarded into the `--mcp-config` file generated for that turn. One `/mcp auth <server>` covers every agent using it.

```json
{ "name": "my-server", "url": "https://mcp.example.com", "auth": "oauth" }
```

For servers that require a pre-registered app instead of dynamic client registration (RFC 7591):

```json
{
  "name": "my-server",
  "url": "https://mcp.example.com",
  "auth": "oauth",
  "oauth_client_id": "...",
  "oauth_client_secret": "...",
  "oauth_scopes": ["read", "write"]
}
```

`oauth_auth_timeout` (default `"5m"`) bounds how long `/mcp auth` waits for you to complete the browser flow.

**Authorizing**: the first time an agent tries to use the server, milk detects the OAuth error and prints a notice pointing at `/mcp auth <server-name>`. Run it — the TUI stays interactive while milk discovers the server's OAuth endpoints, registers a client if needed, starts a local callback listener, and prints the authorization URL:

```
[milk] starting OAuth flow for MCP server "my-server"
[milk] authorization URL: https://auth.example.com/authorize?...
[milk] opening your browser — if it doesn't open, paste the URL above into one
```

milk tries to open your browser automatically; over SSH, open the printed URL yourself. On success, reconnect with `/mcp reconnect my-server`.

**Notes**:
- Tokens are stored by milk at `~/.milk/mcp_oauth/<server-name>.json` — not by the Claude CLI or any external tool.
- Refresh is automatic (proactive before expiry, reactive on 401) using the stored refresh token. The escalation-agent path resolves the token fresh on every turn instead, since it has no persistent connection to retry.
- Locking is per-process — two milk instances against the same OAuth-protected server concurrently aren't coordinated beyond what the disk-backed token file naturally serializes.
- `"auth": "token_cmd"` servers follow the same cross-agent sharing: resolved once, forwarded to both the primary and escalation agents.

### How each agent type connects

- **`claude-cli` agents**: milk translates applicable `mcp_servers` entries into a JSON file passed via `--mcp-config` — HTTP servers become `{"type":"http","url":"...","headers":{...}}` entries, stdio servers become `{"type":"stdio","command":"...","args":[...]}`. The `claude` subprocess connects to each server directly; milk does not proxy the connection.
- **`smolagent` agents (native MCP)**: milk writes the agent's resolved server params to a temp JSON file and passes its path via `MILK_MCP_CONFIG_FILE`; the `milk-smolagent` script connects to each server itself through smolagents' native `MCPClient` (stdio and Streamable HTTP transports, auth resolved per turn via `internal/mcpauth.ResolveHeader`) and adds the resulting tools to the agent's tool list — real, directly callable MCP tool execution. A server that fails to connect is skipped with a transcript warning; milk builds no MCP `ToolSet` for these agents (no competing connection). This path applies per turn to primary, escalation and tool-agent roles alike.
- **`aider-cli` agents**: no native MCP client exists upstream, so MCP tool schemas are serialised into a text block (`internal/escalation.BuildMCPContextBlock`) and injected into the agent's context alongside built-in tool descriptions — informational only, not a wired function-calling path.

### Observability

The MCP client emits to `~/.milk/otel/`:

| Signal | Type | Description |
|---|---|---|
| `mcp.connect` | span | One per server per connect attempt; `status` is `ok` or `error` |
| `mcp.tool_call` | span | One per tool invocation; `server`, `tool`, `status` attributes |
| `mcp.lazy_reconnect` | span | Emitted on a deferred reconnect triggered by first use |
| `mcp.connect_failures` | counter | Failed connects or lazy-reconnect failures |
| `mcp.tool_calls` | counter | Total tool calls dispatched through the MCP client |

---

## Tool batch dispatch (barrier model)

When an agent emits a batch of tool calls in a single turn, milk executes it in **call order** under a barrier model ([ADR-0062](../adr/0062-tool-batch-barrier-dispatch.md)):

- **Pure reads run concurrently** — a maximal run of consecutive read-only calls (`read_file`, `list_dir`, `grep`, `find_files`, `project_map`, `get_memory`, `list_memory`, `get_session_context`, `get_context_stats`, `get_metrics`, `search_signals`, `list_tasks`, `milk_config_help`) overlaps within the run.
- **Everything else is a barrier** — writes, `bash`, `http_get`/`http_request`, memory writes, orchestration (`spawn_background_agent`, `start_workflow`, agent-as-tool, MCP tools) execute alone, in order. A `bash "go build"` in the same batch as an `edit_file` always sees the edited file.
- **A failed mutating barrier cancels the rest of the batch** — after `bash`/`write_file`/`edit_file`/`delete_file`/`move_file` fails (for bash the exit code decides: non-zero exit, launch failure or timeout — stderr on an exit-0 run is informational and fails nothing; for the others, an error result), the remaining calls are skipped with an explanatory result instead of running against state that never materialised. A failed read cancels nothing.
- **Cancellation propagates immediately** — Ctrl-C or `/stop` cancels every in-flight tool goroutine; none wait for the turn timeout.
- **Per-tool timeout** — each tool gets its own timeout; a hung tool is cancelled without affecting earlier results in the batch.
- **Result order preserved** — results are appended to history in original call order, regardless of finish order.
- **Permission checks are synchronous** — run before dispatch so the TUI presents prompts in order without interleaving.

Relative paths and shell commands resolve against the **session working directory** (`session/new`'s `cwd` over ACP), not the process working directory — the two differ whenever milk runs headless for a project it wasn't started in.

```json
{
  "agents": [
    {
      "name": "local",
      "url": "http://localhost:8080",
      "model": "qwen2.5-coder",
      "limits": { "tool_timeout_secs": 30 }
    }
  ]
}
```

| Field | Location | Default | Description |
|---|---|---|---|
| `tool_timeout_secs` | `agents[*].limits` | 120 | Per-tool timeout in seconds. `-1` for no limit. |
| `turn_timeout_secs` | `agents[*].limits` | 600 | Per-turn **idle** warning in seconds (unaffected by concurrent dispatch) — measured from the last activity event, warns once per idle stretch, never ends the turn. `-1` disables. |
| `turn_deadline_secs` | `agents[*].limits` | off (0/-1) | Opt-in turn deadline: wrap-up summary at the deadline, hard stop if the turn will not end. See [providers.md — limits](providers.md). |

---

## File and image attachments

Attach files and images to an agent turn via `/attach` or by pasting a file path.

### `/attach <path>`

```
/attach /path/to/notes.txt
/attach ~/screenshots/error.png
```

- **Text files** — contents injected as a fenced code block prepended to your prompt; both local and CLI agents receive the full contents (inlined up to 200KB, with a truncation marker beyond that).
- **Images** — base64-encoded and sent as a multipart vision payload to vision-capable local models (OpenAI `image_url` format); a Claude CLI runner gets `@temp-file` references staged at dispatch time (the binary reads `@path` natively). Over ACP the same handling is reached with standard `{"type":"image","data":"…","mimeType":"image/png"}` blocks in `session/prompt` (see [docs/reference/acp-integration.md](../reference/acp-integration.md#multimodal-input-image-pasting)).
- **PDF and other binary files** — noted as binary with a byte count; text extraction is not performed. Binary content is never dumped into the prompt as text.
- **Image resizing** — a PNG/JPEG/GIF whose long edge exceeds 2000 px or whose encoding exceeds 500 KB is shrunk once, when attached (TUI paste, `/attach`, ACP image blocks) — and likewise when an MCP tool returns an image mid-turn — so every agent and transport sees the small copy. 500 KB (~670 KB as base64) keeps one image well under backends' 5 MiB per-image limit and leaves room inside the 900 KB default request-size guard, which would otherwise drop conversation history on every request of an image turn. Token estimates count an image at a flat ~4K tokens, never by its base64 length. Area-averaging downscale; JPEG EXIF orientation is applied before the (EXIF-dropping) re-encode; flat screenshots and images with transparency stay PNG, photos become JPEG, and an image already within the limits is passed through byte-identical. A `[milk] image … resized W×H → W×H, A MB → B MB (format)` line reports what changed. WebP/BMP/undecodable files and images over 50 megapixels are sent as-is.
- **Size and detection** — one attachment is capped at 32MB (rejected with a message beyond that). Content is sniffed by magic bytes first (a text file named `foo.png` is text); SVG inlines as text rather than riding the vision path.

The status bar shows `[N attached]` while pending. Attachments clear after the turn completes.

### File path detection in paste

Pasting text that looks like an existing absolute path (starting with `/` or `~/`) prompts:

```
[milk] pasted path "/etc/hostname" — attach as file? [y/N]
```

`y`/Enter attaches it; `n`/Escape/anything else inserts it as plain text. Non-existent paths are inserted as plain text without prompting.

### Session history

Attachment data is never stored verbatim. The session records a compact placeholder like `[attached: filename.png]` next to the prompt (on every path, including a self-escalation); file contents are sent on the turn they're attached, not persisted beyond that.

### Supported types

| Extension | MIME type | Handling |
|---|---|---|
| `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.bmp`, `.tiff` | `image/*` | Multipart vision payload |
| `.svg` | `image/svg+xml` | Fenced text block (most vision endpoints accept only raster formats) |
| `.txt`, `.md`, `.log` | `text/plain` | Fenced block in prompt |
| `.go`, `.py`, `.js`, `.ts`, `.sh`, … | `text/x-*` | Fenced block in prompt |
| `.json`, `.yaml`, `.toml`, `.html`, `.css` | Various text | Fenced block in prompt |
| `.pdf` | `application/pdf` | Binary notice only |
| Other | `text/plain` (default) | Fenced block in prompt |

---

## Machine-readable output

milk can present itself to machines two ways: a long-lived ACP v2 agent
server for editor embedding (`milk serve --acp`), and a one-shot batch event
stream for CI/scripts (`--output-format stream-json|json`). Both are **real,
shipped, and narrower than the full design** — the wire contract's full
target shape is ratified in [ADR-0049](../adr/0049-machine-readable-wire-contract.md)
and [ADR-0050](../adr/0050-batch-stream-json-contract.md), and the complete
normative catalog lives in [docs/internal/design/machine-readable-output-design.md](https://github.com/scoutme/milk/blob/main/docs/internal/design/machine-readable-output-design.md)
— but what's actually implemented today is a deliberately scoped subset of
that catalog, documented precisely (not aspirationally) in two places:

- **[docs/reference/acp-integration.md](../reference/acp-integration.md)** — `milk serve --acp`:
  which of the 4 real methods work, which of the rest return a plain
  JSON-RPC "method not found," the permission-handling caveat (local-provider
  agents only), and a worked example exchange. Read this, not the design
  doc, if you're integrating an editor against milk right now.
- **Batch mode, below** — `--output-format stream-json|json`.

```
milk serve --acp                                  # ACP v2 agent server on stdio (editor embedding)
milk [flags] "prompt" --output-format stream-json  # batch event stream (CI/scripts/log pipelines)
milk [flags] "prompt" --output-format json         # single terminal-result document
milk [flags] "prompt"                              # --output-format text (default, unchanged)
```

- **`--output-format text`** — current behavior byte-for-byte, unchanged.
  Interactive TUI mode ignores `--output-format` entirely.
- **`--output-format stream-json`** — exactly **three** lines on stdout per
  run, in order: one `system`/`init` event, one completed `assistant`
  message (the whole response text — **not** token-level deltas; there is no
  `stream_event`/partial-message emission in batch mode today), and one
  terminal `result` event. `--output-format json` is that same `result`
  event alone, as a single `json.MarshalIndent`-ed document.
- Nothing else in the full §6 event catalog is emitted in batch mode: no
  `stream_event` partial deltas, no `tool_use`/`tool_result` events (tool
  calls happen, just not reported on this wire — see the eval harness's
  `tool_calls: 0` caveat in [docs/reference/eval.md](../reference/eval.md) if you rely on this),
  no `system` subtypes beyond `init` (no `agent_switch`, `route`,
  `notification`, `warning`, `state`, `task_*`, `memory`, `commands`,
  `config_option`, `permission_denied`, `error`). `system/init` itself omits
  `tools` and `mcp_servers` — nothing at the CLI boundary enumerates them
  cheaply yet.
- `--permission-mode`/`--allow-tool`/`--no-partial-messages` flags described
  in the design doc **do not exist** on the CLI — batch mode has no
  permission-prompt wiring at all today; a tool needing interactive
  confirmation is handled per the agent's own existing default (see
  [docs/reference/acp-integration.md](../reference/acp-integration.md)'s permission caveat, which
  applies here too).
- What *is* locked and accurate: **snake_case fields** throughout
  (`input_tokens`, `cache_read`, `is_error`, `session_id`), a `type`
  discriminator per line (`subtype` for the `system`/`result` families, all
  open-set), **stdout = events only, stderr = prose** (`[milk]` warnings
  stay on stderr, never mirrored into a stdout event yet), exactly one
  terminal `result` per run, and no ANSI escapes ever on this wire. The
  machine-checkable schema for the three real line shapes is
  [`docs/schema/stream-json.schema.json`](../schema/stream-json.schema.json)
  (golden fixtures under `internal/transport/streamjson/testdata/`).
