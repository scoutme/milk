# 36. MCP stdio Subprocess Transport

**Status:** Accepted

## Context

milk's MCP client (`internal/mcp/`) previously supported only Streamable HTTP transport (MCP spec 2025-03-26). Many useful MCP servers — local tools, language-specific helpers, project-specific scripts — are designed as stdio servers: a subprocess that reads JSON-RPC requests from stdin and writes responses to stdout.

The config already had a `transport` field with `"stdio"` documented as "reserved for future use" and `Command`/`Args` fields were absent.

## Decision

Add a stdio subprocess transport path to the existing `Client`. Key choices:

**Process lifetime via `context.Background()`**
The subprocess is started with `exec.CommandContext(context.Background(), ...)`, not the connect-timeout context passed to `Connect()`. This is deliberate: the connect context may be as short as 5 seconds (the default `connect_timeout`). Tying the subprocess to it would kill the process as soon as the handshake context expires, causing every subsequent tool call to fail with a broken pipe. The subprocess must live for the lifetime of the milk session; `Close()` kills it explicitly.

**`stdioMu` serializes the channel**
Unlike HTTP (where each request is an independent connection), a stdio process has a single stdin/stdout channel. Concurrent `roundtripStdio` calls would interleave writes and reads unpredictably. `stdioMu` is a separate mutex from `mu` (which guards `ready`, `tools`, `deadUntil`) so that normal status checks do not block in-flight tool calls.

**Response matching by ID**
The subprocess may emit server-initiated notifications before the response to a given request. `roundtripStdio` reads lines in a loop, decoding each, and skips any message whose `id` doesn't match the outstanding request. This is consistent with how `readSSEResult` handles HTTP+SSE.

**No session ID**
The MCP session ID concept exists only in the HTTP transport (carried in the `Mcp-Session-Id` header). stdio servers maintain implicit session state through the persistent process; no session ID exchange is needed or expected.

**`writeMCPConfigFile` for claude-cli**
For `claude-cli` agents, MCP servers are passed via `--mcp-config`. Stdio servers are now serialized as `{"type":"stdio","command":"...","args":[...]}` entries, which Claude Code CLI accepts natively.

**Wizard and inline `/mcp add`**
The `/mcp add` wizard was extended with a `transport` step (defaults to `http`) and a `command` step (shown only when transport is `stdio`, replacing the `url` step). Inline `key=val` syntax supports `transport=stdio command=<path> args=<arg1,arg2>`.

## Consequences

- Stdio MCP servers (local binaries, scripts) can be registered and assigned to agents exactly like HTTP servers.
- The subprocess is a long-lived child process of the milk TUI; it is killed when `Close()` is called at session end.
- If the subprocess exits unexpectedly mid-session, the next tool call will get a broken pipe error. The existing lazy-reconnect + backoff mechanism (`deadUntil`) will suppress repeated reconnect attempts.
- `MCPServerConfig.URL` is now `omitempty` in JSON so stdio entries don't emit a spurious `"url":""` field.
