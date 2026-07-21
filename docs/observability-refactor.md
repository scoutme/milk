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

### CLI and file-management helpers

The observability package currently exposes helpers for:

- `/metrics` formatting from `metrics.jsonl`
- `search_signals` across `logs.jsonl`, `traces.jsonl`, and `metrics.jsonl`
- `/otel` file statistics and timestamp bounds
- `/otel trim` archive-and-reset behavior
- file-growth threshold checks via `otel.warn_mb` and `otel.max_mb`

---

## CLI behavior

### `/metrics`

`/metrics` renders the latest value for each metric+label combination in a human-readable list. The output is intentionally stable and sorted so it can be used in tests and manual verification. The latest sample timestamp is shown when available, and the output ends with a short maintenance hint.

### `search_signals`

`search_signals` performs a case-insensitive substring search across `logs.jsonl`, `traces.jsonl`, and `metrics.jsonl`. It returns file name, line number, and a truncated matching line.

### `/otel`

`/otel` shows file sizes, record counts, and best-effort oldest/newest timestamps for each OTel file, plus a total size summary.

### `/otel trim`

`/otel trim` archives the current files with a datestamp suffix and recreates fresh empty files.

### File size thresholds

- `otel.warn_mb`: emit a one-time warning at session start when any file exceeds the threshold.
- `otel.max_mb`: disable OTel for the session when a file exceeds the hard cap.

---

## What changed from the earlier draft

The earlier refactor draft described a broader set of implementation details than the current codebase needs. This document now matches the implemented behavior and the actual CLI surface:

- `/metrics` and `search_signals` are implemented as `internal/obs` helpers.
- File management is handled by `internal/obs/filesize.go`.
- The metric inventory is the canonical source of truth for the observability surface.
- Existing memory and token metrics remain unchanged.

---

## Validation checklist

- [x] `/metrics` includes the new runtime metrics after exercising the relevant code paths.
- [x] `search_signals` can find the new metric names and log events.
- [x] `/otel` reports file sizes, record counts, and age bounds.
- [x] `/otel trim` archives files and recreates fresh ones.
- [x] Session-start warnings and max-cap behavior are honored.
- [x] Memory and token metrics continue to work unchanged.
- [x] Build and tests pass.
