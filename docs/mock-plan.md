# milk-mock — implementation plan

Design reference: [mock-design.md](mock-design.md)

---

## Phase 1 — Core structure

**Branch:** `feat/mock-provider`

### Step 1.1 — Binary skeleton

Create `cmd/milk-mock/main.go` with two subcommands via Cobra:

```
milk-mock server   --name --addr --provider --chat-path --output-dir --latency-ms --fail-rate
milk-mock claude   (all real claude flags; prompt as final positional arg)
```

Mode detection: `milk-mock server` is explicit. `milk-mock claude` is also explicit. Optionally, when invoked as a symlink named `claude` or `milk-mock-claude`, auto-select claude mode.

**Files to create:**
- `cmd/milk-mock/main.go` — Cobra root + mode dispatch
- `cmd/milk-mock/server.go` — server subcommand
- `cmd/milk-mock/claudemode.go` — claude subcommand

### Step 1.2 — Shared recorder

Create `internal/mockrecorder/recorder.go`:

```go
type Recorder struct {
    Name      string
    OutputDir string
}

func (r *Recorder) RecordTurn(turn TurnRecord) error     // appends to <name>-sessions.jsonl
func (r *Recorder) RecordMetrics(m MetricsRecord) error  // appends to <name>-metrics.jsonl
```

`TurnRecord` and `MetricsRecord` match the JSON shapes in mock-design.md.

Token approximation: `func approxTokens(s string) int { return (len(s) + 3) / 4 }`

**Files to create:**
- `internal/mockrecorder/recorder.go`

---

## Phase 2 — OpenAI-compat server

### Step 2.1 — Request parsing

Reuse the existing `local.chatRequest` / `local.Message` structs (they are unexported; copy or re-declare in mock package — avoid import coupling to internal packages).

Declare in `cmd/milk-mock/server.go`:
- `mockChatRequest` — mirrors `local.chatRequest`
- `mockStreamChunk` — mirrors `local.streamChunk`

### Step 2.2 — Response generator

```go
func generateResponse(req mockChatRequest, name string) (text string, toolCalls []mockToolCall)
```

1. Extract last user message from `req.Messages`.
2. Scan for `[tool:<name>,<json>]` directives (regex: `\[tool:([^,\]]+),([^\]]+)\]`).
3. Parse the system context: first 200 chars of the first `system` role message, trimmed to first `\n`.
4. Return tool calls if directives found; otherwise return text response.

Text response template: `"response for: <last_user_msg>, context received: <context_summary>"`

### Step 2.3 — SSE streaming

```go
func streamText(w http.ResponseWriter, text string, latencyMs int)
func streamToolCall(w http.ResponseWriter, calls []mockToolCall)
```

Set headers: `Content-Type: text/event-stream`, `X-Accel-Buffering: no`, flush after each chunk.

Token-level streaming: split response text on word boundaries, emit one word per SSE chunk with optional `time.Sleep(latencyMs * time.Millisecond)`.

### Step 2.4 — Turn lifecycle

`POST /v1/chat/completions` handler:

1. Decode request → check `fail_rate` (return 500 if triggered).
2. `generateResponse(req)` → toolCalls or text.
3. If toolCalls: `streamToolCall(w, calls)`, wait for next request.
4. On follow-up request (contains a `tool` role message): `streamText(w, buildResponse(req, name))`.
5. After response: `recorder.RecordTurn(...)`.

Session tracking by `X-Session-Id` header or by maintaining a per-IP last-request map (simple: keyed by remote addr).

Non-streaming (`stream: false`): return full response JSON, no SSE.

---

## Phase 3 — Claude-mode subprocess

### Step 3.1 — Flag parsing

Parse the exact flags the real `claude` binary accepts (those that milk passes):
```
--print, --output-format, --verbose, --include-partial-messages,
--dangerously-skip-permissions, --permission-prompt-tool,
--allowedTools, --add-dir, --session-id, --resume,
--append-system-prompt-file, <prompt>
```

