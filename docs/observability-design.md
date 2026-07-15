# milk observability — refactor spec

## Current state

milk uses the `internal/obs` package as the single OpenTelemetry boundary. Metrics, traces, and logs are exported to JSONL files under `~/.milk/otel/`, and the CLI exposes `/metrics`, `/otel`, `/otel trim`, and `search_signals` for inspection and maintenance.

The implementation already covers the core memory and token accounting paths. This sprint focuses on keeping the docs aligned with the actual metric names, labels, file-growth behavior, and CLI output.

---

## Design goals

- Keep observability additive and non-breaking.
- Preserve the existing `internal/obs` boundary and file-export approach.
- Keep memory and token metrics unchanged.
- Use OTel-compatible signals so `/metrics` and `search_signals` can consume them.
- Make file growth visible and manageable from the CLI.

---

## Implemented signal model

### Token metrics

- `milk.tokens.prompt`
- `milk.tokens.completion`
- `milk.tokens.total`

Labels:
- `model`
- `agent`

### Memory metrics

- `milk.memory.percept_events`
- `milk.memory.percepts_recorded`
- `milk.memory.percepts_recalled`
- `milk.memory.consolidation.decayed`
- `milk.memory.consolidation.pruned`
- `milk.memory.consolidation.promoted`
- `milk.memory.global_size`

Labels used by the memory package:
- `event`
- `producer`
- `scope`

### Router metrics

- `milk.router.decisions`
- `milk.router.score`
- `milk.router.classify_latency_ms`

Labels used by router metrics:
- `target`
- `rule`
- `model`

### File-growth metrics

- `milk.otel.logs_bytes`
- `milk.otel.traces_bytes`
- `milk.otel.metrics_bytes`

### Runtime instrumentation currently emitted by the codebase

The following runtime paths emit observability data today:

- turn lifecycle token usage and session turn counters
- router scoring and decision routing
- memory percept record/recall/consolidation events and gauges
- OTel file growth gauges and file maintenance helpers
- search and inspection of raw signal files
- session state transitions and session-end summaries where those paths already emit metrics
- tool usage, permission outcomes, and tool-agent dispatch outcomes where those paths already emit metrics
- Claude subprocess token usage parity

This sprint keeps the instrumentation additive and avoids renaming existing metrics or labels.

---

## Label conventions

The implemented metrics use a small, consistent label vocabulary:

- `agent`: `primary`, `escalation`, or `router`
- `model`: model identifier
- `event`: `record`, `recall`, or `consolidate`
- `producer`: memory producer label
- `scope`: memory scope label
- `target`: `local` or `escalation`
- `rule`: router rule label

The docs intentionally avoid promising labels that are not present in the implementation.

---

## CLI surfaces

### `/metrics`

`/metrics` renders the latest value for each metric+label combination in a stable, human-readable list. The output is sorted, includes the latest sample timestamp when available, and ends with a short maintenance hint.

### `search_signals`

`search_signals` performs a case-insensitive substring search across the OTel JSONL files and returns matching lines with file name and line number. Results are truncated to keep the output readable.

### `/otel`

`/otel` reports:

- file sizes
- record counts
- oldest/newest timestamps when available
- total size

### `/otel trim`

`/otel trim` archives the current files with a datestamp suffix and recreates empty files so the session can continue with a clean slate.

### Threshold controls

- `otel.warn_mb`: warn once per session when any file exceeds the threshold.
- `otel.max_mb`: disable OTel for the session when any file exceeds the threshold.

---

## What remains unchanged

- The `internal/obs` boundary remains the only observability integration point.
- Memory metrics remain unchanged.
- Token metrics remain unchanged.
- Existing agent, router, and memory behavior remains additive and non-breaking.

---

## Validation checklist

- [x] New metrics appear in `metrics.jsonl` after exercising the relevant code paths.
- [x] `search_signals` can find the new metric names and log events.
- [x] `/metrics` reports the new values in a human-readable way.
- [x] `/otel` reports file sizes, record counts, and age bounds.
- [x] `/otel trim` archives current files and creates fresh empty files.
- [x] Session-start warnings and max-cap behavior are honored.
- [x] Existing memory and token metrics continue to work unchanged.
- [x] The project builds and tests pass.
