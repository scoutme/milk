# 50. Batch `stream-json` JSONL Contract (§6 Catalog + §8.3 Conventions)

- **Status:** accepted
- **Date:** 2026-10-02
- **Issue:** Phase 4b — batch contract hardening: lock the `--output-format
  stream-json` JSONL contract so downstream consumers (the eval harness, CI log
  pipelines, external dashboards) can rely on it.

## Context

[ADR-0049](0049-machine-readable-wire-contract.md) ratified the overall wire
contract: ACP v2 as the embedding wire, Claude-Code-shaped batch JSONL as the
batch wire, with the design doc's §6 catalog and §8.3 conventions as normative
detail. Phase 4b hardens the batch side into a *checkable* contract: a
published JSON Schema ([docs/schema/stream-json.schema.json](../schema/stream-json.schema.json)),
golden recordings
([internal/transport/streamjson/testdata/events/](../../internal/transport/streamjson/testdata/events/)),
and a real consumer — the eval harness's `milk-tui` adapter now drives milk
with `--output-format stream-json` and consumes the JSONL (no session-file
scraping). With real consumers in the wild, the catalog can no longer drift
casually: its exact shape, its evolution rule, and its supersession rule must
be pinned in their own ADR, referencing §6/§8.3 verbatim rather than through
the longer design narrative.

## Decision

### 1. The §6 catalog is the locked line set

Every `--output-format stream-json` stdout line is a UTF-8 JSON object — one
trailing `\n`, nothing else on stdout — with this envelope
(design §6, locked):

```jsonc
{"type": "...", "session_id": "sess_...", "seq": 17, "ts": "2026-10-02T12:00:00.123Z"}
```

- `type` (required string discriminator), `session_id` (required), `seq`
  (required, monotonic per run), `ts` (optional), `parent_tool_use_id`
  (optional — present exactly when the actor is nested: sub-agent, tool-agent,
  workflow stage).
- Line set (the §6 catalog, locked):
  - `system` (with `subtype`) — `init` (first line, always: `cwd`,
    `milk_version`, `agent`, `escalation_agent`, `tools`, `mcp_servers`,
    `route`, `session_state`, `warnings`, `capabilities`), `agent_switch`,
    `route`, `notification`, `warning`, `state` (`session_state` +
    `stop_reason` on idle), `task_started`/`task_progress`/`task_notification`,
    `background_tasks_changed`, `memory`, `commands`, `config_option`,
    `permission_denied`, `error`;
  - `stream_event` — partial deltas only (`partial_messages_v1`): `content_block_delta`
    (`text_delta` / `thinking_delta`) and `tool_args_delta` (`tool_use_id`,
    `partial_json`);
  - `assistant` — completed message blocks: `text`, `thinking` (reasoning
    preserved verbatim per [ADR-0042](0042-preserve-reasoning-content.md)),
    `tool_use` (`id`, `name`, `input`);
  - `user` — `tool_result` content blocks + `tool_use_result` summary
    (`path`, `is_error`, `summary`);
  - `result` (last line, always, exactly one) — `is_error`, `num_turns`,
    `duration_ms`, final `result` text, `stop_reason`, `route_history`,
    `usage` + per-model `model_usage`, `subtype` (`success |
    error_during_execution | error_max_turns | interrupted | refused`).

The machine-checkable form of this catalog is
[docs/schema/stream-json.schema.json](../schema/stream-json.schema.json); the
golden recordings in `internal/transport/streamjson/testdata/events/` are its
executable examples and the fixture source for consumer tests (e.g.
`eval/adapter_milk_test.go`).

### 2. The §8.3 conventions are locked (design §8.3, verbatim rules)

- **snake_case fields** (`input_tokens`, `cache_read`, `cache_creation`,
  `is_error`, `session_id`, `total_cost_usd` where cost exists) — matching
  session files, eval reports and `MilkEvent`. (The ACP side uses ACP's
  camelCase shapes verbatim — never rename standard fields.)
- **`type` discriminator per line; `subtype`** for the `system`/`result`
  families. **Open-set enums everywhere** — consumers must ignore values they
  don't recognize (result `subtype` known set above, with `is_error` as the
  boolean contract).
- **stdout = events only; stderr = prose/logs.** Everything `run()` prints as
  `[milk]` is mirrored as `system/init.warnings` or `system/warning`.
- **Exactly one terminal `result`** per one-shot run — the last `stream-json`
  line, or the whole `--output-format json` document.
- **Never ANSI** — machine transports emit no escape sequences.
- **Whole-document exports stay `json.MarshalIndent`** (sessions, config, eval
  reports, `--output-format json`) — `stream-json` is the only streaming
  serialization and the only place with per-line framing.

### 3. Evolution: additive-only within `stream_v1`

Capability `stream_v1` covers the locked catalog above. Within it, evolution is
**additive-only**:

- new optional line fields, new `system` subtypes, new `stream_event.event`
  types and new open-enum values may be added;
- existing fields never change name, type or meaning; fields are never removed
  or made required;
- every additive change updates the JSON Schema and adds/extends a golden in
  the **same change**, so `docs/schema/stream-json.schema.json` always
  validates the goldens (`check-jsonschema` or the in-repo contract test in
  `internal/transport/streamjson`);
- a consumer written against today's `stream_v1` keeps working against every
  additive revision (the open-set rule makes unknowns ignorable).

### 4. Supersession rule

**Any shape change** to the §6 catalog or the §8.3 conventions — renaming,
retyping, narrowing, removing, making optional fields required, changing the
framing (`seq`, one-line-per-event, terminal `result`), or leaving
`stream_v1` for a new capability — **requires a superseding ADR**. Additive
additions per rule 3 do not. Until such an ADR exists, this document and
ADR-0049 are the binding contract; where the design doc's prose and this ADR
ever disagree on the batch JSONL shape, **this ADR wins** (it is the narrower,
later lock) and the design doc must be corrected — its §6/§8.3 text remains the
normative field-level catalog.

## Alternatives Considered

- **Leaving the batch shape "Claude Code's, roughly".** Rejected: the §6
  catalog already diverges deliberately (`tool_args_delta` instead of
  `input_json_delta`, `cache_creation` naming, milk `system` subtypes,
  `route_history`); "close to Claude Code" is not a checkable contract.
- **Schema-only (no ADR).** Rejected: a schema can be regenerated casually;
  the supersession rule is what makes the lock binding on *milk itself*, not
  just on the validator.
- **Freezing the catalog exactly (no additive rule).** Rejected: additive-only
  within a declared capability is what lets `tasks_v1`/`workflows_v1`-style
  surfaces grow without a new ADR per field, while still blocking drift.

## Consequences

- `docs/schema/stream-json.schema.json` + the goldens under
  `internal/transport/streamjson/testdata/events/` are the checkable form of
  the contract; CI or reviewers can validate with `check-jsonschema` or
  `go test ./internal/transport/streamjson/...`.
- Consumers are pinned to `stream_v1` semantics: ignore unknown types,
  subtypes, enum values and fields; require `system`/`init` first and exactly
  one terminal `result` last.
- The eval harness's `milk-tui` adapter is the first in-repo consumer and the
  reference for external ones (`eval/adapter_milk.go` + its fixture tests).
- [docs/tooling.md](../tooling.md) "Machine-readable output" and the
  selfdocs `machine-readable-output` topic carry the human summary with links
  here and to the schema.
