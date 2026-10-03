# `stream-json` goldens — recorded contract examples

Golden recordings of `milk "prompt" --output-format stream-json` stdout, one
file per recorded one-shot run (the whole run: the JSONL event stream from
`system`/`init` to the terminal `result`). They are the executable form of the
locked batch contract: [ADR-0050](../../../../docs/adr/0050-batch-stream-json-contract.md)
(§6 catalog + §8.3 conventions, design doc
[docs/machine-readable-output-design.md](../../../../docs/machine-readable-output-design.md)).

## Files

- `events/*.jsonl` — golden runs. One UTF-8 JSON object per line, `seq`
  monotonic, `system`/`init` first, exactly one terminal `result` last.
  - `basic_turn.jsonl` — `system/init` meta, `system/state` transitions,
    `stream_event` `text_delta`/`thinking_delta` partials, a completed
    `assistant` message, `result` with `usage` + `model_usage`.
  - `tool_use_turn.jsonl` — `tool_args_delta` partial-JSON fragments,
    `assistant` `tool_use`, `user` `tool_result` + `tool_use_result` summary
    (with `parent_tool_use_id`), multi-model `model_usage`.
  - `error_result.jsonl` — `system/error` banner followed by the terminal
    `result` (`subtype: error_during_execution`, `is_error: true`) — the
    contract emits the terminal `result` even on failure.
  - `interrupted_result.jsonl` — `subtype: interrupted` with `usage` omitted
    (consumers fall back to summing `model_usage`).
- `expected/<name>.json` — companion parse results for each golden, in the
  eval harness's report shapes (`eval.TokenUsage` / `eval.ToolCall`): the
  turn a compliant consumer extracts from the run. Note the deliberate key
  mapping: the contract's `usage.cache_creation` lands on the eval shape's
  `cache_create` (eval `TokenUsage` predates the contract and keeps its own
  whole-document JSON tags). `expected/*.json` is what
  `eval/adapter_milk_test.go` asserts against.

## Provenance

Recorded against the §6 catalog (which is itself derived from Claude Code's
`SDKMessage` shapes and milk's session/eval conventions), kept in sync by the
contract tests. They double as consumer fixtures — the eval adapter tests
feed these exact recordings through `parseMilkStream`.

## Validating

- in-repo: `go test ./internal/transport/streamjson/...` — checks every golden
  line against `docs/schema/stream-json.schema.json` plus the framing
  invariants (init first, one terminal `result` last, monotonic `seq`, no
  ANSI).
- external: `check-jsonschema --schemafile docs/schema/stream-json.schema.json
  <line>` (the schema validates ONE line at a time; split the JSONL first).

Any change to these goldens must follow ADR-0050's evolution rules
(additive-only within `stream_v1`, schema + goldens updated in the same
change; shape changes need a superseding ADR).
