# tool_search design — deferred tool schemas with on-demand loading (issue #480)

Status: design, pre-implementation. Config-gated, default unchanged; no default flips
without the standing evidence bar (A/B across provider classes).

## 1. Problem and evidence

milk sends every registered tool's full schema on every request. Measured baselines
(per-tool serialized bytes, `internal/agent/local` no-store 21-tool set; memory/obs/
board/MCP tools ride on top):

| tier | total | core 10 (coding surface) | tail 11 |
|---|---:|---:|---:|
| full | 15,042 B | 8,194 B | 6,848 B |
| lean | 6,117 B | 3,087 B | 3,030 B |

Core 10: `bash bash_output find_files grep read_file write_file edit_file multiedit
list_dir project_map`. Tail 11: `kill_bash open_file delete_file move_file
http_request web_fetch get_session_context escalate ask_user start_workflow
milk_config_help`.

The merged diets (`tool_description_tier`, `system_prompt_tier`, stable prompt layout)
halved the wording tax. What remains is structural:

- The **tail is ~half the payload even at `lean`** (3,030 of 6,117 B).
- **MCP servers and Agent Plugins add tools unbounded** — the schema share has no
  ceiling. Anthropic's own numbers for a typical 5-server setup: ~55k tokens of
  definitions before any work.
