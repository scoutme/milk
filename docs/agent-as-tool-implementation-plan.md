# Implementation Plan: Agent-as-Tool (ADR-0034)

## Overview

Enable any configured agent to act as a callable tool for a localRunner (OpenAI-compat) primary agent. Specialist agents (aider, smolagents, custom summarisers) become tool functions the primary model can invoke without a full session handoff.

Tool definitions live at two levels: a global `agent_tools` array on `Config` (available to all agents) and an optional per-agent `tools` array on `AgentConfig` (additive/override layer). The effective list for any caller is the merge of the two, with cycle guard and enabled filtering applied.

## Phases

---

### Phase 1 — Config schema

**Files:** `internal/config/config.go`, `internal/config/config_test.go`

1. Add `AgentToolEntry` struct:
   ```go
   type AgentToolEntry struct {
       Agent       string `json:"agent"`
       Description string `json:"description"`
       Enabled     *bool  `json:"enabled,omitempty"` // nil = true
   }

   func (e AgentToolEntry) IsEnabled() bool {
       return e.Enabled == nil || *e.Enabled
   }
   ```

2. Add `AgentTools []AgentToolEntry` to `Config` (top-level, after `EscalationAgent`):
   ```go
   AgentTools []AgentToolEntry `json:"agent_tools,omitempty"`
   ```

3. Add `Tools []AgentToolEntry` to `AgentConfig` (after `Limits`):
   ```go
   Tools []AgentToolEntry `json:"tools,omitempty"`
   ```

4. Add `EffectiveToolAgents(callerName string) []AgentToolEntry` on `Config`:
   - Start with a copy of `Config.AgentTools`.
   - For each entry in the caller's `AgentConfig.Tools`: if an entry with the same `agent` name exists in the working list, replace it in place; otherwise append.
   - Filter: drop entries where `agent == callerName` (cycle guard).
   - Filter: drop entries whose `agent` name is not found in `effectiveAgents()` (log at debug level).
   - Filter: drop entries where `!IsEnabled()`.
   - Return the result.

5. Add `config_test.go` cases for `EffectiveToolAgents`:
   - No tools configured → empty list.
   - Global only → all entries returned for any caller (minus cycle guard).
   - Per-agent override shadows global entry (description replaced).
   - Per-agent disables a global entry (`enabled: false`).
   - Per-agent adds an entry not in global list.
   - Cycle guard: caller's own name is excluded.
   - Unknown agent name is silently dropped.

---

### Phase 2 — Tool definition synthesis

**Files:** `internal/agent/local/tools.go`, `internal/agent/local/tools_test.go`

1. Add `AgentToolSchemas(entries []config.AgentToolEntry) []map[string]any`:
   - One schema per entry:
     ```json
     {
       "type": "function",
       "function": {
         "name": "agent_<sanitised>",
         "description": "<entry.Description>",
         "parameters": {
           "type": "object",
           "properties": {
             "request": { "type": "string", "description": "The request to send to the agent." }
           },
           "required": ["request"]
         }
       }
     }
     ```
   - Name sanitisation: `strings.ToLower`, replace `[^a-z0-9]+` → `_`, prefix `agent_`.

2. Thread `toolAgentEntries []config.AgentToolEntry` into `schemas(...)` as a new parameter. Append `AgentToolSchemas(toolAgentEntries)` after the built-in tool list.

3. Callers of `schemas` in `local.go` pass the effective tool-agent list for the current turn. The list is computed once per turn via `cfg.EffectiveToolAgents(agentName)` and supplied by the runner (see Phase 3).

4. Add `tools_test.go` coverage for `AgentToolSchemas`: empty list, single entry, name sanitisation edge cases.

---

### Phase 3 — Tool-agent runner interface

**Files:** `cmd/milk/runner.go`

1. Add `RunToolCall` to the `TurnRunner` interface:
   ```go
   // RunToolCall executes a single lightweight inference call with no session
   // bookkeeping. Returns the agent's text response or an error.
   RunToolCall(ctx context.Context, cfg config.Config, prompt string, out io.Writer) (string, error)
   ```

