# 17. OpenTelemetry-native Observability with File Exporters

Date: 2026-05-15

## Status

Accepted

## Context

milk has no observability beyond ad-hoc `fmt.Fprintf` to stderr. The memory system (ADR-0016) makes this a concrete problem: without structured, queryable records it is impossible to verify that Percepts are being stored, recalled, decayed, pruned, and promoted correctly. Routing decisions and agent turn timing are equally opaque.

The TECO architecture guidelines mandate OpenTelemetry for traces and metrics, and structured logs at DEBUG/INFO/ERROR. The Go OTel SDK (v1.43) supports all three signals with local file exporters that require zero infrastructure.

## Decision

Adopt the OTel Go SDK for all three signals (logs, traces, metrics). Use `stdoutlog`, `stdouttrace`, and `stdoutmetric` exporters redirected to OTLP JSON files under `~/.milk/otel/`. No collector, no HTTP server.

**Why OTel over a custom logger.** Custom loggers create a local schema that diverges from the standard over time. OTel gives spec-compliant OTLP JSON from day one — the files are immediately consumable by any collector if the user later wants to ship to Grafana or OpenSearch. Switching from file export to OTLP/gRPC is a one-line exporter change; callers are untouched.

**Why file export over an embedded collector.** milk is a single-user local binary. Running an HTTP metrics server or gRPC collector endpoint for personal use adds complexity with no benefit. File export is the right primitive: observable with `tail -f`, archivable with `cp`, and collector-ready when needed.

**Why all three signals.** Logs alone cannot answer latency questions (traces) or show trends across sessions (metrics). Traces alone cannot explain *what happened* in a span without log context. Metrics alone cannot confirm that a specific Percept was promoted on a specific date. All three are needed to fully verify memory system behaviour.

**File management without sampling or rotation.** The Go OTel SDK has no built-in file rotation. Rather than implement rotation (which can corrupt in-flight writes), milk exposes explicit user controls: a `/otel` slash command shows file sizes and record counts, `/otel trim` archives current files by renaming them with a datestamp and starts fresh, and config thresholds (`warn_mb`, `max_mb`) warn or cap when files grow large. The self-observability metrics (`milk.otel.*_bytes`) make file growth visible in the metrics stream itself.

**`internal/obs` as the sole OTel boundary.** All provider bootstrapping, exporter wiring, and shutdown logic lives in `internal/obs`. Other packages (`internal/memory`, `internal/router`, `internal/agent/...`) call standard OTel APIs — they have no knowledge of how signals are exported. This keeps the exporter swap cost minimal.

## Consequences

All three OTel SDK module groups (`sdk`, `sdk/log`, `sdk/metric` and their stdout exporters) are added as direct dependencies. This adds ~15 transitive deps and approximately 3–5 MB to the binary. Acceptable for a CLI tool.

Memory system effects are now fully verifiable: every Percept write, recall, and consolidation produces a structured log record, a span, and a counter increment. A user can run `/otel` to confirm the system is active and inspect file sizes, and `/otel trim` to reset without losing history.

The absence of sampling means trace files grow proportionally to turn count. `warn_mb` (default 50 MB) provides early warning; `max_mb` (default disabled) provides a hard cap. Phase O3 may add head-based trace sampling if file growth becomes a practical concern.
