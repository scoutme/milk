# smolagents integration — design

## Background

milk currently supports two agent execution models:

| Provider | Execution | Loop managed by |
|---|---|---|
| `local` / `bedrock` | HTTP → OpenAI-compat REST | Go tool loop in `internal/agent/local` |
| `claude-cli` | subprocess → `claude --print --output-format stream-json` | Claude Code binary |

smolagents ([github.com/huggingface/smolagents](https://github.com/huggingface/smolagents)) is a Python agentic framework from HuggingFace. It offers two agent types (`CodeAgent` and `ToolCallingAgent`) and ~10 model backends (OpenAI-compat, LiteLLM, HF Inference, Bedrock, Transformers, vLLM, MLX). The key differentiator over milk's own tool loop is **CodeAgent**: the LLM writes Python code that gets executed in a sandboxed interpreter, rather than calling discrete JSON-schema tools. This enables a fundamentally different style of reasoning and tool composition.

---

## What smolagents provides that milk doesn't

| Capability | milk `local` | smolagents |
|---|---|---|
| Tool dispatch | Go-side JSON tool-call loop (up to 10 turns) | Python ReAct loop (`max_steps`, default 20) |
| Code execution | `bash` tool (subprocess) | `CodeAgent`: LLM writes Python, executed in-process (or sandboxed via e2b / Modal / Docker) |
| HF ecosystem tools | — | `web_search`, `visit_webpage`, HF Spaces tools, MCP server tools |
| Multi-agent composition | — | `managed_agents`: nest agents as tools |
| Planning | — | `planning_interval`: periodic structured plan injection |
| Model backends | OpenAI-compat + Bedrock | 10 backends incl. LiteLLM (100+ providers), local Transformers, vLLM, MLX |
| Persistence / Hub | — | `agent.save(folder)`, `agent.push_to_hub()` |

The most compelling addition for milk users is `CodeAgent` — the ability to write and execute multi-step Python programs to answer coding or data questions, without being limited to a fixed set of pre-declared tools.

---

## Architecture: generic subprocess abstraction

Rather than cloning `internal/agent/claude/` into `internal/agent/smolagent/`, milk factors the shared subprocess mechanics into a common base — `internal/agent/subprocess/` — and makes both `claude` and `smolagent` thin wrappers on top.

**Only two things differ between subprocess CLI agents:**

1. **How CLI args are assembled** (`--print --output-format stream-json --resume …` vs `--model-type … --action-type …`)
2. **How stdout is parsed** (Claude's stream-json NDJSON vs milk's subprocess protocol)

Everything else — subprocess lifecycle, temp file context injection (`--append-system-prompt-file`), env stripping, `RunFirst`/`RunResume` mechanics — is identical and lives in `subprocess/runner.go`.

```
internal/agent/
  subprocess/
    runner.go     — RunFirst, RunResume, subprocess lifecycle, temp file injection, env strip
    types.go      — ArgBuilder, StreamParser interfaces; ParseResult, ParseOpts

  claude/
    claude.go     — Agent type (With* builders; delegates to subprocess.Runner)
    args.go       — claudeArgBuilder: --print --output-format stream-json --resume etc.
    stream.go     — Claude stream-json NDJSON parser → subprocess.ParseResult  (unchanged)
    permissions.go — stdin-based permission protocol (unchanged)

  smolagent/
    smolagent.go  — Agent type (With* builders; delegates to subprocess.Runner)
    args.go       — smolagentArgBuilder: --model-type, --action-type, --tools, --imports etc.
    stream.go     — milk subprocess protocol parser → subprocess.ParseResult
```

### ArgBuilder and StreamParser interfaces

```go
// ArgBuilder constructs the CLI args for a specific agent binary.
type ArgBuilder interface {
    Bin() string
    BaseArgs() []string                                    // flags prepended before session args (e.g. --print)
    FirstArgs(sessionID string, contextFiles []string) []string
    ResumeArgs(sessionID string, contextFiles []string) []string
    EnvStrip() []string                                    // env var prefixes to remove from subprocess
    Ping() error                                           // lightweight health check
}

// StreamParser processes stdout from the subprocess.
type StreamParser interface {
    Parse(r io.Reader, out io.Writer, opts ParseOpts) (ParseResult, error)
}
```

`claude` and `smolagent` each implement `ArgBuilder` and `StreamParser`; `subprocess.Runner` owns all the plumbing.

---

## milk subprocess protocol

Since `milk-smolagent` is a custom adapter we write anyway, it implements **milk's subprocess protocol** — not a smolagents-specific schema. Any future CLI wrapper (aider, open-interpreter, a shell-script model bridge) that emits this format gets a free Go integration via `subprocess.Runner` + a thin `StreamParser`.

### NDJSON event types

```json
{"type":"system","session_id":"<uuid>","agent":"<name>","version":"1"}
{"type":"text_delta","text":"partial token..."}
{"type":"step","number":1,"thought":"...","code":"..."}
{"type":"observation","number":1,"content":"..."}
{"type":"final_answer","text":"..."}
{"type":"result","is_error":false,"input_tokens":820,"output_tokens":340}
```

`step` and `observation` are optional extensions — they carry smolagents step metadata and are rendered as formatted blocks by `smolagent/stream.go`. Adapters that don't emit them are equally valid; the base parser ignores unrecognised types.

### Built-in `smolagent` CLI vs `milk-smolagent`

The smolagents package ships its own `smolagent` CLI (`smolagents.cli:main`) but it does **not** implement milk's subprocess protocol — it has no `--append-system-prompt-file`, no `--session-id`, and emits plain text. `milk-smolagent` is a purpose-built adapter.

---

## `provider: "smolagent-cli"` — config shape

```json
{
  "agents": [
    {
      "name": "qwen-local",
      "url": "http://localhost:8080",
      "model": "Qwen/Qwen2.5-Coder-7B-Instruct",
      "provider": "local"
    },
    {
      "name": "smol-coder",
      "provider": "smolagent-cli",
      "smolagent_bin": "milk-smolagent",
      "url": "http://localhost:8080",
      "model": "Qwen/Qwen2.5-Coder-7B-Instruct",
      "model_type": "OpenAIModel",
      "action_type": "code",
      "smolagent_tools": ["web_search"],
      "max_steps": 15,
      "authorized_imports": ["numpy", "pandas", "json", "pathlib"]
    },
    {
      "name": "claude",
      "provider": "claude-cli",
      "bin": "claude"
    }
  ],
  "agent": "qwen-local",
  "escalation_agent": "claude"
}
```

Switch at runtime: `/agent switch smol-coder` to use smolagents as primary; `/agent switch smol-coder as escalation` to route escalated turns there.

### New `AgentConfig` fields (scoped to `provider: "smolagent-cli"`)

```go
SmolagentBin      string   `json:"smolagent_bin"`       // path to milk-smolagent (default: "milk-smolagent")
ActionType        string   `json:"action_type"`         // "code" | "tool_calling" (default: "code")
ModelType         string   `json:"model_type"`          // smolagents model class name (default: "OpenAIModel")
SmolagentTools    []string `json:"smolagent_tools"`     // tool names passed to smolagents
MaxSteps          int      `json:"max_steps"`           // default: 15
AuthorizedImports []string `json:"authorized_imports"`  // CodeAgent only; packages allowed to import
```

Existing fields (`URL` → `--api-base`, `Model` → `--model-id`, `APIKey` → `--api-key`) are forwarded to `milk-smolagent`.

---

## `milk-smolagent` Python script

Single-file Python script (~250 lines), installable via `pip install milk-smolagent` or placed on PATH.

### CLI interface

```
milk-smolagent
  [--model-type InferenceClientModel|OpenAIModel|LiteLLMModel|TransformersModel]
  [--model-id MODEL_ID]
  [--api-base URL]
  [--api-key KEY]
  [--action-type code|tool_calling]
  [--tools tool1 tool2 ...]
  [--authorized-imports pkg1 pkg2 ...]
  [--max-steps N]
  [--session-id UUID]
  [--append-system-prompt-file PATH]      # may appear twice (static + dynamic)
  [--verbosity 0|1|2]
  -- "user prompt"
```

### Execution

1. Read `--append-system-prompt-file` files; concatenate as `instructions`
2. Instantiate model from `--model-type` + `--model-id` + `--api-base` / `--api-key`
3. Instantiate `CodeAgent` or `ToolCallingAgent` with tools, max_steps, authorized_imports
4. Emit `{"type":"system","session_id":"...","agent":"smolagent","version":"1"}`
5. Call `agent.run(task=prompt, stream=True)`
6. Translate generator events → NDJSON on stdout:
   - `ActionStep` → `step` + `observation` events
   - `ChatMessageStreamDelta` → `text_delta`
   - `FinalAnswerStep` → `final_answer`
7. Emit `result` event with token totals

**Error handling:** unhandled exceptions → `{"type":"result","is_error":true,"error":"..."}` then exit 1.

### Session continuity

`agent.run(reset=True)` is called each turn; history is injected via `--append-system-prompt-file`. No multi-turn smolagents-internal memory in the first iteration — conversation history lives in milk's session JSON, not in smolagents' step memory.

---

## Context injection

Context files are concatenated and passed as `instructions` to the smolagents agent constructor, which injects them as an extra system prompt block. This is identical to milk's `--append-system-prompt-file` strategy on the claude-cli path.

Static context (nonce + escalation instructions) and dynamic context (turn summary) are two separate `--append-system-prompt-file` flags, preserving milk's cache-optimised context structure.

---

## Session and routing integration

- `smolagent-cli` agents participate in routing exactly like `local` agents
- `smolagent-cli` can be set as `escalation_agent` — making smolagents an alternative escalation path to Claude
- The same dispatch path is used for both `claude-cli` and `smolagent-cli` escalation targets; the only difference is which `subprocess.Agent` implementation is constructed
- `EscalationSignal`: if `milk-smolagent` emits a `final_answer` containing milk's escalation tag, the Go wrapper surfaces it as an `EscalationSignal`
- Session state and history stored in milk's session JSON as normal

---

## Go implementation scope

**`internal/agent/subprocess/`** (~150 lines, new):
- `types.go`: `ArgBuilder`, `StreamParser`, `ParseResult`, `ParseOpts`
- `runner.go`: `Runner` struct, `RunFirst`, `RunResume`, `appendContextFiles`, `newCmd`, `filterEnv`

**`internal/agent/smolagent/`** (~250 lines, new):
- `smolagent.go`: `Agent` type with `With*` builder methods; delegates to `subprocess.Runner`
- `args.go`: `smolagentArgBuilder`
- `stream.go`: milk subprocess protocol parser

**`internal/agent/claude/`** (refactored, net ~50 lines smaller):
- `args.go`: `claudeArgBuilder` extracted from `claude.go`
- `claude.go`: `runPipe`/`newCmd`/`filterEnv`/`appendContextFiles` removed; delegates to `subprocess.Runner`

**`internal/config/config.go`**: add smolagent-specific fields; add `IsSmolagentCLI()` helper

**`cmd/milk/main.go` + `cmd/milk/repl.go`**: wire `smolagent-cli` as a valid escalation target alongside `claude-cli`

**`scripts/milk-smolagent`**: Python adapter script

---

## Comparison: original Option A vs this design

| Criterion | Original Option A | This design |
|---|---|---|
| New Go code | ~250 lines (clone of claude pkg) | ~150 lines subprocess + ~250 lines smolagent — but claude pkg shrinks by ~200 lines |
| Code reuse | None — duplicate subprocess plumbing | subprocess.Runner shared between claude and smolagent |
| Adding a 3rd subprocess agent | Another ~400 line clone | ArgBuilder + StreamParser impls only (~100 lines) |
| Config clarity | New `provider` type — explicit | Same |
| Per-turn startup overhead | Yes (~0.5–1 s) | Same |
| Multi-turn agent memory | No | Same |

---

## Not in scope (first iteration)

- Permission prompts for Python tool execution inside smolagents
- `reset=False` multi-turn agent memory
- Keep-alive mode to eliminate Python startup overhead
- Bedrock model backend for `milk-smolagent` (add `--model-type AmazonBedrockModel`)
- Gradio / Hub integration
