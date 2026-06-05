# smolagents integration — design alternatives

## Background

milk currently supports two agent execution models:

| Provider | Execution | Loop managed by |
|---|---|---|
| `local` / `bedrock` | HTTP → OpenAI-compat REST | Go tool loop in `internal/agent/local` |
| `claude-cli` | subprocess → `claude --print --output-format stream-json` | Claude Code binary |

smolagents ([github.com/huggingface/smolagents](https://github.com/huggingface/smolagents)) is a Python agentic framework from HuggingFace. It offers two agent types (`CodeAgent` and `ToolCallingAgent`) and ~10 model backends (OpenAI-compat, LiteLLM, HF Inference, Bedrock, Transformers, vLLM, MLX). The key differentiator over milk's own tool loop is **CodeAgent**: the LLM writes Python code that gets executed in a sandboxed interpreter, rather than calling discrete JSON-schema tools. This enables a fundamentally different style of reasoning and tool composition.

This document explores how to integrate smolagents into milk as an alternative to the `local` agent, analyzes three viable shapes, and recommends one.

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

## Integration alternatives

### Option A — `provider: "smolagent-cli"` (subprocess wrapper)

A thin Python bridge script (`milk-smolagent`) wraps the smolagents Python API and emits NDJSON on stdout, mirroring the `claude-cli` pattern. milk spawns it per turn.

```
milk → Go subprocess wrapper → milk-smolagent → smolagents Python API → model
```

**Config shape:**

```json
{
  "agents": [{
    "name": "smol-coder",
    "provider": "smolagent-cli",
    "bin": "milk-smolagent",
    "model_type": "OpenAIModel",
    "model_id": "Qwen/Qwen2.5-Coder-7B-Instruct",
    "api_base": "http://localhost:8080",
    "action_type": "code",
    "tools": ["web_search"],
    "max_steps": 15
  }]
}
```

**New Go code:** `internal/agent/smolagent/` — analogous to `internal/agent/claude/`. Spawns the Python script, reads NDJSON, handles context injection.

**CLI flags passed to `milk-smolagent`:**

```
milk-smolagent \
  --model-type OpenAIModel \
  --model-id Qwen/Qwen2.5-Coder-7B-Instruct \
  --api-base http://localhost:8080 \
  --action-type code \
  --tools web_search \
  --max-steps 15 \
  --session-id <uuid> \
  --append-system-prompt-file <path> \    # static context (milk instructions)
  --append-system-prompt-file <path> \    # dynamic context (turn summary)
  -- "user prompt"
```

**NDJSON output format** (emitted by `milk-smolagent`):

```json
{"type":"system","session_id":"<uuid>","model_id":"Qwen/Qwen2.5-Coder-7B-Instruct","action_type":"code"}
{"type":"step","step_number":1,"thought":"I need to …","code":"result = tool(…)"}
{"type":"observation","step_number":1,"content":"tool output…"}
{"type":"stream_delta","text":"Partial text token…"}
{"type":"final_answer","text":"The answer is …"}
{"type":"result","is_error":false,"steps":3,"input_tokens":820,"output_tokens":340}
```

**Session continuity:** smolagents' `run(reset=True)` is called each turn; conversation history is injected via `--append-system-prompt-file` exactly as with the claude-cli path. No long-lived Python process required.

**Pros:**
- Directly parallels the existing `claude-cli` provider shape — minimal conceptual surface
- All smolagents agent types and model backends available
- CodeAgent sandbox options (e2b, Modal, Docker) work as-is
- No persistent daemon; milk manages the lifecycle
- Can target the same local inference server already running for the `local` agent

**Cons:**
- Requires writing and shipping `milk-smolagent` (Python, ~200 lines)
- Python startup latency per turn (~0.5–1 s for import overhead) unless a keep-alive mode is added
- Custom NDJSON schema maintained separately from smolagents' own output types
- No `reset=False` multi-turn continuity (agent's internal `memory` resets each turn); conversation history lives in milk's session, not in smolagents' step memory
- Permission handling: milk's permission prompt system doesn't apply to Python tool execution inside smolagents

---

### Option B — OpenAI-compat adapter service

A persistent Python service translates OpenAI `/v1/chat/completions` requests into smolagents `run()` calls and streams responses back as SSE. milk connects to it using the existing `local` agent — **no new Go code needed**.

```
milk → Go local agent (HTTP) → smolagent-server (Python FastAPI) → smolagents → model
```

**Config shape (zero new fields):**

```json
{
  "agents": [{
    "name": "smol-coder",
    "url": "http://localhost:19998",
    "model": "smol-code",
    "provider": "local"
  }]
}
```

**Adapter responsibilities:**

1. Accept `POST /v1/chat/completions` with `{messages, stream: true}`
2. Extract the last user message as the task; prepend system messages as instructions
3. Instantiate `CodeAgent` (or `ToolCallingAgent`) with the configured backend model
4. Call `agent.run(task, stream=True)`
5. Translate `StreamEvent` union → SSE `data: {...}` chunks in OpenAI delta format
6. Map `ActionStep` / `ToolCall` / `ToolOutput` events to text narration or structured content
7. Emit `data: [DONE]` on `FinalAnswerStep`

**Startup:**

```bash
pip install "smolagents[openai]"
smolagent-server \
  --model-type OpenAIModel \
  --model-id Qwen/Qwen2.5-Coder-7B-Instruct \
  --api-base http://localhost:8080 \
  --action-type code \
  --addr :19998
```

**Pros:**
- Zero new Go code; uses the existing `local` agent path
- Works immediately after shipping the Python adapter
- Persistent process: no per-turn Python startup overhead
- Multi-turn `reset=False` continuity is possible (adapter holds agent instance per session ID)
- Tool calls surfaced back to milk as assistant text (narrated steps), so the TUI renders them naturally

**Cons:**
- Users must run a separate Python daemon alongside the inference server
- Session management is now split: milk session + adapter-internal smolagents session
- The impedance mismatch between OpenAI messages and smolagents `run(task)` is lossy: tool-call deltas, step observations, and code blocks are narrated as plain text, losing milk's structured tool-call rendering
- Adapter must be kept alive; if it crashes, milk's graceful-degradation path (warn + route to escalation) kicks in but the agent is silently gone
- The adapter's `/v1/chat/completions` interface doesn't expose smolagents-specific parameters (action_type, max_steps, authorized_imports) without a custom extension

---

### Option C — `action_type` field on existing `AgentConfig`

Extend the existing `AgentConfig` with a `action_type` discriminator. When present, the `local` agent (or a new thin variant) uses smolagent subprocess execution instead of its own HTTP tool loop. This makes it a **flavour** of the `local` agent rather than a separate provider type.

```json
{
  "agents": [{
    "name": "smol-coder",
    "url": "http://localhost:8080",
    "model": "Qwen/Qwen2.5-Coder-7B-Instruct",
    "provider": "local",
    "action_type": "code",
    "smolagent_bin": "milk-smolagent",
    "max_steps": 15
  }]
}
```

When `action_type` is set, `internal/agent/local/local.go` (or a new `RunSmol()` path) delegates to the subprocess rather than running the HTTP tool loop itself. The HTTP URL is still passed to the subprocess as `--api-base`; the Go HTTP transport is bypassed for inference calls.

**Conceptually:** `action_type` means "hand the agentic loop to smolagents; still use this URL for the model backend."

**Pros:**
- Single config entry: model URL, credentials, and agentic behaviour are co-located
- Existing transport stack (SigV4, Bearer token, token_cmd) is still used to validate the connection; smolagent subprocess receives `--api-key` or `--api-base` directly
- Routing, session management, and context injection remain unchanged
- Gradual adoption: set `action_type` to opt into smolagents; remove it to revert to the Go tool loop

**Cons:**
- Adds branching inside `internal/agent/local/` or a parallel `RunSmol()` method — risk of divergence from the primary code path
- `action_type` semantics straddle two different execution models, which is confusing: the `local` agent's HTTP tool loop and the smolagent subprocess are fundamentally different
- Same Python startup overhead as Option A
- The `url` field becomes ambiguous: used for classification calls and routing checks even when the agentic loop is delegated

---

## Comparison matrix

| Criterion | A (subprocess) | B (HTTP adapter) | C (action_type flavour) |
|---|---|---|---|
| New Go code | ~250 lines (`internal/agent/smolagent/`) | 0 | ~100 lines (branch in `local`) |
| New Python code | ~250 lines (`milk-smolagent`) | ~350 lines (FastAPI adapter) | ~250 lines (`milk-smolagent`) |
| Config clarity | New `provider` type — explicit | Invisible (looks like plain local) | Overloaded existing fields |
| Per-turn startup overhead | Yes (~0.5–1 s) | No (persistent) | Yes (~0.5–1 s) |
| Multi-turn agent memory | No (reset=True per turn) | Yes (if adapter holds instance) | No |
| Tool-call rendering in TUI | Structured (NDJSON) | Narrated text | Structured (NDJSON) |
| Fits existing architecture | Matches `claude-cli` shape | Matches `local` shape | Awkward blend |
| Permission prompts | Unsupported | Unsupported | Unsupported |
| Operator complexity | Low (script on PATH) | Medium (daemon to manage) | Low |
| Extensibility | Each field maps 1:1 to smolagent CLI | Adapter must grow per feature | Fields shared with local agent |

---

## Recommendation

**Ship Option A** (`provider: "smolagent-cli"`) as the primary integration path, with Option B as an optional companion for users who need zero startup overhead or multi-turn agent memory.

**Rationale:**

1. **Architectural fit.** Option A is structurally identical to `claude-cli`. Both are subprocess providers with a NDJSON streaming contract and context injection via `--append-system-prompt-file`. Adding `internal/agent/smolagent/` mirrors `internal/agent/claude/` without bending either existing agent type.

2. **Explicit config.** `provider: "smolagent-cli"` makes the execution model visible. Users can have a `local` agent and a `smolagent-cli` agent in the same config and switch between them with `/agent switch`.

3. **Deferred complexity.** The Python startup overhead is real but acceptable for the typical smolagents use case (longer, multi-step coding or research tasks). If latency becomes a problem, a keep-alive mode can be added to the `milk-smolagent` script later (long-poll STDIN, re-use the Python process across turns).

4. **CodeAgent is the value.** The most differentiated thing smolagents brings is Python code execution. This requires the subprocess model anyway (the Python interpreter must run somewhere). Option B abstracts it behind HTTP but the value is still subprocess execution; Option A makes that explicit.

Option B is worth implementing as a follow-on if users want:
- Agent-internal memory continuity across turns
- Adapting smolagents as the `local` agent without any new Go code
- Routing the same OpenAI-compat URL through smolagents transparently

---

## Option A — detailed spec

### AgentConfig additions

```go
// internal/config/config.go — AgentConfig

// Smolagent-specific fields (used when Provider == "smolagent-cli")
SmolagentBin        string   `json:"smolagent_bin"`        // path to milk-smolagent (default: "milk-smolagent")
ActionType          string   `json:"action_type"`          // "code" | "tool_calling" (default: "code")
ModelType           string   `json:"model_type"`           // smolagents model class name (default: "OpenAIModel")
SmolagentTools      []string `json:"smolagent_tools"`      // tool names to pass to smolagents
MaxSteps            int      `json:"max_steps"`            // default: 15
AuthorizedImports   []string `json:"authorized_imports"`   // CodeAgent only; packages allowed to import
```

Existing `URL`, `Model`, `APIKey`, `TokenCmd`, `AWSRegion` etc. continue to work — they are forwarded to `milk-smolagent` and used for the model backend.

### New Go package: `internal/agent/smolagent/`

**`smolagent.go`** — subprocess wrapper:

```go
type Agent struct {
    bin              string    // milk-smolagent binary path
    modelType        string
    modelID          string
    apiBase          string
    apiKey           string
    actionType       string    // "code" | "tool_calling"
    tools            []string
    maxSteps         int
    authorizedImports []string
    debugLog         io.Writer
}

type RunResult struct {
    Text          string
    SessionID     string
    Steps         int
    InputTokens   int
    OutputTokens  int
}

func (a *Agent) RunFirst(ctx context.Context, systemFiles []string, prompt string, out io.Writer, sess *session.Session) (*RunResult, error)
func (a *Agent) RunResume(ctx context.Context, sessionID string, systemFiles []string, prompt string, out io.Writer, sess *session.Session) (*RunResult, error)
```

**`stream.go`** — NDJSON parser matching the format emitted by `milk-smolagent`:

| Event type | Action |
|---|---|
| `system` | capture session_id, model_id |
| `step` | write thought + code to `out` as formatted block |
| `observation` | write observation to `out` |
| `stream_delta` | write `text` to `out` |
| `final_answer` | write text to `out`; set result.Text |
| `result` | set token counts; check is_error |

### `milk-smolagent` Python script

Single-file Python script (~250 lines), installable via `pip install milk-smolagent` or by placing it on PATH.

**CLI interface:**

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
  [--session-id UUID]                     # informational; used in NDJSON system event
  [--append-system-prompt-file PATH]      # may appear twice (static + dynamic)
  [--verbosity 0|1|2]                     # controls NDJSON step events
  -- "user prompt"
```

**Execution:**

1. Read `--append-system-prompt-file` files; concatenate as `instructions`
2. Instantiate model from `--model-type` + `--model-id` + `--api-base` / `--api-key`
3. Instantiate `CodeAgent` or `ToolCallingAgent` with tools, max_steps, authorized_imports
4. Emit `{"type":"system","session_id":"...","model_id":"...","action_type":"..."}`
5. Call `agent.run(task=prompt, stream=True)`
6. Consume generator:
   - `ActionStep` → emit `step` + `observation` events
   - `ChatMessageStreamDelta` → emit `stream_delta`
   - `FinalAnswerStep` → emit `final_answer`
7. Emit `result` event with token totals from `RunResult.token_usage`

**Error handling:** unhandled exceptions → emit `{"type":"result","is_error":true,"error":"..."}` then exit 1.

### Context injection

Context files are concatenated in order and passed as `instructions` to the agent constructor. smolagents injects `instructions` as an extra system prompt block (via `MultiStepAgent.__init__`). This is equivalent to milk's `--append-system-prompt-file` strategy with the claude-cli path.

Static context (nonce + escalation instructions) and dynamic context (turn summary) are two separate `--append-system-prompt-file` flags, preserving milk's cache-optimised context structure.

### Config example

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

Switch at runtime: `/agent switch smol-coder` to use smolagents as primary; `/primary` to return.

### Session and routing integration

- `smolagent-cli` agents participate in routing exactly like `local` agents: the router classifies the prompt and can direct it to a `smolagent-cli` agent as primary
- `smolagent-cli` can also be set as `escalation_agent` — this makes smolagents an alternative escalation path to Claude
- `EscalationSignal` from the `smolagent-cli` agent: if smolagents emits a `final_answer` event containing milk's escalation tag, the Go wrapper surfaces it as an `EscalationSignal` error (same mechanism as the local agent's self-escalation)
- Session state and history stored in milk's session JSON as normal; `milk-smolagent` receives them via system prompt files, not its own persistence

### Not in scope (first iteration)

- Permission prompts for Python tool execution inside smolagents (smolagents controls the executor)
- `reset=False` multi-turn agent memory (each turn starts fresh; conversation history via system prompt)
- Bedrock model backend for `milk-smolagent` (can be added by setting `--model-type AmazonBedrockModel`)
- Keep-alive mode to eliminate Python startup overhead
- Gradio / Hub integration
