# milk-mock — Scriptable Provider Mock

`milk-mock` is a test-double binary that lets you run milk integration tests
without a live inference server. It ships two subcommands:

| Subcommand | Replaces |
|---|---|
| `milk-mock server` | Any local OpenAI-compatible inference server |
| `milk-mock claude` | The `claude` CLI subprocess (`provider: "claude-cli"`) |

## Build

```sh
go build -o ./milk-mock ./cmd/milk-mock
# or via Taskfile:
task build:mock
```

## Subcommand: `milk-mock server`

Starts an HTTP server that speaks the OpenAI chat completions protocol.

```sh
milk-mock server --port 19999 --name mytest --rec-dir /tmp/mock-recordings
# or:
task mock:server
```

### Flags

| Flag | Default | Description |
|---|---|---|
| `--port` | `19999` | Port to listen on |
| `--name` | `mock` | Name prefix for recording files |
| `--rec-dir` | `~/.milk/mock-recordings` | Directory for NDJSON recording files |

### Response behaviour

- **Text request**: returns `response for: <input>, context received: <context_summary>`
- **Tool-call request**: if the user message contains `[tool:<name>,<args>]`, the server emits
  a tool-call response with `finish_reason: "tool_calls"`.

### Wire into milk config

```json
{
  "agents": [
    {
      "name": "mock-local",
      "provider": "local",
      "url": "http://localhost:19999",
      "model": "mock-model"
    }
  ]
}
```

## Subcommand: `milk-mock claude`

Drop-in replacement for `claude --print --output-format stream-json`. Reads a
prompt (from the `--` argument or stdin), emits NDJSON stream events, and records
the turn.

```sh
milk-mock claude -- "hello world"
```

### Flags (mirrors claude CLI)

`--session-id`, `--resume`, `--output-format`, `--print`,
`--dangerously-skip-permissions`, `--allowedTools`, `--add-dir`,
`--append-system-prompt-file`, `--mcp-config`, `--verbose`,
`--include-partial-messages`

All flags are accepted; most are recorded but not enforced.

### Wire into milk config

```json
{
  "agents": [
    {
      "name": "mock-claude",
      "provider": "claude-cli",
      "bin": "/path/to/milk-mock"
    }
  ],
  "escalation_agent": "mock-claude"
}
```

The `bin` field must point to the `milk-mock` binary. The `claude` subcommand is
automatically selected when milk calls `milk-mock --print --output-format stream-json`.

> **Note**: milk calls `<bin> --print --output-format stream-json ...`, so it passes
> the flags directly to the `milk-mock` root command, not to a subcommand. The
> `claudeCmd` handles this by recognising the `--print` / `--output-format` flags on
> the root command level via `milk-mock claude ...`.
>
> To use it as a drop-in, create a shell wrapper:
> ```sh
> #!/bin/sh
> exec /path/to/milk-mock claude "$@"
> ```

## Tool-call syntax: `[tool:<name>,<args>]`

Include this anywhere in the user prompt to trigger a tool-call response:

```
[tool:bash,{"command":"echo hello"}]
[tool:read_file,/path/to/file.txt]
```

Multiple tool calls in a single prompt emit multiple tool-call objects.

## Recording file layout

For a recorder named `test`, two NDJSON files are written:

```
~/.milk/mock-recordings/
  test-sessions.jsonl    # one line per turn
  test-metrics.jsonl     # one line per session
```

### `test-sessions.jsonl` schema

```json
{"turn":1,"input":"hello","context_summary":"...","tool_calls":[],"response":"response for: hello","timestamp_ms":1234567890}
```

### `test-metrics.jsonl` schema

```json
{"session_id":"mock-xyz","turns":3,"approx_tokens":120,"duration_ms":450}
```

## Taskfile tasks

| Task | Description |
|---|---|
| `task build:mock` | Build `./milk-mock` |
| `task mock:server` | Start mock server on port 19999 |
| `task mock:claude` | Show help for mock claude subcommand |
| `task test:integration` | Run integration tests (requires `./milk-mock`) |
