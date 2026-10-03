# 49. Machine-Readable Wire Contract: ACP v2 Embedding + Batch JSONL

- **Status:** accepted
- **Date:** 2026-10-02
- **Issue:** editor embedding & machine-readable output — wire-contract ratification

## Context

milk has exactly two presentation layers — the bubbletea TUI (rich,
interactive, unparseable) and one-shot text streaming (parseable only by
re-rendering prose). Integrations have no machine surface to attach to: an
editor that wants to host milk as a managed agent ("GitHub Copilot inside VS
Code" shape — the editor owns the UI, the agent owns the turn loop), or a
CI script / log pipeline that wants to parse a run.

The design survey in
[docs/machine-readable-output-design.md](../machine-readable-output-design.md)
compared ACP, Claude Code `--output-format stream-json`, raw provider streams,
UI-framework streams, LSP and MCP against eight requirements (standard-based,
embeddable, stream-safe, complete, provider-neutral, host-agnostic
presentation, deterministic batch framing, versioned & feature-detectable) and
recommended one canonical event model with two wire shapes: **ACP v2** for
editor embedding (primary target) and **Claude-Code-shaped batch JSONL** for
CI/scripts (secondary). That doc shipped as a PROPOSAL with the wire contract
explicitly deferred "to an ADR once the wire contract is ratified". This ADR
is that ratification; the design doc's §5, §6, §8.2 and §8.3 are its
normative detail.

## Decision

The wire contract is locked as recorded in the design doc, in four rules:

### 1. Embedding wire: ACP v2 (design §4, §5, §7)

`milk serve --acp` is milk's embedding surface: an ACP **v2** agent server,
JSON-RPC 2.0 over stdio, one process serving many sessions. The method map is
the standard's own — client→agent `initialize`, `auth/login`, `auth/logout`,
`session/new`, `session/prompt`, `session/cancel`, `session/list`,
`session/delete`, `session/resume`, `session/close`, `session/set_config_option`;
agent→client `session/update`, `session/request_permission`,
`elicitation/create`, `elicitation/complete`; protocol-level `$/cancel_request`.
Milk-specific payloads (toasts, warnings, memory, route) ride ACP's sanctioned
`Ext*` mechanism (`milk/notification`, `milk/warning`, `milk/memory`,
`milk/route`) and `_meta` fields — never a forked schema. Unknown fields,
enum values and `Ext*` methods must be ignored (the spec's own rule).
Versioning is `initialize.protocolVersion` + capability sets on both sides;
an ACP **v1** bridge is a thin translation layer, added only if v1-only hosts
matter. On this side ACP's camelCase field names are used verbatim — never
rename standard fields.

### 2. Batch wire: JSONL per §6 catalog under §8.3 conventions (design §6, §8.3 — locked)

`--output-format stream-json` emits the §6 event catalog as line-framed JSONL:
one UTF-8 JSON object per line, `type` discriminator (plus `subtype` for the
`system`/`result` families), monotonic `seq`, optional `parent_tool_use_id` for
nested actors (sub-agents, tool-agents, workflow stages), `system`/`init`
first, exactly one terminal `result` last. The §8.3 conventions are locked:

- **snake_case** fields (`input_tokens`, `cache_read`, `is_error`,
  `session_id`, `total_cost_usd` where cost exists) — matching session files,
  eval reports and `MilkEvent`;
- **open-set enums** everywhere — consumers must ignore values they don't
  recognize (result `subtype`: `success | error_during_execution |
  error_max_turns | interrupted | refused`, with `is_error` as the boolean
  contract);
- **stdout = events only; stderr = prose** — `[milk]` human logs stay on
  stderr and are mirrored as `system/init.warnings` / `system/warning` events;
- **exactly one terminal `result`** per one-shot run;
- **never ANSI** — machine transports emit no escape sequences; TUI
  colorization is routed through `colorize()` so it cannot leak into events.

Evolution is additive-only within capability `stream_v1`; any shape change to
the §6 catalog or the §8.3 conventions requires a superseding ADR.

### 3. Normalization at the model layer (design §8.2)

Every provider signal — OpenAI-compatible deltas (`content`,
`reasoning_content`, `tool_calls`), Ollama `thinking`, Bedrock Converse events,
`claude-cli` `stream-json` (`SDKMessage`, via
`internal/agent/claude/stream.go`), subprocess `MilkEvent` — is normalized
**once, at the model layer**, into one canonical typed event model
(`internal/events`). The three transports — `internal/transport/streamjson`
(batch), `internal/transport/acp` (embedding), TUI (events → `tea.Msg`) —
subscribe to that union and never see provider signal shapes; provider detail
may ride in an extension field. Reasoning content stays preserved verbatim per
[ADR-0042](0042-preserve-reasoning-content.md).

### 4. `MarshalIndent` whole-document rule (design §8.3)

Whole-document exports — session files, config, eval reports,
`--output-format json` (the terminal `result` event alone) — serialize with
`json.MarshalIndent`, matching `milk config`'s whole-document convention.
`stream-json` is the only streaming serialization and the only place with
per-line framing: compact line framing never replaces indented whole
documents, and whole documents never grow line framing.

## Alternatives Considered

- **Claude Code `stream-json` as the embedding protocol.** Rejected: it is a
  simplex batch format — no session lifecycle, no agent→client request/response
  (permission prompts, structured input), no cancellation semantics, no client
  capability negotiation; an editor would have to bolt on a private side channel,
  i.e. the custom protocol requirement 1 forbids. Kept as (and only as) the
  batch format where it excels — milk already parses it natively as a consumer,
  so `claude-cli` turns relay nearly verbatim.
- **Custom WebSocket/HTTP protocol (Copilot-internal style).** Rejected — a
  forked schema would be ACP reinvented badly; a websocket *relay* of the same
  messages is a deployment detail, not a schema.
- **LSP.** Rejected: wrong level — no agent turn model, no tool/permission
  concepts. **MCP.** Rejected: wrong direction — embeds tools into agents, not
  agents into UIs; MCP stays milk's tool-serving side, orthogonal.
- **Vercel AI SDK UI stream as the contract.** Rejected as contract
  (UI-framework-coupled, session-res semantics); its `text-start/delta/end` +
  `tool-input-start/delta/end` framing is still the model for the batch
  `tool_args_delta` event.
- **ACP v1 as the target.** Rejected: v2 is the forward path (create-or-update
  tool calls, `plan_update`, agent-owned terminals, `state_update` turn ends);
  v1 only via the translation bridge above if demanded.

## Consequences

- The machine contract is checkable, not folklore: golden-file tests
  (`testdata/events/*.jsonl`) and a generated JSON Schema for the batch form,
  with the ACP side checked against the upstream published schema in CI.
- [docs/tooling.md](../tooling.md) carries a human-facing "Machine-readable
  output" summary (selfdocs-indexed); the design doc remains the full
  normative catalog and now carries a ratified status pointing here.
- Implementation lands per the design doc §11 phasing (event model + Host
  abstraction → `milk serve --acp` core → parity surface → batch mode +
  contract hardening). This ADR fixes the contract regardless of which phase
  is in flight, so parallel implementation work cannot drift the wire shape.