2. Implement on `localRunner`:
   - Build `[{role: "user", content: prompt}]`.
   - Call the agent's single-shot inference path (no tool schemas, no history, no memory injection).
   - Return the text response.

3. Implement on `subprocessRunner`:
   ```go
   func (r *subprocessRunner) RunToolCall(ctx context.Context, _ config.Config, prompt string, out io.Writer) (string, error) {
       _, res, err := r.agent.RunFirst(ctx, "", "", prompt, out)
       if err != nil { return "", err }
       return res.Text, nil
   }
   ```
   Subprocess agents are stateless per-call — `RunFirst` with empty context strings is sufficient.

   Implement a stub on `cliRunner`:
   ```go
   return "", errors.New("tool-agent calls not supported for this provider")
   ```
   `claude-cli` requires resumable sessions; the tag-intercept path (v2) is the future work.

   `buildToolRunner` in `toolagent.go` routes by provider: `aider-cli` → `aider.New`, `subprocess` → `smolagent.New`, `claude-cli` → error, else → `local.NewFromConfig`.

---

### Phase 4 — Tool-agent runner cache + dispatch hook

**Files:** `cmd/milk/repl.go`, `internal/agent/local/local.go`, `cmd/milk/dispatch.go`, new `cmd/milk/toolagent.go`

1. Add `toolRunners map[string]TurnRunner` to `dispatchAgents`.

2. New file `cmd/milk/toolagent.go` — `getOrBuildToolRunner(ctx, name string, cfg config.Config, da *dispatchAgents) (TurnRunner, error)`:
   - Check `da.toolRunners` cache.
   - If missing, resolve the `AgentConfig` by name, construct the runner using the same per-provider switch extracted from startup.
   - Store in cache and return.

3. Add `ToolAgentDispatcher` callback type and field on `local.Agent`:
   ```go
   type ToolAgentDispatcher func(ctx context.Context, agentName, request string, out io.Writer) (string, error)
   ```
   Wire via `agent.SetToolAgentDispatcher(fn)`.

4. In `executeToolCalls` (`internal/agent/local/local.go`), before the `dispatchTool` call, intercept `agent_*` calls:
   ```go
   if strings.HasPrefix(tc.Function.Name, "agent_") && a.toolAgentDispatcher != nil {
       var reqArgs struct{ Request string `json:"request"` }
       json.Unmarshal([]byte(tc.Function.Arguments), &reqArgs)
       result, err := a.toolAgentDispatcher(ctx, tc.Function.Name[len("agent_"):], reqArgs.Request, out)
       if err != nil {
           result = toolResult{Error: err.Error()}.String()
       }
       msgs = append(msgs, Message{Role: "tool", Content: result, ToolCallID: tc.ID})
       continue
   }
   ```

5. In `dispatch.go`'s `runPrimary`, wire the dispatcher after building the runner:
   ```go
   if lr, ok := runner.(*localRunner); ok {
       lr.agent.SetToolAgentDispatcher(func(ctx context.Context, name, request string, out io.Writer) (string, error) {
           tr, err := getOrBuildToolRunner(ctx, name, cfg, da)
           if err != nil { return "", err }
           return tr.RunToolCall(ctx, cfg, request, out)
       })
   }
   ```

6. Add OTel counters: `milk.tools.tool_agent_calls` (labelled by `agent` name) and `milk.tools.tool_agent_errors`.

---

### Phase 5 — `/agent tool` subcommands

**Files:** `cmd/milk/interactive.go`

1. Add `"tool"` case to the `/agent` subcommand switch. Delegate to `execAgentTool(sub string, st *interactiveState) string`.

2. `execAgentTool` parses the second token (`list`, `enable`, `disable`, `add`, `remove`) and the optional `for <agent>|global` suffix. Default scope: active primary agent. Special scope `"global"` targets `st.cfg.AgentTools` directly.

3. Implement per-subcommand helpers:
   - `agentToolList(scope, st)` — tabular output: name, scope badge (global/override/local), truncated description, enabled status.
   - `agentToolEnable(toolName, scope, st)` — set `Enabled = true`; create entry if absent (inherit description from global entry if available, otherwise prompt).
   - `agentToolDisable(toolName, scope, st)` — set `Enabled = false`.
   - `agentToolAdd(toolName, scope, st)` — prompt for description; append entry.
   - `agentToolRemove(toolName, scope, st)` — remove entry from the target scope.
   - All mutations: `config.Save(st.cfg)` + evict `da.toolRunners[toolName]`.

