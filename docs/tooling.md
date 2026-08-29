# Tooling, MCP, and tool-agents

milk gives any agent — primary or escalation, on any provider — three ways to act beyond generating text: built-in tools, other configured agents exposed as callable tools, and MCP servers.

---

## Built-in tools

The primary agent (HTTP or Bedrock backends) has these tools available with no configuration:

| Tool | Parameters | Returns |
|---|---|---|
| `bash(command)` | `command string` | stdout, stderr, exit_code |
| `find_files(path, pattern)` | root dir + glob (e.g. `*_test.go`) | matching file paths — locate by filename; use `grep` for contents |
| `grep(pattern, path, recursive)` | pattern/path/bool | matches — searches file contents; use `find_files` for filenames |
| `read_file(path, offset, limit)` | path + optional range | content |
| `write_file(path, content)` | — | ok — creates parent directories; expands `~` |
| `edit_file(path, old_string, new_string, replace_all)` | — | ok — exact-string replacement; rejects ambiguous matches unless `replace_all=true` |
| `delete_file(path)` | — | ok — permission-gated |
| `move_file(source, destination)` | — | ok — permission-gated; creates destination parent directories |
| `list_dir(path)` | — | entries (names, types, sizes) |
| `http_get(url, max_bytes)` | — | body — bounded GET |
| `http_request(method, url, headers, body, max_bytes)` | — | body, status — permission-gated |
| `get_session_context()` | — | full shared session history (both agents), so the primary model can see prior escalation turns |
| `get_context_stats()` | — | current history turn counts and total character size, so the agent can self-regulate before hitting context limits |
| `open_file(path)` | — | ok — opens the file in the configured editor |
| `current_need(goal)` | one-sentence goal | ok — same effect as the user typing `/need <goal>` |
| `export_session(format, output_path)` | `"text"`\|`"json"`, optional file path | transcript inline, or written to `output_path` |
| `milk_config_help(topic)` | e.g. `"mcp add"`, `"agent add"` | a section of milk's own embedded reference docs — lets the agent look up how to manage milk's own config instead of guessing the schema; omit `topic` to list what's available. Backed by `internal/selfdocs`, which indexes `docs/spec.md`/`providers.md`/`workflows.md`/`tooling.md`/`operations.md` by heading — same content this site is built from |

Self-escalation (`escalate(reason)`) is covered in [docs/workflows.md](workflows.md#routing). Memory tools (`get_memory`, `list_memory`, `forget_memory`) and task tools (`create_task`, `update_task`, `list_tasks`, `complete_task`) are covered in [docs/operations.md](operations.md).

Restrict or extend the set per agent with `limits.included_tools` / `limits.excluded_tools` — see [docs/providers.md — Per-agent limit overrides](providers.md#per-agent-limit-overrides).

Claude CLI and subprocess (aider, smolagents) agents use their own native tool sets instead of this list.

---

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

Manage at runtime with `/agent tool list|enable|disable|add|remove` — see [docs/spec.md — CLI Interface](spec.md#cli-interface).

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

**Limitations**: no permission prompts (all tool uses auto-approved); each call is stateless (no session history); the tool-agent does not see the calling agent's session history. For testing without a live `claude` binary, point `bin` at a `milk-mock claude` wrapper — see [docs/mock-setup.md](mock-setup.md).

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
- **`aider-cli` and `subprocess` agents** (aider, smolagents): these do not receive a generated MCP config file. Instead, MCP tool schemas are serialised into a text block and injected into the agent's context alongside built-in tool descriptions — informational only, not a wired function-calling path. `aider` has no native MCP client upstream. `smolagents` does have one (`MCPClient`/`ToolCollection.from_mcp`) that milk's adapter script does not yet use — a possible follow-up, not yet scheduled.

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

## Concurrent tool dispatch

When an agent emits a batch of tool calls in a single turn, all tools in the batch run **concurrently**, each in its own goroutine:

- **Cancellation propagates immediately** — Ctrl-C or `/stop` cancels every in-flight tool goroutine; none wait for the turn timeout.
- **Per-tool timeout** — each tool gets its own timeout; a hung tool is cancelled without affecting the rest of the batch.
- **Result order preserved** — results are appended to history in original call order, regardless of finish order.
- **Permission checks are synchronous** — run before dispatch so the TUI presents prompts in order without interleaving.

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
| `turn_timeout_secs` | `agents[*].limits` | 600 | Per-turn timeout (unaffected by concurrent dispatch). |

---

## File and image attachments

Attach files and images to an agent turn via `/attach` or by pasting a file path.

### `/attach <path>`

```
/attach /path/to/notes.txt
/attach ~/screenshots/error.png
```

- **Text files** — contents injected as a fenced code block prepended to your prompt; both local and CLI agents receive the full contents.
- **Images** — base64-encoded and sent as a multipart vision payload to vision-capable local models (OpenAI `image_url` format); for the Claude CLI path, a data URI is injected as a context block.
- **PDF and other binary files** — noted as binary with a byte count; text extraction is not performed.

The status bar shows `[N attached]` while pending. Attachments clear after the turn completes.

### File path detection in paste

Pasting text that looks like an existing absolute path (starting with `/` or `~/`) prompts:

```
[milk] pasted path "/etc/hostname" — attach as file? [y/N]
```

`y`/Enter attaches it; `n`/Escape/anything else inserts it as plain text. Non-existent paths are inserted as plain text without prompting.

### Session history

Attachment data is never stored verbatim. The session records a compact placeholder like `[attached: filename.png]` next to the prompt; file contents are sent on the turn they're attached, not persisted beyond that.

### Supported types

| Extension | MIME type | Handling |
|---|---|---|
| `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp` | `image/*` | Multipart vision payload |
| `.txt`, `.md`, `.log` | `text/plain` | Fenced block in prompt |
| `.go`, `.py`, `.js`, `.ts`, `.sh`, … | `text/x-*` | Fenced block in prompt |
| `.json`, `.yaml`, `.toml`, `.html`, `.css` | Various text | Fenced block in prompt |
| `.pdf` | `application/pdf` | Binary notice only |
| Other | `text/plain` (default) | Fenced block in prompt |
