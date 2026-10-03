# Integrating with milk via ACP

`milk serve --acp` runs milk as a long-lived [Agent Client
Protocol](https://agentclientprotocol.com) v2 agent: an editor (or any other
client) spawns it as a subprocess and speaks JSON-RPC 2.0 over its
stdin/stdout. This doc describes **exactly what's implemented today** —
separately from [docs/machine-readable-output-design.md](machine-readable-output-design.md),
which is the full design proposal covering the complete planned surface, most
of which isn't built yet. If you're integrating an editor or client against
milk right now, this page — not the design doc — is the contract to code
against. Where the two disagree, this page wins; the design doc's own status
notes track what's landed phase by phase.

## Launching it

```
milk serve --acp
```

- Speaks newline-delimited JSON-RPC 2.0: one JSON object per line, on both
  stdin (client → agent) and stdout (agent → client).
- **stdout carries protocol traffic only.** Human-readable warnings (e.g. a
  stale config recovery notice) go to stderr, never stdout — never try to
  parse stdout as anything but JSON-RPC lines.
- The process keeps running until stdin closes or it receives `SIGINT`/`SIGTERM`.
- One process serves **multiple sessions**: `session/new` can be called more
  than once, and each session's `session/prompt` call runs independently —
  a long-running turn in one session never blocks another session's calls.

## What's implemented

| Method | Direction | Status |
|---|---|---|
| `initialize` | client → agent | ✅ real |
| `session/new` | client → agent | ✅ real |
| `session/prompt` | client → agent | ✅ real |
| `session/cancel` | client → agent (notification) | ✅ real |
| `session/update` (`state_update`) | agent → client | ✅ real: `running` at turn start, `idle` at turn end |
| `session/update` (`agent_message_chunk`) | agent → client | ✅ real, but **one per completed turn, not per token** — see caveat below |
| `session/update` (`tool_call_update`) | agent → client | ✅ real, for both the local-provider and claude-cli-escalation paths |
| `session/request_permission` | agent → client | ✅ real, but **local-provider agents only** — see caveat below |

