# milk-mock — design

## Purpose

A scriptable provider mock for integration testing and debugging milk without a live inference server. It speaks each supported wire format faithfully, exercises milk's routing, context injection, and tool-dispatch pipeline, and records everything it sees to disk.

---

## Modes

`milk-mock` is a single binary with two modes, matching milk's two provider families:

| Mode | Invocation | Wire format | How milk connects |
|---|---|---|---|
| `server` | `milk-mock server --name <n> --provider openai\|bedrock --addr :port` | HTTP | `url` in `agents` config |
| `claude` | `milk-mock claude [flags] "prompt"` | NDJSON subprocess | `bin` in `agents` config |

**`server` mode** starts an HTTP listener. milk points its `url` at it and talks to it like a real inference server.

**`claude` mode** is invoked by milk as if it were the `claude` binary. It reads the same CLI flags, writes the same NDJSON event stream to stdout, and (optionally) reads permission responses from stdin.

---

## Tool call syntax

To trigger a tool call from inside a prompt, embed one or more directives:

```
[tool:<name>,<json_args>]
```

Examples:

```
[tool:bash,{"command":"ls /tmp"}]
[tool:read_file,{"path":"/etc/hosts"}]
[tool:bash,{"command":"pwd"}] [tool:bash,{"command":"whoami"}]
```

### Dispatch model

**`server` mode (OpenAI-compat):** The mock emits a `tool_calls` response. Milk dispatches the tool and sends the result back in a follow-up request. The mock then emits its normal text response. This exercises milk's full tool-dispatch pipeline.

**`claude` mode:** The mock runs tools directly (as the real claude binary would) and incorporates results into the assistant turn. Optionally emits a `control_request` permission prompt before running.

---

## Response template

When no tool directives are present, or after tool results are incorporated, the mock responds with:

```
response for: <last user message>, context received: <system context summary>
```

The context summary is the first 200 chars of the system prompt (or `--append-system-prompt` file content), trimmed to the first newline.

---

## Wire formats

### OpenAI-compatible (`--provider openai`)

**Endpoint:** `POST /<chat_path>` (default `/v1/chat/completions`)

**Request:** standard `chatRequest` JSON (model, messages, tools, stream=true)

**Response (no tool call):** SSE stream, one token per chunk:
```
data: {"choices":[{"delta":{"content":"response for: "},"finish_reason":null}]}
data: {"choices":[{"delta":{"content":"hello"},"finish_reason":null}]}
…
data: {"choices":[{"delta":{},"finish_reason":"stop"}]}
data: [DONE]
```

**Response (tool call requested):** Emit the tool_call delta first, then `[DONE]`. After milk re-sends the request with the tool result, emit the text response.

```
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"mock_1","type":"function","function":{"name":"bash","arguments":""}}]},"finish_reason":null}]}
data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\":\"ls /tmp\"}"}}]},"finish_reason":null}]}
data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}
data: [DONE]
```

**Non-streaming** (`stream: false`, used for classification): return the full response in a single JSON object.

### Bedrock Converse (`--provider bedrock`)

The Bedrock path uses AWS Event Stream binary framing, which is complex to produce correctly. The Bedrock mock is deferred to a follow-on iteration. For now, point Bedrock agents at an OpenAI-compat mock by wrapping the request translation, or use `--provider openai` with a URL override.

### Claude CLI (`claude` mode)

The mock binary is placed at the path configured in `bin` (e.g. `/usr/local/bin/milk-mock-claude`). It accepts the same flags as the real `claude` binary:

```
milk-mock claude --print --output-format stream-json --verbose
  --include-partial-messages
  [--dangerously-skip-permissions | --permission-prompt-tool stdio]
  [--allowedTools t1,t2]
  [--session-id <uuid>]
  [--resume <uuid>]
  [--append-system-prompt-file <path>]
  "prompt"
```

