# 34. Agent-as-Tool: Peer Agents as Callable Tools

Date: 2026-06-24
Status: Accepted

## Context

milk routes turns between a primary agent and an escalation agent. Both roles are filled by a single named agent at a time, and every turn goes to one of them.

Some agents excel at narrow tasks: aider is a dedicated code-editing agent, a smolagents instance might wrap specialised Python tools, a local LLM might be fine-tuned for 
summarisation. None of these fit neatly into the primary/escalation binary — they are not general-purpose enough to own a conversation, but they are far better than the primary 
agent at specific subtasks.

The current model forces a full role-switch (escalation) to leverage a specialist agent, which carries the overhead of full context handoff and session state changes. What is 
actually needed is a lightweight, scoped call: send a request, get a response, continue.

OpenAI-compatible local agents already support tool calling. That infrastructure can be extended to route tool calls to peer agents without changing the protocol or adding new 
agent backends.

## Decision

### 1. Config: global `agent_tools` + optional per-agent override

Tool-agent definitions live at two levels:

**Global** (`Config.AgentTools`): apply to all agents. A tool defined here is available to every agent that can call tools, subject to the cycle guard.

**Per-agent** (`AgentConfig.Tools`): additive or override layer for a specific agent. An entry whose `agent` name matches a global entry shadows the global one (e.g. to give a 
different description or to disable it for this agent). An entry with a name not present globally is appended.

```json
{
  "agent_tools": [
    {
      "agent": "aider",
      "description": "Specialist code-editing agent. Send it a natural-language edit request; it applies the changes and returns a summary."
    },
    {
      "agent": "summariser",
      "description": "Fast summarisation agent. Pass a block of text; returns a concise summary."
    }
  ],
  "agents": [
    {
      "name": "qwen",
      "provider": "local",
      "url": "http://localhost:8080",
      "model": "qwen2.5-coder-7b"
    },
    {
      "name": "gemma",
      "provider": "local",
      "url": "http://localhost:8081",
      "model": "gemma-4-e4b",
      "tools": [
        {
          "agent": "aider",
          "description": "Apply a code edit. Describe the change in plain English.",
          "enabled": false
        }
      ]
    }
  ]
}
```

In the example, `qwen` sees both `aider` and `summariser` from the global list. `gemma` also sees both, but its per-agent entry overrides the `aider` description and disables it.

`AgentToolEntry` fields:

| Field         | Type   | Required | Semantics |
|---------------|--------|----------|-----------|
| `agent`       | string | yes      | Name of the target agent (must exist in `agents`) |
| `description` | string | yes      | Capability description injected into the calling agent's tool list |
| `enabled`     | bool   | no       | Defaults to `true` when absent; `false` disables without removing |

**Merge algorithm** for caller named `X`:

1. Start with a copy of `Config.AgentTools`.
2. For each entry in `X`'s `AgentConfig.Tools`: if an entry with the same `agent` name already exists in the list, replace it in place; otherwise append it.
3. Remove entries where `agent == X` (cycle guard).
4. Remove entries where `agent` is not found in `Config.effectiveAgents()` (unknown agent guard, with debug log).
5. Remove entries where `IsEnabled()` is false.

The result is the effective tool-agent list for that turn.

### 2. Tool definition synthesis

When building the tool list for a localRunner turn, milk takes the effective tool-agent list and synthesises one OpenAI function definition per entry:

```json
{
  "type": "function",
  "function": {
    "name": "agent_<sanitised_name>",
    "description": "<entry.Description>",
    "parameters": {
      "type": "object",
      "properties": {
        "request": {
          "type": "string",
          "description": "The request to send to the agent."
        }
      },
      "required": ["request"]
    }
  }
}
```

`sanitised_name` is the agent name lowercased with non-alphanumeric characters replaced by `_`, prefixed with `agent_` to avoid collisions with built-in tool names.

These definitions are appended to the existing tool list (after built-in tools such as `escalate`). They are not injected when the calling agent is acting as the escalation target 
(role-level cycle guard).

### 3. Tool-agent execution

When the local agent tool loop encounters a call whose name matches the `agent_<name>` pattern and the target is a registered tool-agent, milk:

1. Resolves the target `AgentConfig` by name.
2. Constructs a minimal TurnRunner for the target using the standard runner-building path (same code as startup initialisation). If 
the target runner is already built and cached in `dispatchAgents.toolRunners`, the cached instance is reused.
3. Calls a new `RunToolCall(ctx, cfg, prompt, out)` method on the 
runner. This method runs exactly one inference turn with no session recording, no state transitions, no escalation, no memory injection. It returns the agent's text response.
4. Returns the response as the tool result string. 5. Continues the tool loop.

If the tool-agent call fails (agent unreachable, timeout, error response), milk returns a structured error string as the tool result — the calling agent can decide how to proceed. 
It does not escalate or crash the session.

### 4. Tool-agent runner cache

`dispatchAgents` gains a `toolRunners map[string]TurnRunner` field. Runners are constructed lazily at first use and cached for the session lifetime.

The cache is keyed by agent name. If the user changes an agent's config mid-session via `/agent add` or `/agent switch`, the cache entry for that name is evicted. Any change to 
global or per-agent tool entries via `/agent tool` also evicts affected runners.

### 5. Scope and future paths

**v1 — localRunner (caller) + localRunner or subprocessRunner (target).** Tool-agent calls are supported when the calling agent is a **localRunner** (OpenAI-compat HTTP). The tool calling infrastructure for this path already exists in `internal/agent/local`. Subprocess agents (`aider-cli`, `subprocess`/smolagent) may also act as **targets**: they are stateless per-call, so `RunToolCall` invokes `RunFirst` with empty context strings and no session bookkeeping. The `claude-cli` provider is excluded from v1 targets — it requires session management that is incompatible with stateless single-shot invocation.