- **Tool selection accuracy degrades beyond 30–50 tools** (Anthropic, "Advanced tool
  use"). milk's built-ins are already ~30; with MCP/plugins the failure mode arrives
  quietly.

Constraint from the product decision (2026-11-27): the full toolset is milk's identity.
We do not ship a stripped core; static subsetting stays available via
`limits.included_tools` / `excluded_tools` for users who want it. The toolset must
stay *whole* — the fix is smarter **selection/discovery**, not fewer tools.

## 2. Standards survey

### 2.1 Anthropic tool search tool (server-side; stable)

- Wire: every tool definition is still sent in `tools`, deferred ones carry
  `defer_loading: true`. The API excludes deferred tools from the system-prompt
  context; a `tool_search_tool_{regex,bm25}` tool (never itself deferred) searches
  names, descriptions, argument names and argument descriptions.
- Discovery returns `tool_reference` blocks (default 5, `limit` 1..10k); the API
  expands them into full definitions inline. References persist in history — reuse
  across turns without re-searching.
- Two query flavors: regex (Python `re`, case-insensitive, ≤200 chars) and BM25
  (natural language, ≤500 chars).
- **Custom client-side search is a first-class pattern**: return `tool_result` with
  `tool_reference` content blocks from your own tool; the API expands them the same
  way. (Relevant for milk: we run the search ourselves for OpenAI-compat providers.)
- Best practices that translate directly: keep 3–5 most-used tools non-deferred;
  namespace tool names (`github_`, `slack_`) so one query matches a group; keywords
  in descriptions matching how users phrase tasks; add a system-prompt line listing
  tool *categories*.
- When to use: 10+ tools, >10k definition tokens, 200+ MCP tools. Not worth it below
  ~10 small tools.
- Prompt caching: deferred tools are excluded from the prefix; `defer_loading` is
  cache-friendly by construction; a deferred tool may not carry `cache_control`.

### 2.2 pi's `tool_search` (client-side; production-proven)

- One tool, fixed description (`# Tool discovery…`), schema `{query, limit?}`
  (default 8), opt-in extension — installed "where the model should find deferred
  tools".
- Ranking: **Okapi BM25 (k1 1.2, b 0.75)** over name / description+parameter text /
  namespace as separate fields (`packages/durable/src/search.ts`):
  - term **coverage beats score** (more distinct query terms matched ranks first);
  - a query term matching the *name* beats any number of text mentions;
  - whole-local-name-in-query bonus (after `__` split) so `mcp__peekaboo__app` wins
    for "peekaboo app";
  - exact-name query ranks that tool first;
  - only text is length-normalized.
- Search text = description + parameter names + descriptions, recursively resolving
  `$ref`s (`schemaText` in `tool-search.ts`).
- Loading: results say `Loaded N tools. They are available from your next call:` and
  the harness **adds them to the conversation's offered set** (`control.addTools`),
  persisted on the conversation branch (survives compaction).
- Result text carries name + first description line only — the full schema goes on
  the wire as a real tool definition on the next request.

### 2.3 MCP and OpenAI-compat: no wire-level standard

- MCP: no deferred-tool concept in the protocol; `tools/list` pagination (cursors) is
  the only scaling story. Anthropic's MCP connector layers `defer_loading` on the
  toolset config (client-side policy, server-side expansion).
- OpenAI-compat chat completions: no `defer_loading`, no `tool_reference`. All tools
  sent up front. But the tool schema is **advisory**: the harness executes whatever
  `name` the model emits, validating args against the *real* schema it owns. This is
  the seam the client-side emulation below exploits.

### 2.4 Takeaway

Two proven shapes: **server-side** (Anthropic: wire carries all definitions, context
carries few) and **client-side growth** (pi: wire carries few, grows as discovered).
milk needs the client-side shape for OpenAI-compat providers (mimo and friends) and
can pass through the server-side shape where it exists (claude-cli / Anthropic
protocol). The design below is the client-side shape with one deliberate deviation
from pi: the offered set does **not** grow — see §3.3.

## 3. Design: stub + load (client-side, cache-stable)

### 3.1 Wire shape

The `tools` array becomes three layers, and — crucially — **it never changes within a
session**:

1. **Pinned tools** — full schemas as today (subject to `tool_description_tier`).
   Default pin set: the core 10 + every tool the prompt rules mandate
   (`record_memory`, `get_memory`, `list_memory`, `forget_memory`, `get_metrics`,
   `create_task`, `update_task`, `list_tasks`, `complete_task`, `current_need`,
   `project_map`) — rules must never reference a tool the model cannot call
   immediately.
2. **`tool_search` tool** — `{query: string, limit?: int}` (default 5, cap 20),
   fixed description: *Searches deferred tool metadata (BM25) and returns full
   schemas for the matches. Some tools — MCP servers, plugins, rarely used built-ins —
   are deferred; search before calling them. For MCP tool discovery always use
   `tool_search`.*
3. **Stubs for every deferred tool** — name + first description sentence + "deferred:
   run `tool_search('<name>')` for its schema before first use". ~30–60 B per tool.
   The stub schema is `{"type": "object"}` (open), so no provider-level shape
   validation blocks a call.

All names always present → the model always knows the toolset exists (milk's identity
intact), the tools block is byte-constant → the prompt-cache prefix never breaks when
a tool is loaded.

### 3.2 `tool_search` execution

- Ranking: pi's BM25 semantics ported to Go (~150 lines): coverage-first, name-field
  boost, exact-name pinning, namespace field for `mcp__server__tool` names, length
  normalization only on body text. Search text = description + parameter names +
  descriptions (schema walk, `$ref`-aware).
- Result: `Loaded N tools:` + for each match the **full schema** (honoring the
  agent's `tool_description_tier`) as tool-result text, plus first-line reminders of
  what else matched beyond the limit. No match → `No matching tools found.` (never an
  error).
- Deferred tools that are *called anyway* (model skipped search): execute if the args
  validate against the real schema — the harness owns the truth and a valid call
  should just work. If validation fails, return a corrective result: `"<name> is
  deferred; its schema was not loaded. Run tool_search('<name>') first."` — one round
  trip, self-teaching.

### 3.3 Why stubs instead of pi's `control.addTools`

pi grows the offered set on discovery; each growth rewrites the tools prefix and
costs a full cache invalidation on OpenAI-compat providers (prefix = system+tools).
milk instead keeps the stub in place and delivers the schema **inside a tool result**:
message-tail content, which rides the existing per-turn cache and never invalidates
anything. The model can call any tool after reading its schema from the search
result — tool schemas guide *argument construction*, and milk validates against the
real schema at execution. Cost: a discovered schema occupies message space (paid
once, then cached as history grows) instead of request-head space.

Compaction note: schema text lives in history, so compaction may drop it; the model
re-searches cheaply. Optional Phase 2: track the loaded set in session state and
re-inject compact schema reminders after compaction (pi's `modelTools.add`
persistence equivalent).

### 3.4 Config surface

Per agent, mirroring the diet knobs' discipline:

```
tool_search: "off" | "auto"          # default "off" — byte-for-byte status quo
tool_search_pin: ["name", ...]       # optional extra pinned names; core+rules set is implicit
tool_search_defer: "non-pinned"      # default; future: "mcp-only" etc.
```

- `auto`: non-pinned tools become stubs; `tool_search` joins the wire.
- Composes with `tool_description_tier`: stubs are always first-sentence; *loaded*
  schemas respect the agent's tier. (Correction to the earlier issue comment: tool
  search is a **loading policy**, orthogonal to the wording diet — not a fourth tier.)
- `config.Validate` warns on unknown values and on `tool_search_pin` names that don't
  resolve to registered tools.

### 3.5 Prompt coupling

- The stable `<milk-context>` block (or system prompt for `classic` layout) gains one
  line: *"N tools deferred (MCP, plugins, rarely used). Use tool_search(query) to
  load schemas; all deferred tools are callable once their schema is known."*
- Rules naming specific tools unchanged (pin set guarantees they're visible).
- Category hint (Anthropic best practice): the line lists the categories present
  (mcp, plugin, network, workflow…), one word each.

## 4. Phasing

**Phase 1 — core (this design's PR):** stubs + `tool_search` + BM25 + corrective
calls, `internal/agent/local` (all OpenAI-compat providers incl. mimo/cloud), config
knobs, budget test (search mode wire size ceiling), wire-e2e test through the
scripted server (`cachewarm_wire_test.go` pattern), payload meter extension
(`result.payload.tools_bytes` naturally reflects stubs; add `tool_search_loads`).

**Phase 2 — persistence + eval:** post-compaction schema re-injection; eval adapter
flag `--tool-search on`; a discrimination scenario where the task requires an
obscure tool across turns (e.g. memory persistence + a plugin/MCP-shaped tool), A/B
`off` vs `auto` on payload, quality, discovery success, tool-call count.

**Phase 3 — native passthrough:** providers that speak `defer_loading` /
`tool_reference` (claude-cli → Claude Code tool sets; Bedrock InvokeModel for Anthropic
models) get the server-side shape — zero client emulation, definitions stay in the
request but out of context.

## 5. Success criteria (eval plan)

- Existing suites at `auto` with default pin set: quality parity (rules-mandated tools
  are pinned, so no behavior change expected) and tools payload −50% or better at
  MCP-heavy fixtures (the 55k-token scenario is the headline).
- New discovery scenario: ≥90% of runs find and correctly use the required deferred
  tool within 2 search calls; no increase in false "tool unavailable" failures.
- Cost/latency: no regression on turn counts; payload meter proves the split.
- Default flips only after evidence across provider classes (long-TTL cloud, local
  small-window) per the standing decision framework.

## 6. Open questions

1. **Strict function-calling providers** that validate emitted args against the wire
   schema would reject stub calls. Mitigation: stub schema is open-object; add a
   `tool_search_strict: off` escape or fall back to `auto`-pinning on such providers.
   (mimo/llama.cpp/vLLM are non-strict today.)
2. **Pin-set drift**: as rules evolve, the implicit pin set must follow. Budget test
   asserts every rule-referenced tool name is registered and pinned.
3. **Frequency-based auto-pinning** (pin what the last N sessions actually used) —
   nice follow-up, not in scope.
4. **Namespace weighting** for `mcp__server__tool` names — pi's `__`-split bonus is
   the starting point; may want server-level field boost for "server X tools" queries.

## References

- Anthropic, *Tool search tool* (platform.claude.com, stable 2025-11): wire protocol,
  `defer_loading`, `tool_reference`, custom client-side search, best practices.
- Anthropic, *Advanced tool use* (engineering blog): 30–50 tool accuracy ceiling,
  55k-token multiserver context cost.
- pi, `packages/durable/src/tool-search/tool-search.ts` + `search.ts`: BM25 ranking
  semantics, fixed tool description, `control.addTools` persistence.
- milk measurements: #480 comments (schema baselines), #468/#474/#472 A/B results.