Use Cobra with `DisableFlagParsing` off and unknown flags ignored (to stay forward-compatible).

### Step 3.2 — Event emitter

```go
func emitJSON(w io.Writer, v any)                    // json.Marshal + "\n"
func emitSystem(w io.Writer, sessionID string)
func emitTextDelta(w io.Writer, idx int, text string)
func emitContentBlockStart(w io.Writer, idx int, blockType, name, id string)
func emitContentBlockStop(w io.Writer, idx int)
func emitToolUseDelta(w io.Writer, idx int, partialJSON string)
func emitAssistant(w io.Writer, content []claudeContent)
func emitResult(w io.Writer, sessionID string, isError bool, denials []permDenial)
func emitControlRequest(w io.Writer, reqID, toolName, toolUseID string, input any)
```

All write to `os.Stdout`.

### Step 3.3 — Tool directive handling

When `--permission-prompt-tool stdio` is set:
1. Emit `control_request` to stdout.
2. Read one NDJSON line from stdin (`control_response`).
3. If `behavior: "deny"` → skip tool, note denial in result.
4. If `behavior: "allow"` → run/mock the tool.

Tool "execution" in the mock: return `"[mock tool result for: <name>(<args>)]"` — the mock does not actually run tools. This exercises milk's permission prompt and context injection paths without real side effects.

### Step 3.4 — Session persistence

On `--resume <uuid>`: load `<output-dir>/<name>-sessions.jsonl`, filter by `session_id`, count prior turns. Include prior turn count in context summary: `"context received: <system_summary> [resuming session <uuid>, turn <n>]"`.

On new session: generate UUID, write `session` event.

### Step 3.5 — Recording

Same `Recorder` as server mode. Record after `result` event is emitted.

---

## Phase 4 — Build and wiring

### Step 4.1 — go.mod / Taskfile

No new dependencies needed (Cobra is already in `go.mod`).

Add to `Taskfile.yml`:
```yaml
mock:build:
  cmds:
    - go build -o milk-mock ./cmd/milk-mock/
mock:server:
  cmds:
    - ./milk-mock server --name {{.NAME | default "mock"}} --addr :19999
```

### Step 4.2 — Integration test config snippet

Add `docs/mock-setup.md` with copy-paste config for both openai and claude-cli mock.

---

## File map

```
cmd/milk-mock/
  main.go           — Cobra root, mode dispatch
  server.go         — server subcommand (OpenAI-compat HTTP)
  claudemode.go     — claude subcommand (NDJSON subprocess)
  respond.go        — shared: directive parser, response builder
internal/mockrecorder/
  recorder.go       — TurnRecord + MetricsRecord, JSONL append
docs/
  mock-design.md    — design (this branch)
  mock-plan.md      — implementation plan (this file)
  mock-setup.md     — integration config snippets (Phase 4)
```

---

## Sequencing

```
Phase 1  (1.1 + 1.2)  → binary compiles, recorder writes JSONL
Phase 2  (2.1–2.4)    → server mode works: milk can use it as primary agent
Phase 3  (3.1–3.5)    → claude mode works: milk can use it as escalation agent
Phase 4  (4.1–4.2)    → wired into Taskfile, docs complete
```

Each phase is independently testable:
- After Phase 1: `milk-mock server --name test` starts and writes files.
- After Phase 2: point a milk config at `localhost:19999`, run a turn, check `test-sessions.jsonl`.
- After Phase 3: set `bin: "milk-mock"` for the escalation agent, run a turn, check files.

---

## Deferred

- Bedrock event-stream format (binary framing is non-trivial; not needed for unit-testing routing logic)
- Real tool execution (mock result strings are sufficient for pipeline testing)
- `--fail-rate` and `--latency-ms` chaos knobs (Phase 2 placeholder, implement after core works)