**stdout event sequence (normal turn):**
```json
{"type":"system","session_id":"<generated-uuid>"}
{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"response for: "}}}
…
{"type":"stream_event","event":{"type":"content_block_stop","index":0}}
{"type":"stream_event","event":{"type":"message_stop"}}
{"type":"assistant","message":{"content":[{"type":"text","text":"<full response>"}]}}
{"type":"result","session_id":"<uuid>","is_error":false,"permission_denials":[]}
```

**stdout event sequence (tool call turn):**
```json
{"type":"stream_event","event":{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"mock_tu_1","name":"bash"}}}
{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"command\":\"ls /tmp\"}"}}}
{"type":"stream_event","event":{"type":"content_block_stop","index":0}}
{"type":"assistant","message":{"content":[{"type":"tool_use","id":"mock_tu_1","name":"bash","input":{"command":"ls /tmp"}}]}}
```

When `--permission-prompt-tool stdio` is set, emit a `control_request` before running the tool and wait for milk's `control_response` on stdin.

**Session continuity (`--resume`):** The mock loads the prior session file and includes prior turns in the context summary.

---

## Recording

All output files use the mock's `--name` value as prefix and are written to `--output-dir` (default `~/.milk/mock/`).

### `<name>-sessions.jsonl`

One record appended per turn:

```json
{
  "ts": "2026-06-01T10:00:00Z",
  "session_id": "uuid",
  "turn": 3,
  "mode": "openai",
  "input": "last user message text",
  "context": "first 500 chars of system prompt",
  "tool_calls": [{"name": "bash", "args": {"command": "ls /tmp"}, "result": "file1\nfile2"}],
  "response": "response for: ..., context received: ..."
}
```

### `<name>-metrics.jsonl`

One record appended per session (on process exit / graceful shutdown):

```json
{
  "ts": "2026-06-01T10:01:00Z",
  "session_id": "uuid",
  "mode": "openai",
  "turns": 5,
  "tool_calls_total": 3,
  "prompt_tokens_total": 1240,
  "completion_tokens_total": 380,
  "total_tokens_total": 1620,
  "duration_ms": 4200
}
```

**Token counting:** approximate — `len(text) / 4` (4 chars ≈ 1 token). This matches the ballpark of common subword tokenizers without requiring a real tokenizer dependency.

---

## Configuration

All settings are flags; no config file. This makes `milk-mock` self-contained and easy to wire into `config.json` agents entries.

**Common flags:**

| Flag | Default | Description |
|---|---|---|
| `--name` | `"mock"` | Used for output filenames and response text |
| `--output-dir` | `~/.milk/mock/` | Directory for session and metrics files |
| `--latency-ms` | `0` | Artificial per-token delay in ms (simulate slow models) |
| `--fail-rate` | `0.0` | Fraction of requests to fail with 500 (chaos testing) |

**Server-mode flags:**

| Flag | Default | Description |
|---|---|---|
| `--addr` | `:19999` | Listen address |
| `--provider` | `openai` | Wire format: `openai` (Bedrock: future) |
| `--chat-path` | `/v1/chat/completions` | Override endpoint path |

**Claude-mode flags:** passed through from CLI args directly (same as real `claude` binary).

---

## Integration with milk config

**OpenAI-compat mock:**

```json
{
  "agents": [{
    "name": "mock",
    "url": "http://localhost:19999",
    "model": "mock-model",
    "provider": "local"
  }]
}
```

Run:
```
milk-mock server --name mock --provider openai --addr :19999
```

**Claude-cli mock:**

```json
{
  "agents": [{
    "name": "mock-claude",
    "provider": "claude-cli",
    "bin": "/path/to/milk-mock"
  }],
  "escalation_agent": "mock-claude"
}
```

Run (no daemon needed — milk spawns it per turn):
```
ln -s $(which milk-mock) /usr/local/bin/milk-mock-claude
```

Or use `bin: "milk-mock"` and rely on PATH (milk-mock detects it was invoked as `claude` via `os.Args[0]` or the first positional arg being a prompt).

---

## Non-goals

- Real tokenization (approximation is sufficient for testing)
- Bedrock binary event stream (deferred)
- Multi-user / concurrent session isolation (single-user test tool)
- Prompt injection / adversarial behaviour simulation (out of scope)