4. Update help string (near line 116):
   ```
   /agent tool list [<agent>|global]          show tool-agents (default: primary)
   /agent tool enable <tool> [for <agent>|global]
   /agent tool disable <tool> [for <agent>|global]
   /agent tool add <tool> [for <agent>|global]
   /agent tool remove <tool> [for <agent>|global]
   ```

5. Extend `tabCompleteSlash` to complete agent names after `agent tool enable|disable|add|remove` and after `for`.

---

### Phase 6 — Setup wizard step

**Files:** `cmd/milk/interactive.go`

1. Add `initStepAgentTools initWizardStep` constant between `initStepEscalation` and `initStepOpenConfig`.

2. Wizard step handler (after escalation step):
   - Skip if `len(effectiveAgents) <= 1`.
   - Ask: `"Configure any agents as tools (available to all agents)? [y/N]"`
   - On `y`: numbered list of all agents (excluding none — the user picks which ones); prompt for a description per selected agent; append to `st.wiz.cfg.AgentTools`.
   - On `n` or blank: skip to `initStepOpenConfig`.

---

### Phase 7 — Docs

**Files:** `docs/spec.md`

1. Add `agent_tools` to the top-level config field table and `tools` to the `AgentConfig` field table.
2. Add "Agent-as-Tool" subsection: feature overview, global vs per-agent merge, v1 scope limitation, cycle guard.
3. Add `/agent tool` subcommands to the slash command reference table.

---

## Sequencing

```
Phase 1 (config schema)
    ↓
Phase 2 (schema synthesis)  →  Phase 3 (runner interface)
                                        ↓
                              Phase 4 (dispatcher wiring)
                                        ↓
                         Phase 5 (/agent tool) ──┐
                         Phase 6 (wizard)        ├─ independent after Phase 4
                         Phase 7 (docs)  ────────┘
```

---

## Key constraints / notes

- **No session writes**: `RunToolCall` must never call `sess.AddTurn`, `sess.Save`, or transition session state.
- **Error budget**: failed tool-agent calls return `toolResult{Error: ...}` — the calling model sees a tool error and can retry or continue; the session is unaffected.
- **Cache eviction**: `/agent tool` mutations evict `da.toolRunners[name]` so the next call rebuilds with fresh config. Agent config changes via `/agent add`/`switch` also evict.
- **Global scope name collision**: if both `Config.AgentTools` and `AgentConfig.Tools` define the same `agent` name, the per-agent entry wins — it replaces the global entry in the merged list, not supplements it.
- **No memory/percept injection in v1**: `RunToolCall` is a direct single-shot call. Document this limitation in the schema.
- **Tool-agent chaining (unvalidated)**: when an agent is invoked as a tool, it should probably not have other tool-agents injected into its own tool list — no recursive chaining by default. Rationale: prevents unbounded call chains and keeps invocations lightweight and deterministic. Validate this before implementing Phase 3/4; surface the decision explicitly rather than letting it fall through as a side effect.
- **Subprocess targets (v1)**: `aider-cli` and `subprocess` agents are supported as tool targets. They are stateless per-call — `RunFirst` with empty context strings handles each call independently. No session ID is returned or stored.
- **claude-cli targets (excluded from v1)**: `claude-cli` agents require resumable sessions; a stateless `RunFirst` call discards the session ID, breaking continuity. Excluded until v2 (tag-intercept) or v3 (MCP).
- **v2 (cliRunner caller tag-based)**: a future phase can extend support to cliRunner **callers** via a `<milk:tool_call:NONCE>` tag + stop-and-re-invoke loop (mirroring the existing percept/need tag infrastructure). Requires validating that Claude reliably honours the "emit tag and stop" instruction before committing. MCP (v3) is the preferred long-term path for CLI and subprocess callers.
- **Metrics**: `milk.tools.tool_agent_calls` and `milk.tools.tool_agent_errors` counters in Phase 4.