Everything else a real ACP client might try — `session/list`, `session/resume`,
`session/delete`, `session/close`, `auth/login`, `auth/logout`,
`session/set_config_option`, `elicitation/create`, `$/cancel_request` as a
standalone per-request cancel — returns the standard JSON-RPC **`-32601`
method not found** error. That's a deliberate, documented gap, not a bug: see
[Deferred](#deferred-not-silently-missing) below.

### Caveat: `agent_message_chunk` is not token-level streaming

ACP's naming suggests incremental delivery, but milk's underlying callback
(`OnResponseSegment`) only fires once per tool-call boundary and once more
for the final text — for the common case of a text-only turn with no tool
calls, that's exactly **one** `agent_message_chunk` carrying the whole
response, not a token stream. A client expecting live token-by-token
rendering will instead see the full text arrive in one notification right
before the `idle` `state_update`. Real partial-token streaming would need a
new, lower-level hook in `internal/agent/local`/`internal/agent/claude` that
doesn't exist yet.

### Caveat: permission prompts only cover local-provider agents

If the session's active agent (primary or, after routing, escalation) is a
local OpenAI-compatible/Bedrock provider, a tool call needing confirmation
sends a real `session/request_permission` request and blocks for your
response. If the active agent is `claude-cli` (milk's default escalation
agent), **no permission request is ever sent** — every tool call it attempts
is silently denied by `claude`'s own default behavior, visible only in its
response text. claude-cli speaks its own separate control-request wire
protocol for permissions; bridging that through ACP is unbuilt. If you need
an escalation agent that can actually use tools under ACP today, configure a
local-provider agent as `escalation_agent` instead of the `claude-cli` default
(see [docs/providers.md](providers.md)).

### Capability negotiation note

`initialize`'s response advertises `capabilities.session: {}`. Per the
upstream ACP schema, supplying *any* value for `session` (even `{}`) declares
support for the **entire** baseline — `session/new`, `session/list`,
`session/resume`, `session/close`, `session/prompt`, `session/cancel`, and
`session/update` — as one bundle; there's no finer-grained flag to advertise
only the subset milk actually implements. milk advertises the baseline
because that's the only way to turn on the four methods it does support;
calling one of the three it doesn't gets `-32601`, which is the correct way
a client should discover the gap (check the response's `error.code`, don't
assume success from the capability flag alone).

## Example exchange

A single-turn conversation, request/response pairs annotated (actual traffic
is newline-delimited, one JSON object per line — formatted here for
readability):

```jsonc
// → client sends
{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":2,"info":{"name":"my-editor"}}}

// ← agent responds
{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":2,"info":{"name":"milk","version":"0.9.0"},"capabilities":{"session":{}}}}

// → client sends
{"jsonrpc":"2.0","id":2,"method":"session/new","params":{"cwd":"/home/me/myproject"}}

// ← agent responds
{"jsonrpc":"2.0","id":2,"result":{"sessionId":"a1b2c3d4-..."}}

// → client sends
{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"a1b2c3d4-...","prompt":[{"type":"text","text":"what does go.mod declare as the module name?"}]}}

// ← agent sends notifications WHILE the request above is still pending —
//   the response to id 3 only arrives after the final one of these:
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"state_update","state":"running"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu_1","name":"read_file","kind":"read","status":"in_progress","rawInput":{"path":"go.mod"}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"tool_call_update","toolCallId":"toolu_1","name":"read_file","kind":"read","status":"completed","rawOutput":"module example.com/t\n\ngo 1.26\n"}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"agent_message_chunk","messageId":"msg-1","content":{"type":"text","text":"go.mod declares module example.com/t."}}}}
{"jsonrpc":"2.0","method":"session/update","params":{"sessionId":"a1b2c3d4-...","update":{"sessionUpdate":"state_update","state":"idle","stopReason":"end_turn"}}}

// ← agent responds (only now — session/prompt was held open for the whole turn)
{"jsonrpc":"2.0","id":3,"result":{"messageId":"msg-1"}}
```

Trying an unimplemented method:

```jsonc
// → client sends
{"jsonrpc":"2.0","id":4,"method":"session/list","params":{}}

// ← agent responds
{"jsonrpc":"2.0","id":4,"error":{"code":-32601,"message":"method not found: session/list"}}
```

## Error handling

- **`-32601`** (JSON-RPC reserved "method not found"): the method isn't
  implemented. This is the only error code milk's dispatcher assigns
  deliberately; every other failure (bad params, unknown `sessionId`, a
  turn-level error) comes back as a generic `-32000` with a human-readable
  `message` — don't pattern-match on its text, just surface it.
- `session/cancel` is a **notification** (no `id`, no response). Sending one
  for a session with no turn currently running is a safe no-op.
- A malformed JSON-RPC line on stdin is silently dropped, not reported —
  there's no response channel for a line that failed to parse into even a
  method name.

## Known gaps vs. the full design (deferred, not silently missing)

Tracked explicitly in `docs/machine-readable-output-design.md`'s Phase 2
status note, repeated here for integrators who don't want to read the whole
design doc:

- `session/list`, `session/resume`, `session/delete`, `session/close`
- `auth/login`, `auth/logout`
- `session/set_config_option` (the internal handler exists, isn't wired to
  the dispatcher)
- `elicitation/create` — `Elicit` always returns a cancelled result; nothing
  will ever actually prompt the client with a form
- `plan_update` (workflow progress), `terminal_update` (PTY/agent-owned
  terminal streaming)
- The `milk/notification`, `milk/warning`, `milk/memory`, `milk/route`
  extension channels — never emitted
- `available_commands_update` — milk's slash commands aren't surfaced to an
  ACP client
- `NewSessionRequest.mcpServers` — accepted and parsed, never merged with
  the agent's own configured MCP servers
- `cwd` isn't validated as an absolute path (the ACP spec says it should be),
  only checked for non-empty

## For implementers extending this

- `internal/transport/acp` — wire shapes (`lifecycle.go` for the inbound
  request/response structs, `acp.go`/`host.go`/`map.go`/`plan.go`/
  `terminal.go`/`commands.go`/`ext.go` for the rest of the ACP v2 vocabulary,
  most of it already defined but unwired) and the JSON-RPC transport
  (`stdio.go`'s `StdioConn` — generic, no milk-specific knowledge, testable
  via `io.Pipe`).
- `cmd/milk/serve.go` — the `serve --acp` cobra command.
- `cmd/milk/acp_server.go` — method dispatch (`initialize`/`session/new`/
  `session/prompt`/`session/cancel`); add a new `case` here plus the
  matching struct in `lifecycle.go` to wire up another method.
- `cmd/milk/acp_session.go` — per-session state and the actual turn dispatch,
  structurally mirroring `cmd/milk/repl.go`'s `buildTUIAgents` (TUI callbacks
  → `tea.Msg`; this file's callbacks → `conn.Notify`/ACP `Mapper` calls).
- `cmd/milk/host_acp.go` — the `events.Host` adapter bridging milk's
  host-agnostic permission/elicitation interface onto ACP's wire shapes, for
  local-provider agents.
- Tests: `internal/transport/acp/stdio_test.go` (transport-layer, race-clean,
  no subprocess), `cmd/milk/acp_server_test.go` (direct handler tests, fake
  `Conn`, no subprocess), `cmd/milk/serve_acp_e2e_test.go` (real compiled
  binary, real subprocess — the closest thing to a live integration test
  available without a real ACP client in this environment).
