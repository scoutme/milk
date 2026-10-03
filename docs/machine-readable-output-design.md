# editor embedding & machine-readable output (ACP + `stream-json`) — design proposal

> **Status: RATIFIED — the wire contract is locked in
> [ADR-0049](adr/0049-machine-readable-wire-contract.md)** (ACP v2 as the
> embedding wire; batch JSONL §6/§8.3 as the locked batch contract; §8.2
> normalization-at-model-layer; the `MarshalIndent` whole-document rule).
> Implementation proceeds per §11 phasing — the contract below holds
> regardless of which phase is in flight. This doc answers: *how should `milk`
> present itself to a machine — rich enough that an editor (or any external
> UI) can host it and reach parity with the TUI without embedding it?*
>
> **Status note (phase 4b — contract hardening, docs, eval):** the batch
> contract is additionally locked in [ADR-0050](adr/0050-batch-stream-json-contract.md)
> (§6 catalog + §8.3 conventions, additive-only within `stream_v1`, superseding
> ADR required for shape changes) with its machine-checkable form —
> [docs/schema/stream-json.schema.json](schema/stream-json.schema.json) and the
> golden recordings under `internal/transport/streamjson/testdata/` — and a
> real consumer: `eval/adapter_milk.go` spawns `milk --output-format
> stream-json` and decodes it via `streamjson.Decoder`. The typed event model
> + JSONL encoder/decoder live in `internal/transport/streamjson`; the ACP v2
> payload vocabulary lives in `internal/transport/acp`, now with a real
> JSON-RPC stdio loop (`milk serve --acp` — see the Phase 2 status note
> below). This note records status only — the contract below is unchanged.
>
> **Status note (phase 4 — `--output-format` CLI wiring):** `text|json|
> stream-json` landed (`cmd/milk/outputformat.go`, wired in `main.go`'s
> one-shot `run()`), narrower than full §6 fidelity — see this note for what's
> real vs. deferred. `text` is the exact pre-existing code path, provably
> unchanged (no edits to `dispatch.go`/`runner.go`). `json`/`stream-json`
> emit `system/init` first and a terminal `result` always last; the only
> content event is one completed `assistant` message per turn (via the
> existing `onResponse` tap). Deferred, not faked: `tool_use`/`tool_result`
> events (`local.Agent`'s and `claude.Agent`'s tool-use callbacks don't expose
> a stable tool-call ID today — a real API gap in two agent packages, not
> `cmd/milk` plumbing; **accepted cost: every `stream-json` eval run reports
> `tool_calls: 0` until that lands**, even for tool-heavy turns); `stream_event`
> partial deltas (`OnResponseSegment`'s real contract is "once per tool call
> boundary," not token-level streaming — wiring it in would overclaim
> `partial_messages_v1`, so `capabilities` only ever advertises `stream_v1`);
> `system/agent_switch` and multi-hop `route_history` (no role-aware signal
> distinguishes primary's own response from one forwarded through a mid-run
> self-escalation hand-off — `route_history` stays single-hop, `num_turns:1`,
> `assistant.agent` is omitted rather than risked); `Tools`, `MCPServers`,
> `system/state` (no cheap call site enumerates them at the `main.go`
> boundary; both are optional on the wire and the eval adapter already
> tolerates their absence). One accepted, intentional behavior change: a few
> `fmt.Fprintf(out, ...)` diagnostic lines in `dispatch.go` (transient-retry,
> self-escalation, unsupported-workflow notices) go silent under `json`/
> `stream-json` since `out` becomes `io.Discard` — correct per §8.3 (stdout is
> events-only), not a bug.
>
> **Status note (phase 1 — Host interface + internal/events):** landed
> narrower than this doc's own §11 Phase 1 bullet reads literally, for
> concrete reasons found while implementing it — see the Phase 1 plan's
> rationale (preserved in git history on the branch this landed on) for the
> full list. In short: `internal/events` (`internal/events/host.go`) ships
> minimal — just the `Host` interface and its four methods' payload types,
> not the §6 content-event catalog, which has no consumer yet. `Host` is
> implemented by `cmd/milk/host_tui.go`'s `tuiHost`, wrapping the TUI's
> existing `tuiInputReader`/`m.notify` machinery unchanged. Exactly one
> production call site is migrated (`makeLocalPermAsk`, the local-agent
> permission ask) — every other permission/elicitation call site
> (`makeTUIPermissionHandler`, `makePermissionHandler`,
> `buildAskUserQuestionAnswers`) stays on its current path, since those are
> protocol handlers for claude-cli's own control-request wire format (one
> racing a live remote-oversight call), not host-presentation calls; forcing
> them through `Host` now would mean rewriting daily-exercised logic with no
> immediate payoff. `Host.State` is wired to nothing — `m.busy` alone is not a
> clean running/idle signal (at least 4 overlapping gates exist, plus
> `WorkflowQuestionsMsg` sets `busy=false` while actually awaiting input), so
> getting the classification right is deferred as real design work, not
> extraction. None of the 22 existing toast (`m.notify`) call sites are
> migrated — they're synchronous on bubbletea's `Update()` call stack and no
> engine/goroutine code emits a toast today, so there's no real caller to
> prove an async migration against yet. `internal/transport/streamjson` and
> `internal/transport/acp` are untouched. The ANSI-literal fix (`internal/ansi`,
> new package) covers exactly the four `\033[...]` literal sites §11's Phase 1
> bullet names (`cmd/milk/runner.go:596,659`, `internal/agent/local/
> local.go`'s `⚙ calling agent` literal and its now-deleted duplicate
> `dimWrap`) — not the broader "~10 direct `fmt.Fprint` call sites" catalog in
> §8.2, which is plain prose output with no ANSI, and belongs to Phase 4.
>
> **Status note (phase 2 — `milk serve --acp` core):** landed —
> `internal/transport/acp/lifecycle.go` (the inbound `initialize`/
> `session/new`/`session/prompt`/`session/cancel` structs, field names
> verified against the upstream `agentclientprotocol/agent-client-protocol`
> schema/v2, not guessed from this doc's own prose tables — one real
> correction found that way: `PromptResponse` carries only `messageId`, never
> `stopReason`, which rides on a `state_update` notification instead) and
> `internal/transport/acp/stdio.go` (`StdioConn`: a real JSON-RPC 2.0
> transport over any reader/writer, concurrent-in-flight-safe, each incoming
> request dispatched to its own goroutine — required because `session/
> prompt`'s response is held open for the whole turn per the upstream schema,
> so a second session's `session/new` must never stall behind it).
> `cmd/milk/host_acp.go` adapts `events.Host` (Phase 1) onto `acp.ACPHost` for
> **local-provider agents only**; claude-cli-as-escalation gets no new wiring
> and stays on its existing `denyAllHandler` default — its own control-request
> wire format is a separate protocol, out of scope here. Tool-call identity
> (`tool_call_update`) uses the *real* ids both agent packages already
> compute and previously discarded (`local.Agent`'s `toolCall.ID`, `claude.
> Agent`'s `ContentBlock.ID`) — threaded through widened callback signatures,
> not a synthetic FIFO-ordered id, which turned out to be unsafe for both
> providers (local-agent's result order is call-order only by incidental
> implementation choice; claude-cli's tool execution order is opaque to milk
> entirely). `AgentCapabilities.Session` is advertised as the upstream
> schema's monolithic baseline (there's no finer-grained flag covering only
> new/prompt/cancel/update) — `session/list|resume|close` calls get the
> standard JSON-RPC "method not found" error (`acp.MethodNotFoundError`,
> -32601), the correct way to say "not implemented yet," not a capability
> lie. Deferred, unchanged from the design's own catalog: `auth/*`,
> `session/list|resume|delete|close`, `session/set_config_option` dispatch
> (`ConfigState` already exists, stays unwired), `elicitation/create` wiring,
> `plan_update`/workflow mapping, `terminal_update`, the `milk/*`
> `ExtNotification` channels, `available_commands_update`. One fix to shared
> code this required: `internal/session/store.go`'s `Save`/`Drop` did an
> unsynchronized read-modify-write of the shared `index.json` — harmless with
> one session per process (true until now), a real lost-update race once
> `milk serve --acp` runs concurrent sessions; fixed with a package-level
> mutex, purely additive. Verified end-to-end against the real compiled
> binary (`cmd/milk/serve_acp_e2e_test.go`, mirroring `eval/adapter_milk.go`'s
> subprocess pattern) since no real ACP client (Zed, a VS Code adapter, etc.)
> is available in this environment or vendored in the repo — the single-
> session round trip and the "unknown method" error path are both 100%
> reliable; a third test proving two sessions never block each other is
> opt-in (`MILK_ACP_E2E_STRESS=1`) because real-subprocess scheduling in this
> sandboxed environment made it ~25% flaky — the same property is proven
> deterministically and race-clean twice over by other means (`internal/
> transport/acp/stdio_test.go`'s concurrent-request tests at the transport
> layer, and an in-process-only variant hitting `acpServer` directly that
> completed in 5-9ms across 15/15 runs with zero failures), isolating the
> flakiness to the subprocess+OS-pipe layer in this sandbox, not to milk's
> own concurrency.
>
> **Scope decision (recorded):** the primary target is **editor embedding** —
> milk as a managed agent inside an editor ("GitHub Copilot inside VS Code" is
> exactly this shape: the editor owns the UI, the agent owns the turn loop).
> The wire protocol for that target is **ACP (Agent Client Protocol)**. The
> batch `--output-format stream-json` mode remains in scope as a *secondary*
> consumer of the same event model, for CI/scripts/log pipelines.

---

## 1. Problem & target shapes

`milk` today has exactly two presentation layers: the bubbletea TUI (rich,
interactive, unparseable) and one-shot text streaming (parseable only by
re-rendering prose). Integrations have no machine surface to attach to.

There are two integration classes, and the first is the target:

1. **Editor embedding (primary).** An editor spawns milk as a long-lived
   subprocess, drives it over a bidirectional protocol, and renders the UI
   itself (Zed, VS Code, JetBrains, Neovim/Emacs via adapters). Requires:
   session lifecycle, streaming updates, agent→client requests (permission
   prompts, structured user input), interrupts, capability negotiation.
2. **Batch/scripting (secondary).** `milk "prompt"` piped to `jq`, a CI log
   view, a remote dashboard relayed over SSH/websocket. Requires: line-framed
   event stream on stdout, one terminal result, no ANSI, no interaction.

To reach TUI parity an external host must reconstruct, live:

| TUI surface | Data it renders |
|---|---|
| transcript | streamed text deltas, per-message agent attribution |
| tool lines + diffs (`⚙ name: summary`) | tool lifecycle: announce → args → result → diff |
| `/think` panel | reasoning/thinking deltas (streamed, toggleable) |
| status bar | route (primary/escalation), sticky-escalation state, token usage incl. cache, loop/consumption warnings, session state (running/idle/needs input) |
| notification toasts (ADR-0048) | turn-unrelated notifications with command hints + history |
| task panel (F2) / background agents (F3) / workflows (F4) | task lifecycle, live buffers, stage progress |
| permission prompts (ADR-0013/0015) | **bidirectional**: request/response with the user |
| memory panel (F1) | percept/current-need records written during the turn |
| input completion | slash commands + tool names |
| routing decisions | which agent won and why |
| PTY pane | terminal output of spawned processes |

So the contract must cover **meta payloads (init/config/tools/servers),
response streaming (text + reasoning + tool args), lifecycle events (tasks,
workflows, background jobs), notifications, usage, session state, user
prompts (permissions, elicitations), and interrupts** — not just a response
body.

## 2. Requirements

1. **Standard-based.** Prefer an existing, published, versioned schema over a
   custom one. Where milk must extend, extend *additively* in the chosen
   standard's vocabulary.
2. **Embeddable.** Long-lived process, JSON-RPC 2.0 over stdio, full duplex
   (agent→client requests), session lifecycle (create/resume/list/close/
   delete), per-session cancellation, multiple sessions per process.
3. **Stream-safe.** One message per write, flushable per event, survivable
   over SSH/websocket relays. No whole-response buffering.
4. **Complete.** Every TUI-representable datum in §1 is derivable from the
   protocol alone.
5. **Provider-neutral.** Identical shape whether the turn ran on an
   OpenAI-compatible server, Bedrock, `claude-cli`, or a subprocess agent
   (aider/smolagent). Provider detail may ride in an extension field.
6. **Host-agnostic presentation.** milk must not assume it owns the terminal:
   permission prompts, user input and notifications are *host* concerns
   (TUI today, editor client in embedding) behind one interface.
7. **Deterministic framing for batch mode.** stdout = events only. stderr =
   human logs (unchanged `[milk]` warnings). Exactly one terminal `result`
   event per one-shot run. No ANSI escapes ever.
8. **Versioned & feature-detectable.** A consumer can ask "does this peer have
   X?" without parsing version strings.

## 3. Survey of existing standards

| Standard | Framing | Envelope | Meta | Reasoning | Tools | Notifs | Bidirectional | Fit |
|---|---|---|---|---|---|---|---|---|
| **Agent Client Protocol (ACP)** | JSON-RPC 2.0 over stdio | `session/update` notifications + request/response | `initialize` handshake (both sides), `session/new` config | `agent_thought_chunk` | `tool_call`/`tool_call_update` + `tool_call_content_chunk` | `state_update`, `usage_update`, `plan_update`, `available_commands_update`, `session_info_update` | full duplex by design (permission, elicitation, `$/cancel_request`) | **The standard for editor embedding (primary choice)** |
| **Claude Code `--output-format stream-json`** (Claude Agent SDK `SDKMessage`) | JSONL stdout | `type`-discriminated union (~40 members) | `system/init` (model, tools, mcp_servers, capabilities) | `thinking` blocks + `thinking_delta` | `tool_use`/`tool_result` + `input_json_delta` | `system` subtypes: `notification`, `task_*`, `hook_*` | only via a separate stdin control protocol | **Excellent for batch mode (secondary choice)** |
| OpenAI SSE / chat.completion.chunk | SSE | chunk union | minimal | — | `tool_calls` deltas | — | HTTP | Weak (no lifecycle/meta) |
| Ollama NDJSON | JSONL | flat chunks | `done` stats | `thinking` | `tool_calls` deltas | — | HTTP | Weak (no lifecycle) |
| Vercel AI SDK UI stream | SSE/NDJSON | `UIMessageChunk` | parts lifecycle (`text-start/delta/end`, `tool-input-*`) | `reasoning-*` parts | rich | `data-*` | HTTP | Good framing ideas, UI-framework-coupled |
| Codex CLI `codex exec --json` | JSONL | event union | limited | some | some | — | no | Moderate |
| `llm` CLI `--json` | single doc | log record | — | — | — | — | no | Too small |
| LSP | JSON-RPC stdio | — | — | — | — | — | yes | Wrong level: editor↔language tooling, no agent turn model |
| MCP | JSON-RPC | — | — | — | tools | — | yes | Wrong direction: embeds tools *into* agents, not agents *into* UIs |

### Key observations

- **ACP is purpose-built for exactly the target shape** — editors embedding
  coding agents ("LSP for coding agents"). It is JSON-RPC 2.0 over stdio with
  `initialize` capability negotiation on both sides, a session lifecycle
  (`session/new|prompt|cancel|resume|list|close|delete`), streaming
  `session/update` notifications covering text/thoughts/tools/plans/usage/
  state, and — crucially — agent→client requests: `session/request_permission`
  (structured permission prompts with `allow_once|allow_always|reject_once|
  reject_always` options) and `elicitation/create` (structured user input),
  plus `$/cancel_request` and `Ext*` extension messages. Zed implements it
  natively; client/agent adapters exist for VS Code, JetBrains, Neovim/Emacs;
  Gemini CLI, Goose and others speak it as agents. Authoritative JSON Schema
  is published (`schema/v1`, `schema/v2` in `agentclientprotocol/
  agent-client-protocol`) with language bindings.
- **ACP has two protocol versions, v1 and v2** (negotiated via
  `initialize.protocolVersion`). v2 is the forward path and the documented
  default at agentclientprotocol.com; shipped clients may still speak v1 (see
  the deltas below). milk targets **v2**, with a v1 bridge as a translation
  layer only if v1-only hosts matter (verify client landscape at
  implementation time).
- **Claude Code stream-json is the de-facto standard for batch output** of an
  AI coding CLI. It is simplex (agent→stdout), one-shot, with no session
  lifecycle and no agent→client requests — fine for `jq`, unusable for
  embedding. milk already speaks it natively as a *consumer*
  (`internal/agent/claude/stream.go`, `cmd/milk-mock/claude.go`), so a
  `claude-cli` escalation turn can be relayed nearly verbatim in batch mode.
- Raw provider streams (OpenAI SSE, Ollama NDJSON) and UI-framework streams
  (AI SDK) fail requirement 4/5 — they carry content, not the agent lifecycle.

### ACP v1 → v2 deltas that matter for milk

| Concern | v1 | v2 |
|---|---|---|
| tool lifecycle | `tool_call` (created) + `tool_call_update` | `tool_call_update` (create-or-update) + `tool_call_content_chunk` |
| plans | `plan` | `plan_update` (identified by `PlanId`, entries `pending|in_progress|completed|cancelled`) |
| terminals | client-owned: `terminal/create|output|kill|release|wait_for_exit` | agent-owned: `terminal_update` + `terminal_output_chunk` streamed as session updates |
| file access | client-mediated `fs/read_text_file|write_text_file` | none (agent operates on the FS directly) |
| turn end | `PromptResponse.stopReason` | `state_update` (`running|idle|requires_action`, `IdleStateUpdate.stopReason`: `end_turn|max_tokens|max_turn_requests|refusal|cancelled|error`) |
| sessions | `session/load` + `session/resume` | `session/resume` (with `replayFrom`) |
| auth | `authenticate` / `logout` | `auth/login` / `auth/logout` |
| mode switching | `session/set_mode`, `current_mode_update` | session config options (`config_option_update`, `session/set_config_option`) |

## 4. Recommendation

**Primary: `milk serve --acp` — milk as an ACP v2 agent server.** The editor
spawns `milk serve --acp` on stdio and speaks JSON-RPC 2.0 to it. One process
serves many sessions; each `session/prompt` runs a turn through the existing
router/escalation/tool loop; every event reaches the client as
`session/update`; permissions and structured input round-trip as
`session/request_permission` and `elicitation/create`; interrupts as
`session/cancel`.

**Secondary: `--output-format stream-json|json` on one-shot runs** — the same
canonical event model serialized as Claude-Code-shaped JSONL (plus a single
document variant) for CI/scripts. No interaction: `--permission-mode` /
`--allow-tool` decide locally.

**Layering rule: one canonical event model, three consumers.**

```
providers (local/bedrock/claude-cli/subprocess)
        │ normalized deltas + lifecycle signals
        ▼
 internal event model  (typed Go union — the single source of truth)
        │
        ├── TUI host        (bubbletea: renders panels/status/toasts)
        ├── ACP transport   (JSON-RPC: session/update, request_permission, …)
        └── JSONL transport (--output-format stream-json: §6 lines)
```

Nothing above the transport layer may know which host is attached. This is
requirement 6 and it is also the main refactor §8 describes.

This satisfies every requirement:

1. **Standard-based** — the embedding wire format is a published schema with
   published types; milk's extensions use ACP's own `Ext*`/`_meta` mechanisms.
2. **Embeddable** — ACP's session lifecycle, duplex requests and cancellation
   are the standard's core; nothing to invent.
3. **Stream-safe** — JSON-RPC messages are one-per-write; batch mode JSONL is
   line-framed.
4. **Complete** — §7 shows a TUI-parity coverage map with no gaps.
5. **Provider-neutral** — normalization (§8.2) maps every provider's deltas
   into the same event shapes.
6. **Host-agnostic** — §8.1's `Host` interface.
7. **Deterministic batch framing** — §8.3.
8. **Versioned** — ACP `initialize.protocolVersion` + capability sets on both
   sides; batch mode keeps `capabilities[]` on `system/init`.

### Alternatives considered

- **Claude Code stream-json as the embedding protocol.** Rejected: it is a
  simplex batch format. No session lifecycle, no request/response, no
  cancellation semantics, no client capability negotiation; an editor would
  have to bolt a private side-channel on (which is precisely the custom
  protocol requirement 1 forbids). Kept as the batch format where it excels.
- **Custom WebSocket/HTTP protocol (Copilot-internal style).** Rejected —
  requirement 1; would be ACP reinvented badly. (A websocket *transport* for
  the same ACP messages is a deployment detail later, not a schema.)
- **LSP.** Rejected: no agent turn model, no tool/permission concepts; ACP is
  the ecosystem's answer to "LSP for agents".
- **MCP.** Wrong direction (tools into agents, not agents into UIs). MCP stays
  milk's tool-serving side, orthogonal.
- **Vercel AI SDK UI stream as the contract.** Rejected as contract (web-app
  protocol with session-res semantics); its `text-start/delta/end` +
  `tool-input-start/delta/end` framing is still the model for emitting partial
  tool-argument fragments in batch mode.

## 5. CLI surface

```
milk serve --acp                 # ACP v2 agent server on stdio (the target)
milk [flags] "prompt" --output-format stream-json   # batch event stream (secondary)
milk [flags] "prompt" --output-format json          # single result document
milk [flags] "prompt"                               # --output-format text (default, unchanged)
```

- `milk serve --acp` is a dedicated entry point (like `milk mcp serve`): it
  performs the ACP `initialize` handshake, then serves sessions until stdin
  closes. Interactive surfaces are protocol-native (§7.2) — there is **no
  custom stdin control channel** in batch mode; batch runs are non-interactive
  by definition (`--permission-mode acceptEdits|bypassPermissions|deny`,
  `--allow-tool <glob>` decide locally, emitting `permission_denied` events).
- `--output-format text|json|stream-json` replaces nothing; `text` is current
  behavior byte-for-byte. (`json` = the `result` event alone, `MarshalIndent`,
  matching `milk config`'s whole-document convention and Claude Code's
  `--output-format json`.)
- `--verbose` is orthogonal (debug logs stay on stderr). Partial deltas are
  **on by default** in `stream-json` with `--no-partial-messages` to degrade
  to block-level events (capability `partial_messages_v1` advertised only when
  active).
- Interactive TUI mode ignores `--output-format` (v1).

## 6. Canonical event model & batch JSONL catalog

The event model is transport-internal; §6 lists its **JSONL serialization**
(`stream-json` mode) because that is the easiest contract to golden-test.
§7 maps every event onto ACP. All batch lines: UTF-8 JSON object, `type`
string discriminator, `subtype` where the standard uses one, snake_case
fields, one trailing `\n`, nothing else on stdout:

```jsonc
{"type": "...", "session_id": "sess_...", "seq": 17, "ts": "2026-10-02T12:00:00.123Z"}
```

`seq` is monotonic per run (gap-tolerant consumers can detect dropped lines on
relays). Optional `parent_tool_use_id` on every event whose actor is nested
(sub-agent, tool-agent, workflow stage) exactly as in Claude Code — this is
what makes F3/F4-style tree views reconstructable.

### 6.1 Meta: `system/init` (first line, always)

```jsonc
{"type":"system","subtype":"init","session_id":"sess_01H…","seq":0,
 "cwd":"/repo","milk_version":"0.9.0",
 "agent":{"name":"qwen-local","provider":"local","model":"qwen2.5-coder-32b",
          "role":"primary","context_window_tokens":131072},
 "escalation_agent":{"name":"claude","provider":"claude-cli","model":"claude-sonnet-5"},
 "tools":["bash","read_file","write_file","edit_file","spawn_background_agent","…"],
 "mcp_servers":[{"name":"github","status":"connected"}],
 "route":{"target":"primary","reason":"…","conclusive":true},   // router.Decision
 "session_state":"idle",          // session state machine incl. sticky-escalation states
 "warnings":["config: unknown key …"],   // everything printed as [milk] stderr pre-turn
 "capabilities":["stream_v1","partial_messages_v1","tasks_v1","workflows_v1"]}
```

This is the whole right-hand side of the status bar + the config-recovery
warnings + the tab-completion tool list, delivered as one meta payload.

### 6.2 Content: assistant text, reasoning, tool lifecycle

Base shapes are Claude Code's, normalized per provider (§8.2):

```jsonc
// text delta (partial_messages_v1)
{"type":"stream_event","seq":3,"event":{"type":"content_block_delta","index":0,
  "delta":{"type":"text_delta","text":"I'll check"}}}
// reasoning delta → same shape, delta.type "thinking_delta"  (drives /think-style panels)

// completed content block (always, even with --no-partial-messages)
{"type":"assistant","seq":8,"agent":"primary",
 "message":{"id":"msg_1","role":"assistant","content":[
   {"type":"text","text":"I'll check the file."},
   {"type":"thinking","thinking":"…"},                      // reasoning, ADR-0042 preserved
   {"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"go.mod"}}]}}

// tool result (TUI's ⚙ summary + diff are derived from these)
{"type":"user","seq":11,"parent_tool_use_id":"toolu_1",
 "message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"…"}]},
 "tool_use_result":{"path":"go.mod","is_error":false,"summary":"38 lines"}}
```

Tool-argument fragments use the AI SDK's explicit start/delta/end framing under
a `stream_event` delta (`tool_args_delta`) rather than Claude's raw
`input_json_delta` string fragments, so partial-JSON reassembly is not the
consumer's job:

```jsonc
{"type":"stream_event","seq":7,"event":{"type":"tool_args_delta","tool_use_id":"toolu_1","partial_json":"{\"pa"}}
```

### 6.3 Lifecycle & meta: `system` subtypes

| subtype | Carries | Renders in an external UI as |
|---|---|---|
| `init` | §6.1 | connection banner, tool/MCP list, capabilities |
| `agent_switch` | `from`,`to`,`reason` (self-escalation hand-off, `dispatch.go:308` notice) | transcript notice + status-bar role flip |
| `route` | `router.Decision` re-emissions mid-session | routing indicator |
| `notification` | `id`,`severity`,`command_hint`,`body` — ADR-0048 toasts (MCP connect, OAuth refresh, background-job lifecycle, config reload) | toast overlay + `/notifications` history ring |
| `warning` | loop-detection / consumption signals (`Signal.Category`, `IsConsumption()`, count/limit facts — issue #173 wording) | status-bar `[⚠ …]` badge |
| `state` | session state (`running|idle|requires_action`) + stop reason on idle | status-bar activity state, input enablement |
| `task_started` / `task_progress` / `task_notification` | `task_id`,`kind`(`background_agent`\|`workflow`\|`tool_agent`),`parent_tool_use_id`, live buffer chunks as `task_progress` deltas | F2/F3/F4 panels + live-attach view (ADR-0047) |
| `background_tasks_changed` | snapshot of running/finished job IDs | panel badges |
| `memory` | `op`(`record`\|`get`\|`list`\|`forget`), `percept_id`, subject | F1 memory panel feed |
| `commands` | available slash commands (+ input hints) | input completion |
| `config_option` | session config option changed (`/think`, `/agent switch`, `/model` as options) | settings UI |
| `permission_denied` | `tool`,`reason` | transcript marker (Claude Code parity) |
| `error` | `is_error:true`, `message`,`recoverable` — startup/turn failures before `result` | error banner (terminal `result` still emitted) |

Claude Code consumers already understand `task_*` and
`background_tasks_changed`; milk's background sub-agents (ADR-0043) and
`start_workflow` stages map onto them 1:1 (workflow stage node IDs become
nested `task_id`s under the `start_workflow` tool call's
`parent_tool_use_id`). Workflow `journal.jsonl` stage transitions surface as
`task_progress` with a `stage` payload (its `StageNode`/`PathSnapshot` structs
already carry JSON tags). Toast `id`s double as the `notification_id` for the
history ring.

### 6.4 Terminal: `result` (last line, always, exactly one)

```jsonc
{"type":"result","subtype":"success","session_id":"sess_…","seq":42,"is_error":false,
 "num_turns":3,"duration_ms":8123,
 "result":"final assistant text","stop_reason":"end_turn",
 "route_history":[{"turn":0,"target":"primary"},{"turn":2,"target":"escalation"}],
 "usage":{"input_tokens":1234,"output_tokens":567,"cache_read":8000,"cache_creation":200},
 "model_usage":{"qwen2.5-coder-32b":{"input_tokens":…,"output_tokens":…,"cache_read":…},
                "claude-sonnet-5":{"input_tokens":…,"output_tokens":…}}}
```

`cache_read`/`cache_creation` are milk's existing snake_case token-cache keys
(session store + `eval/adapter_milk.go` schema) — kept as-is rather than
Anthropic's `cache_read_input_tokens`, with the rationale documented at the
schema level. `subtype` open-set: `success | error_during_execution |
error_max_turns | interrupted | refused` (Claude Code vocabulary) — `is_error`
remains the boolean contract per milk's `is_error` convention. For
`--output-format json` this object (indented) *is* the whole output.

## 7. ACP transport (the target)

`milk serve --acp` implements the **agent side** of ACP v2 over stdio. Method
map (authoritative names from `schema/v2/meta.json`):

- **client→agent (milk implements):** `initialize`, `auth/login`,
  `auth/logout`, `session/new`, `session/set_config_option`, `session/prompt`,
  `session/cancel`, `session/list`, `session/delete`, `session/resume`,
  `session/close`
- **agent→client (milk invokes):** `session/update`, `session/request_permission`,
  `elicitation/create`, `elicitation/complete`
- **protocol-level:** `$/cancel_request` (cancel an in-flight request in
  either direction)
- **extensions:** `ExtRequest`/`ExtResponse`/`ExtNotification` — arbitrary
  method names, the sanctioned way to carry milk-specific payloads

### 7.1 Event model → ACP mapping

| Canonical event (§6) | ACP v2 | Notes |
|---|---|---|
| `system/init` (tools, mcp servers, models) | `initialize` response (`capabilities`, `info`) + `session/new` response (`configOptions`, `availableCommands`) | agent capabilities: `session.prompt`, `session.delete/resume/…` per `SessionCapabilities`; mutable parts flow later as updates |
| text deltas | `session/update` `agent_message_chunk` (`ContentChunk`) | streamed per chunk |
| completed assistant message | `agent_message` (`messageId`, content blocks) | message identity = ACP `MessageId` |
| `thinking_delta` / thinking blocks | `agent_thought_chunk` / `agent_thought` | drives `/think`-style panels |
| user prompt echo / tool results | `user_message_chunk` / `user_message` | tool results ride in tool-call updates in v2 |
| tool lifecycle (`tool_use`/`tool_result`) | `tool_call_update` (create-or-update; `kind`, `status`, `rawInput`, `rawOutput`, `locations`, `content`) + `tool_call_content_chunk` for streamed tool output | milk tool → `ToolKind`: `read_file`→`read`, `edit_file`→`edit`, `bash`→`execute`, `find_files`/`grep`→`search`, `http_*`→`fetch`, thinking tools→`think`, `spawn_background_agent`/`start_workflow`→`other` (their children appear as separate calls); `ToolCallStatus` maps 1:1 incl. `cancelled` |
| partial tool args (batch `tool_args_delta`) | `tool_call_update.rawInput` progressive fills | ACP has no arg-fragment event; v2's create-or-update absorbs it |
| `system/state` | `state_update` (`running` / `idle`+`stopReason` / `requires_action`) | `requires_action` fires when a permission/elicitation is outstanding — TUI's "needs input" state |
| `system/task_*` (background agents, workflows, tool-agents) | nested `tool_call_update`s with `parent` linkage via content/title, + `plan_update` for workflows | workflow `start_workflow` call owns a `PlanItems` (`planId`, entries per stage node: `pending|in_progress|completed|cancelled`, priority `high|medium|low`) — F4 panel = the editor's plan UI |
| `background_tasks_changed` | `tool_call_update` batch (statuses) | panel badges |
| `system/notification` (toasts, ADR-0048) | `ExtNotification` `milk/notification` (`id`, `severity`, `command_hint`, `body`) | ACP has no toast channel; `Ext*` is the sanctioned escape hatch. Clients that ignore extensions lose only turn-unrelated notices |
| `system/warning` (loop/consumption) | `ExtNotification` `milk/warning` | renders in editor status/notifications |
| `system/memory` | `ExtNotification` `milk/memory` | F1 panel feed |
| `system/route`, `agent_switch` | `ExtNotification` `milk/route` + `session_info_update._meta` snapshot | status-bar route/role |
| `system/commands` | `available_commands_update` (`availableCommands`, `TextCommandInput.hint`) | milk's slash commands become editor input completion, natively |
| `system/config_option` | `config_option_update` + client calls `session/set_config_option` | `/think on|off`, `/agent switch`, `/model` as `SessionConfigOption`s (v2 replaced v1's `session/set_mode`) |
| permission prompt (ADR-0013 suggestions) | `session/request_permission` (`title`, `description`, `subject` = tool call or command, `options[]` with `PermissionOptionKind` `allow_once|allow_always|reject_once|reject_always`) → outcome `selected(optionId)|cancelled` | maps field-for-field onto milk's structured permission records |
| structured user input prompts | `elicitation/create` (form/select schema: `ElicitationSchema`, `EnumOption`, `MultiSelectItems`) → `elicitation/complete` | ACP's structured-input mechanism; no custom dialog protocol needed |
| interrupt | `session/cancel` (client→agent) | aborts the in-flight turn; terminal update carries `stopReason: cancelled`/`cancelled` |
| PTY pane (ADR-0047-ish process output) | `terminal_update` + `terminal_output_chunk` (agent-owned terminals, v2) | v2's terminal model is agent-owned: milk runs the PTYs and streams output — exactly milk's `internal/livebuf` + `cmd/milk/attach.go` shape |
| `result` (§6.4) | `session/prompt` response (`messageId`) + terminal `state_update` (`idle`, `stopReason`) + `usage_update` | v2 ends turns via state, not a result blob; `usage_update` carries `used`/`size` (context window) + `cost` (`amount`,`currency`) — milk maps `cache_read`→used-context accounting and emits `cost` only when a pricing table exists |
| multi-turn metadata | `session_info_update` (`title`, `updatedAt`, `_meta`) | `_meta` reserved for milk's route/session-state snapshot |
| request cancellation | `$/cancel_request` | cancels permission/elicitation requests in flight |

Coverage map (batch mode §6 forms in parentheses):

| TUI surface | ACP | Gap? |
|---|---|---|
| transcript (text) | `agent_message_chunk` / `agent_message` (`stream_event`, `assistant`) | none |
| reasoning (`/think`) | `agent_thought_chunk` (`thinking_delta`) | none |
| tool lines + diffs | `tool_call_update` (`tool_use`/`tool_result`) | none |
| status bar | `state_update`, `usage_update`, `session_info_update._meta`, `milk/route` (`init`, `route`, `result.usage`) | none |
| loop/consumption warnings (#173) | `milk/warning` (`system/warning`) | none |
| toasts + history (ADR-0048) | `milk/notification` (`system/notification`) | none |
| tasks/background agents/workflows | `tool_call_update` tree + `plan_update` (`task_*`, `background_tasks_changed`) | none |
| live-attach view (ADR-0047) | `tool_call_content_chunk`, `terminal_output_chunk` (`task_progress`) | none |
| permission prompts | `session/request_permission` (`--permission-mode` flags in batch) | none |
| structured input prompts | `elicitation/create` (batch: not applicable) | none |
| memory panel | `milk/memory` (`system/memory`) | none |
| input completion | `available_commands_update` (`system/commands`) | none |
| input history, selection/copy, welcome screen | — | intentionally out of scope (client chrome, not agent state) |

### 7.2 Session lifecycle ↔ milk sessions

- `session/new` (`cwd`, `additionalDirectories`, `mcpServers`) creates an
  entry in `internal/session` (or a fresh in-memory session); `session/resume`
  (`replayFrom`) replays from the session store — milk's session files already
  journal enough to feed replay; `session/list|close|delete` map onto the
  store. Sticky-escalation state and route history are per-session and ride
  in `session_info_update._meta`.
- `NewSessionRequest.mcpServers` lets the editor inject MCP servers (client
  sidecar shape). Decision: accept and merge them with `~/.milk/config.json`
  servers; milk's `internal/mcp` + `internal/mcpauth` stay the implementation
  (MCP-over-ACP proxying is still `unstable` upstream — revisit when it
  stabilizes).
- One process, many sessions: the ACP server runs one `internal/agent/local.Manager`
  + `dispatchAgents` shared across sessions (same as TUI mode — unlike one-shot
  mode, background sub-agents and workflows are first-class here).

## 8. Emission model & refactor (implementation shape)

### 8.1 Host abstraction (the core refactor)

milk's interactive surfaces are currently TUI-shaped calls sprinkled through
`cmd/milk`. They become one interface, implemented by three hosts:

```go
type Host interface {
    Notify(Event)                              // toasts, warnings, route, memory…
    RequestPermission(PermissionRequest) (PermissionOutcome, error)  // ADR-0013/0015
    Elicit(ElicitationRequest) (ElicitationResult, error)            // structured input prompts
    State(StateUpdate)                         // running/idle/requires_action
}
```

- **TUI host** — today's behavior: renders prompts/panels/toasts via `tea.Msg`.
- **ACP host** — `session/request_permission`, `elicitation/create`,
  `ExtNotification`s, `state_update`.
- **Headless host** — batch mode: applies `--permission-mode`/`--allow-tool`
  without round-trips, emits `permission_denied` events.

Concretely this extracts from `cmd/milk`: the permission prompt path
(`main.go:859` writes a prompt to `os.Stdout` — becomes `Host.RequestPermission`),
toast dispatch (ADR-0048), structured user-input prompts, and the status-bar
data feed. The turn loop, router, dispatch, agents and memory stay untouched
above the interface.

### 8.2 One emitter, zero direct writes

Non-interactive output already flows through `out io.Writer` plus four
`TurnCallbacks` (`onResponse`, `onSegment`, `onWorkflowStart`, tool hooks
`WithOnThinking`/`WithOnToolUse`/`WithOnToolUseReady`/`WithOnToolResult`,
`onTokens`) — two wrappers (`activityWriter`, `switchWriter`) prove the
pattern. The plan: a typed event union in a new `internal/events` package (the
canonical model of §6/§7) fed by these callbacks; transports subscribe:

- `internal/transport/streamjson` — `Encoder.Encode` per-line JSONL + `seq`
  stamping (batch mode);
- `internal/transport/acp` — maps events onto `session/update` kinds and
  `ExtNotification`s (embedding mode);
- TUI — converts events to `tea.Msg` (mostly an extraction of existing code).

The ~10 direct `fmt.Fprint(out, …)` call sites (dispatch notices,
`printToolLine`, tool diffs, claude stream deltas, subprocess `[error: …]`, the
rogue `router.go:106` `fmt.Printf` and `main.go:859` permission `os.Stdout`
write) each become event emissions. Events that today reach **no** writer
(thinking, tool results, router decisions, token usage, percept records,
background lifecycle, workflow progress) are exactly the hooks that exist but
are `nil` in one-shot mode — the transports wire them non-nil, and one-shot
mode gains a `dispatchAgents` (tool-agents + `local.Manager`) for the first
time so `spawn_background_agent` and `start_workflow` are legal under
`stream-json` (their panels are the whole point for an external UI). The
hardcoded `\033[2m` literals (`runner.go:596,659`, `local.go:2374,2627`) are
routed through `colorize()` so machine transports are guaranteed ANSI-free.

Provider normalization (applies to all transports once, at the model layer):

| Provider signal | Event-model form | ACP form (§7.1) |
|---|---|---|
| OpenAI `choices[].delta.content` | text deltas + text blocks (boundaries via existing `classifyStreamResult`) | `agent_message_chunk` / `agent_message` |
| `delta.reasoning_content` / Ollama `thinking` | thinking deltas/blocks (ADR-0042 preserved verbatim) | `agent_thought_chunk` / `agent_thought` |
| `delta.tool_calls[]` (index-scoped partial `arguments`) | `tool_args_delta` accumulation → one tool call | progressive `tool_call_update.rawInput` |
| claude-cli `stream-json` | parsed `SDKMessage` shapes (round-trip through `internal/agent/claude/stream.go`); batch transport can relay near-verbatim (`system/raw` for unknown subtypes) | mapped to `tool_call_update`/chunks |
| subprocess `MilkEvent` (`internal/agent/subprocess/parse.go`) | mapped to the same union (already shaped after this design) | same |
| `session.TokenUsage` (`cache_read`, `cache_creation`) | `result.usage` / `model_usage` | `usage_update` |
| Bedrock Converse events | same shapes via the existing native path | same |

### 8.3 Batch-mode conventions (locked)

- **snake_case** fields (`input_tokens`, `cache_read`, `is_error`,
  `session_id`, `total_cost_usd` where cost exists) — matches session files,
  eval reports, `MilkEvent`, journal entries. (ACP side uses ACP's camelCase
  shapes verbatim — never rename standard fields.)
- **`type` discriminator per line**; `subtype` for `system`/`result` families;
  open-set enums everywhere ("ignore values you don't recognize").
- **stdout = events only; stderr = prose/logs.** Everything `run()` prints as
  `[milk]` today is mirrored as `system/init.warnings` or `system/warning`.
- **Whole-document exports stay `json.MarshalIndent`** (sessions, config, eval
  reports, `--output-format json`) — `stream-json` is the only streaming
  serialization and the only place with per-line framing.

## 9. Versioning & extensibility

- **ACP**: negotiate `initialize.protocolVersion` (v2 primary; a v1 bridge is
  a thin translation layer per the §3 delta table, added only if demanded).
  Capabilities on both sides (`AgentCapabilities.session`, `SessionCapabilities`,
  client `ClientCapabilities.auth/elicitation`) feature-detect; unknown fields,
  enum values and `Ext*` methods must be ignored (the spec's own rule).
- **milk extensions** ride ACP's sanctioned `Ext*` mechanism
  (`milk/notification`, `milk/warning`, `milk/memory`, `milk/route`) and
  `_meta` reserved fields — never a forked schema.
- **Batch mode** keeps `system/init.capabilities: []` (open-set strings:
  `stream_v1`, `partial_messages_v1`, `tasks_v1`, `workflows_v1`); evolution is
  additive-only within `stream_v1`.
- Schema source of truth: Go types in `internal/events` with full JSON tags +
  a golden-file test (`testdata/events/*.jsonl`) and a generated JSON Schema
  published under `docs/` for the batch form; the ACP side is checked against
  the upstream published schema in CI. The contract is checkable, not folklore.

## 10. Out of scope / future work

- **TUI mirroring** (`milk attach --json` streaming a live session) — natural
  follow-up; same event model, different source.
- **SSE/WebSocket transport** for browser frontends — ACP-over-relay or a shim
  emitting the same events; trivial once the transports exist.
- **ACP v1 bridge** — only if v1-only hosts matter (§3 delta table is the
  work estimate).
- **MCP-over-ACP proxying** and upstream **subagents RFD** (unstable upstream
  as of 2026-10) — milk's `spawn_background_agent` maps onto plain nested tool
  calls for now; revisit when the RFD lands.
- **Cost accounting** — `result.total_cost_usd`/ACP `Cost` reserved until milk
  has a pricing table.
- **Structured output / JSON schema mode** (`result.structured_output` parity).

## 11. Phasing

1. **Phase 1 — event model + Host abstraction (pure refactor).**
   `internal/events` union; `Host` interface extracting permission/toast/
   elicitation/state from `cmd/milk`; TUI re-hosted on it with zero behavior
   change; ANSI literals routed through `colorize()`. Unblocks everything
   else; land behind no flags.
2. **Phase 2 — `milk serve --acp` core (the target).** `initialize`,
   `session/new|prompt|cancel|resume|list|close|delete`, `agent_message(_chunk)`,
   `agent_thought(_chunk)`, `tool_call_update`, `state_update`, `usage_update`,
   `session/request_permission`. Goal: milk usable as an agent in an ACP
   client with transcript + tools + permissions parity.
3. **Phase 3 — parity surface.** `plan_update` (workflows → F4),
   background-agent tool trees + `tool_call_content_chunk` (F3/attach),
   `terminal_update|terminal_output_chunk` (PTY pane), `elicitation/create`
   (structured input), `available_commands_update` + `session/set_config_option`
   (slash commands, `/think`, `/agent switch`, `/model`), `ExtNotification`s
   (toasts, warnings, memory, route), `session_info_update._meta`.
4. **Phase 4 — batch mode + contract hardening.**
   `--output-format stream-json|json` transports, `--permission-mode`/
   `--allow-tool`, golden files + generated JSON Schema, docs
   (`docs/tooling.md` §machine-readable output + selfdocs topic), eval harness
   adapter consuming `stream-json` (kills the bespoke `milkSessionFile`
   scraping in `eval/adapter_milk.go`), ADR.

## 12. Sources

- ACP schema (authoritative): `agentclientprotocol/agent-client-protocol`
  `schema/v1/schema.json`, `schema/v2/schema.json`, `schema/{v1,v2}/meta.json`
  (method maps quoted in §7)
- ACP protocol docs: https://agentclientprotocol.com/protocol/v2/prompt-lifecycle ,
  `/session-setup`, `/tool-calls`, `/extensibility` (and `/protocol/v1/…`)
- Claude Code CLI reference & headless docs: https://code.claude.com/docs/en/cli-reference , https://code.claude.com/docs/en/headless
- Claude Agent SDK streaming & `SDKMessage` types: https://code.claude.com/docs/en/agent-sdk/streaming , `@anthropic-ai/claude-agent-sdk` `sdk.d.ts`
- MCP spec (framing/capability conventions): https://modelcontextprotocol.io/specification/2025-06-18/basic/
- OpenAI streaming events: https://platform.openai.com/docs/guides/streaming
- Ollama API NDJSON: https://docs.ollama.com/api
- Codex CLI `--json`: https://developers.openai.com/codex/noninteractive
- Vercel AI SDK UI stream (`UIMessageChunk`): https://ai-sdk.dev/docs/ai-sdk-ui/stream-protocol
- milk internals referenced: `cmd/milk/main.go` (`run`, permission write),
  `cmd/milk/dispatch.go` (turn hooks, escalation chain), `cmd/milk/runner.go`
  (`switchWriter`), `cmd/milk/ansi.go` (`activityWriter`, `milkTag`),
  `cmd/milk/attach.go` + `internal/livebuf` (ADR-0047),
  `internal/agent/claude/stream.go`, `internal/agent/subprocess/parse.go`
  (`MilkEvent`), `internal/router/router.go` (`Decision`), `internal/session`
  (token keys, store), `eval/report.go` (JSON tags), ADRs
  0013/0015/0042/0043/0044/0047/0048.
