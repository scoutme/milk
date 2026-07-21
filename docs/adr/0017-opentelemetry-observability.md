# ADR 0017 — OpenTelemetry Observability

milk uses OpenTelemetry as the observability boundary for metrics, traces, and logs. The runtime exports those signals to JSONL files under `~/.milk/otel/`, which keeps the implementation local-only while remaining collector-ready.

The memory system makes observability a concrete requirement: without structured, queryable records it is difficult to verify that Percepts are being stored, recalled, decayed, pruned, and promoted correctly. Routing decisions, turn timing, tool usage, session state transitions, and session-turn accounting are equally important to understand.

## Decision

Use file-based OpenTelemetry exporters for logs, traces, and metrics, and keep all provider/exporter lifecycle management inside `internal/obs`.

## Consequences

- The runtime can inspect observability data without external services.
- `/metrics` and `/otel` can remain simple terminal-facing inspection commands.
- `search_signals` can search the raw JSONL files directly.
- The observability surface stays additive and backward-compatible.

## File growth management

The OTel files are intentionally unrotated by the SDK. milk therefore exposes explicit maintenance controls:

- `/otel` reports file sizes, record counts, and timestamp bounds.
- `/otel trim` archives the current files with a datestamp and recreates empty files.
- `otel.warn_mb` warns when a file grows too large.
- `otel.max_mb` disables OTel for the session when a file exceeds the hard cap.

Self-observability gauges mirror the actual file sizes so growth is visible in the metrics stream itself.

## Coverage notes

The current runtime emits structured observability data for:

- token usage and router scoring/decisions
- memory percept record/recall/consolidation events and gauges
- OTel file growth and maintenance helpers
- search and inspection of raw signal files

This ADR intentionally avoids claiming broader runtime coverage than the implementation currently exposes through `internal/obs`.

## Status

This ADR remains current. Sprint 1 keeps the observability boundary stable and focuses on inspection ergonomics, file maintenance, and documentation alignment.