**v2 — cliRunner via tag-based stop-and-re-invoke.** The existing `<milk:percept:NONCE>` / `<milk:need:NONCE>` stream-interception infrastructure can be extended to support tool calls where the **calling** agent is a CLI agent (Claude Code subprocess). The protocol:

1. `BuildStaticContext` injects a `ToolCallInstruction(nonce, toolAgents)` block instructing the agent: emit `<milk:tool_call:NONCE>{"agent":"…","request":"…"}</milk:tool_call:NONCE>` and stop the response immediately when it wants to call a tool-agent.
2. The stream layer intercepts and strips the tag; milk dispatches `RunToolCall` to the target runner and receives the result.
3. milk re-invokes the CLI agent via `--resume` with the result injected into the next static context file as a `[Tool result from <agent>: …]` block.
4. Steps 1–3 repeat until the agent emits a turn with no tool_call tag.

Constraints: calls are serial (one per re-invoke round-trip); the result travels via the static context file, not as a user message, to keep the transcript clean. Whether the agent reliably honours the "stop here" instruction is model-dependent and must be validated before committing to this path.

**v3 — MCP.** Expose milk's tool-agent runners as a local MCP server. Claude Code natively understands MCP tool definitions; no custom tag protocol is needed. This is the cleanest long-term path for cliRunner but requires a separate MCP server infrastructure piece.

For subprocessRunner callers (aider, smolagents), v2 tag interception is unlikely to be reliable given those agents' autonomous internal loops. MCP (v3) is the appropriate path for enabling them as callers.

In v1, when the calling agent is a cliRunner or subprocessRunner, the effective tool-agent list is computed but not injected. This is documented in the config schema.

### 6. `/agent tool` subcommands

The `/agent` command gains a `tool` subcommand group. All subcommands accept an optional `for <agent-name>` suffix that targets a specific agent's per-agent list. The special target `global` addresses `Config.AgentTools` directly.

```
/agent tool list [<agent>|global]
    Show effective tool-agents for <agent> (default: active primary), or
    list only the global entries when "global" is given.
    Columns: name, scope (global/override/local), description (truncated), enabled.

/agent tool enable <tool-agent> [for <agent>|global]
    Set enabled: true on the matching entry in the target scope.
    If no entry exists in that scope, creates one (inheriting description from
    the global entry if adding a per-agent entry, or prompting if adding globally).

/agent tool disable <tool-agent> [for <agent>|global]
    Set enabled: false on the matching entry. Does not remove the entry.

/agent tool add <tool-agent> [for <agent>|global]
    Interactive wizard: prompt for description, add entry to target scope, save config.

/agent tool remove <tool-agent> [for <agent>|global]
    Remove the entry from the target scope entirely.
```

All subcommands save the updated config to disk immediately and evict affected runner cache entries.

### 7. Setup wizard step

The `/config init` wizard gains a step (`initStepAgentTools`) inserted after `initStepEscalation`:

> "Do you want to configure any agents as tools (available to all agents)?
> Tool-agents let any agent call a specialist agent for focused tasks.
> [y/N]"

If the user answers yes, milk lists all configured agents and prompts for a description for each one the user selects. Selected agents are added to `Config.AgentTools` with 
`enabled: true`. This step is skipped if only one agent is configured.

### 8. Tab completion

The autocomplete system gains completions for `/agent tool` subcommands: agent names from the current config are suggested for the tool-agent position and for the `for` argument.

## Consequences

**Positive:**
- Specialist agents are accessible without a full context handoff or session state change.
- Global definitions avoid repetition: define `aider` as a tool once; every capable agent picks it up.
- Per-agent overrides allow targeted adjustments (different description framing, or disabling a global tool for one specific agent) without duplicating the full definition.
- Cycle guard prevents accidental infinite self-recursion at both the config-resolution and dispatch levels.
- `enabled` flag allows toggling without config surgery.
- No protocol change required for existing agents.

**Negative:**
- v1 scope for **callers** is limited to localRunner (OpenAI-compat HTTP). CLI and subprocess agents cannot call tool-agents without future work (v2/v3).
- v1 scope for **targets** excludes `claude-cli` — it requires resumable sessions which are incompatible with stateless tool invocation.
- v2 (tag-based stop-and-re-invoke for cliRunner callers) is model-dependent: the CLI agent must reliably honour the "emit tag and stop" instruction. This needs empirical validation before committing; if the model ignores or half-follows the instruction, results will be unpredictable.
- v2 tool calls are serial; each call adds a full `--resume` round-trip. Latency compounds for multi-call turns.
- `RunToolCall` introduces a new execution path that bypasses session bookkeeping — care is needed that this never writes to the session or causes state transitions.
- Tool-agent runners share the same connection pool and concurrency as primary/escalation runners; a slow tool-agent call blocks the primary agent's tool loop turn.
- The `agent_` prefix namespace is ad-hoc; collisions with future built-in tools named `agent_*` are possible.
- Two-level merge adds a small amount of reasoning overhead when debugging why a tool-agent does or does not appear for a given agent.

**Neutral:**
- Tool-agent calls are not recorded in the session transcript. The calling agent's tool call and result are recorded as usual (as part of its own turn), but the 
tool-agent's internal reasoning is not.
- Memory and percept injection is disabled for tool-agent calls in v1. The tool-agent sees only the single request string. This keeps calls 
lightweight and deterministic.
