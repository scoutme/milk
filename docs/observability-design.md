# Observability Design for milk

## Alignment with TECO guidelines

The TECO architecture guidelines mandate OpenTelemetry for traces and metrics, structured logs with at least three levels (DEBUG/INFO/ERROR), and log content security policies. milk is a local single-binary CLI, not a microservice, so the infrastructure layer (OTel collector, Prometheus, Grafana, OpenSearch) is not applicable. However the signal taxonomy and standards are adopted in full:

| TECO pillar | milk implementation |
|---|---|
| OTel traces | Go OTel SDK `sdk/trace`, `stdouttrace` exporter → file |
| OTel metrics | Go OTel SDK `sdk/metric`, `stdoutmetric` exporter → file |
| Structured logs (DEBUG/INFO/ERROR) | Go OTel SDK `sdk/log`, `stdoutlog` exporter → file |
| OTel collector | Not deployed locally — file export is collector-ready (OTLP JSON) |
| PII log policy | Local-only files; user controls content. Percept content is written verbatim (TECO "local immutable datastore" exception) |

All three signals use standard OTLP JSON. Switching from file export to an OTLP/gRPC or HTTP collector is a one-line exporter swap — callers are untouched.

---

## File layout

```
~/.milk/otel/
  traces.jsonl      # OTLP JSON spans, one per line
  metrics.jsonl     # OTLP JSON metric exports, one per line
  logs.jsonl        # OTLP NDJSON log records
  milk.log          # human-readable or JSON slog log (otel.log_format=text|json; disabled by default)
```

---

## Signal coverage

### Logs — leveled, structured, per-event

Default level: INFO. Configurable to DEBUG (verbose Percept content, routing scores) or ERROR (errors only).

| Component | Event | Level | Key attributes |
|---|---|---|---|
| `memory` | `percept_recorded` | DEBUG | `percept_id`, `producer`, `w`, `core`, `session_id` |
| `memory` | `percept_recalled` | DEBUG | `query`, `results_n`, `min_confidence`, `session_id` |
| `memory` | `consolidation_run` | INFO | `session_id`, `percepts_before`, `decayed`, `pruned`, `promoted`, `global_total` |
| `memory` | `store_opened` | DEBUG | `session_id`, `global_path`, `session_path` |
| `router` | `route_decision` | INFO | `target`, `method` (`rules`/`classifier`/`forced`), `score`, `elapsed_ms` |
| `agent/local` | `turn_start` | DEBUG | `session_id`, `turn_n`, `input_len` |
| `agent/local` | `turn_end` | INFO | `session_id`, `turn_n`, `elapsed_ms`, `tool_calls_n` |
| `agent/local` | `tool_dispatched` | DEBUG | `tool_name`, `elapsed_ms` |
| `agent/local` | `escalation_signal` | INFO | `reason` |
| `agent/claude` | `turn_start` | DEBUG | `session_id`, `turn_n` |
| `agent/claude` | `turn_end` | INFO | `session_id`, `turn_n`, `elapsed_ms`, `permission_requests_n` |
| `session` | `session_created` | INFO | `session_id`, `cwd` |
| `session` | `session_resumed` | INFO | `session_id`, `cwd` |
| `session` | `session_dropped` | INFO | `session_id` |

### Traces — per-turn span tree

Root span: `milk.turn`
- Attributes: `session_id`, `turn_n`, `input_len`, `agent`
- Children:
  - `milk.route` — routing decision (target, method, score)
  - `milk.agent.local` or `milk.agent.claude` — agent execution
    - `milk.tool.<name>` — one child span per tool call
    - `milk.memory.recall` — when `get_memory` fires (query, results_n)
    - `milk.memory.record` — when `record_memory` fires (content_len)
  - `milk.memory.consolidation` — at session end (decayed, pruned, promoted)

### Metrics

All metrics use the `milk.` namespace per OTel semantic conventions.

| Metric | Type | Labels | Description |
|---|---|---|---|
**Turn lifecycle**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.turns.total` | Counter | `target=local\|escalation`, `source=user\|auto_sticky\|auto` | Completed turns |
| `milk.turns.latency_ms` | Histogram | `target=` | End-to-end turn latency |
| `milk.turns.errors` | Counter | `target=`, `kind=inference` | Turn-level errors |
| `milk.session.duration_ms` | Gauge | — | Total session wall-clock time (recorded at shutdown) |
| `milk.session.state_transitions` | Counter | `from=`, `to=` | Session state machine transitions |

**Router**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.router.decisions` | Counter | `target=local\|escalation`, `rule=explicit\|state\|hard_threshold\|soft_score\|classifier\|default` | Routing decisions |
| `milk.router.classify_latency_ms` | Histogram | `model=` | LLM classifier call latency (only when classifier runs) |
| `milk.router.score` | Histogram | — | Raw soft-signal score (only when no hard rule fires) |
| `milk.router.escalation_signals` | Counter | `reason=repeated_prompt\|explicit_tool_call` | Self-escalation from the local tool loop |

**Inference**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.inference.latency_ms` | Histogram | `model=`, `agent=primary\|escalation\|router`, `provider=local\|bedrock` | Time from HTTP request to stream complete |
| `milk.inference.errors` | Counter | `model=`, `agent=`, `kind=http` | Inference-level errors |

**Tools (local agent)**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.tools.calls` | Counter | `name=`, `agent=primary\|escalation` | Tool invocations |
| `milk.tools.latency_ms` | Histogram | `name=` | Tool execution latency |
| `milk.tools.permission_grants` | Counter | `name=`, `source=store\|interactive\|skip_perms` | Approved permission requests |
| `milk.tools.permission_denials` | Counter | `name=` | Denied permission requests |

**Claude CLI agent**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.claude.turns` | Counter | `mode=first\|resume` | Claude subprocess invocations |
| `milk.claude.latency_ms` | Histogram | `mode=` | Subprocess wall-clock time |
| `milk.claude.tool_uses` | Counter | `name=` | Tool calls inside Claude (from `OnToolUse` callback) |
| `milk.claude.permission_denials` | Counter | — | Permission denials from `ParseResult.PermissionDenials` |
| `milk.claude.errors` | Counter | `kind=subprocess\|is_error` | Claude subprocess errors |

**Tokens**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.tokens.prompt` | Counter | `model`, `agent` | Prompt tokens consumed |
| `milk.tokens.completion` | Counter | `model`, `agent` | Completion tokens generated |
| `milk.tokens.total` | Counter | `model`, `agent` | Total tokens (prompt + completion) |

**Memory**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.memory.percepts_recorded` | Counter | `producer`, `scope` (`session`/`global`) | Percepts written |
| `milk.memory.percepts_recalled` | Counter | — | `get_memory` invocations |
| `milk.memory.consolidation.decayed` | Counter | — | Percepts that lost weight |
| `milk.memory.consolidation.pruned` | Counter | — | Percepts removed (W≤0) |
| `milk.memory.consolidation.promoted` | Counter | — | Percepts promoted to global |
| `milk.memory.global_size` | Gauge | — | Percept count in global store after consolidation |

**Self-observability**

| Metric | Type | Labels | Description |
|---|---|---|---|
| `milk.otel.logs_bytes` | Gauge | — | Current size of logs.jsonl |
| `milk.otel.traces_bytes` | Gauge | — | Current size of traces.jsonl |
| `milk.otel.metrics_bytes` | Gauge | — | Current size of metrics.jsonl |

The last three (`milk.otel.*_bytes`) are self-observability metrics: they make the growth of the observability files themselves visible in the metrics stream.

#### Token metric labels

- `model`: the model name from config (e.g. `"qwen2.5-coder"`, `"claude-3-5-sonnet-20241022"`)
- `agent`: role of the agent that made the call — `"primary"`, `"escalation"`, or `"router"` (classification calls)

#### Token metric availability by backend

| Backend | Source | Availability |
|---|---|---|
| OpenAI-compatible (streaming) | trailing SSE chunk `usage` field | Server-dependent — most local servers (llama.cpp, Ollama) include it; some may not |
| OpenAI-compatible (classify, non-streaming) | `usage` field in JSON response | Always present in spec-compliant servers |
| Bedrock Converse (streaming) | `metadata` event in AWS Event Stream | Always present |
| Bedrock Converse (classify, non-streaming) | `usage` field in JSON response | Always present |
| Claude CLI subprocess | `result` event `usage` field | Always present — parsed from the final `result` NDJSON line |

---

## Observability file management

Without sampling or rotation, observability files grow unboundedly. milk provides built-in visibility and tooling so the user can manage this without external tools.

### `/otel` slash command

A new slash command available in interactive mode:

```
/otel            show observability file sizes and record counts
/otel trim       archive current files (rename to *.YYYY-MM-DD.jsonl) and start fresh
/otel off        disable OTel for this session (no new records written)
/otel on         re-enable OTel
```

`/otel` output example:
```
[milk] observability files (~/.milk/otel/)
  logs.jsonl      1.2 MB   4,310 records   oldest: 2026-04-01  newest: 2026-05-15
  traces.jsonl    3.8 MB  12,041 records   oldest: 2026-04-01  newest: 2026-05-15
  metrics.jsonl   0.4 MB   1,203 records   oldest: 2026-04-01  newest: 2026-05-15
  total           5.4 MB
hint: use /otel trim to archive and reset, or set otel.max_mb in ~/.milk/config.json
```

### Automatic size warnings

When any single file exceeds `otel.warn_mb` (default: 50 MB), milk prints a one-time warning at session start:

```
[milk] warning: ~/.milk/otel/traces.jsonl is 52 MB — run /otel trim to archive
```

The warning is one-per-session (not every turn) to avoid noise.

### Config controls

```json
{
  "otel": {
    "enabled": true,
    "log_level": "INFO",
    "traces": true,
    "metrics": true,
    "warn_mb": 50,
    "max_mb": 0
  }
}
```

- `warn_mb`: warn when any file exceeds this size. 0 = no warning.
- `max_mb`: hard cap — when any file exceeds this, OTel is silently disabled for the session and a warning is printed. 0 = no cap. Recommended starting value: 200 MB.

### `/otel trim` behaviour

Renames each file to `<name>.<YYYY-MM-DD>.jsonl` in the same directory, then creates a fresh empty file. No data is deleted. The user can delete archived files manually or via `rm ~/.milk/otel/*.2026-*.jsonl`.

---

## Package structure

```
internal/obs/
  provider.go     # OTel SDK bootstrap: TracerProvider, MeterProvider, LoggerProvider
  obs.go          # package-level accessors: StartSpan, EndSpan, Inc, Add, SetGauge, RecordDuration
  helpers.go      # withUnit, withAttrs, ContextWithSessionID, SessionIDFromContext
  filesize.go     # file size + record count helpers for /otel command
  metrics.go      # FormatMetrics: parse metrics.jsonl → human-readable table
  search.go       # SearchSignals, FormatSearchResults: grep over signal files
  tools.go        # ToolSchemas + dispatch for get_metrics and search_signals agent tools
```

All other packages (`internal/memory`, `internal/router`, `internal/agent/...`) import only `internal/obs`. No custom logger or metrics types — callers use standard OTel APIs.

Config extension is added to `internal/config/config.go`.

---

## Agent tools

Two OTel tools are exposed to the local agent via `obs.ToolSchemas()`:

**`get_metrics`** — returns the most recent value of every metric in `metrics.jsonl`:
```json
{}
```

**`search_signals`** — grep over logs/traces/metrics JSONL for a pattern:
```json
{
  "pattern": "consolidation",
  "signals": ["logs", "metrics"],
  "max_results": 10
}
```

`signals` defaults to all three files when omitted. Results include file name, line number, and the matching line (truncated to 200 chars). Useful for debugging a specific event, correlating a trace ID across files, or inspecting raw OTel output.

---

## Implementation phases

### Phase O1 — Foundation + memory instrumentation ✓ complete

- `internal/obs`: provider bootstrap (all three signals), file exporters, shutdown
- Config `otel` block with `enabled`, `traces`, `metrics`, `warn_mb`, `max_mb`, `metrics_flush_minutes`
- Instrument `internal/memory`: all Percept events as logs + metrics + trace spans
- Provider init and `Shutdown()` wired in `cmd/milk` (both single-prompt and REPL)
- `filesize.go`: size + record count helpers; `metrics.go`: metric value parser
- `search.go`: `SearchSignals` grep over signal files
- `/otel` slash command (stats, trim, on/off); `/metrics` slash command
- `get_metrics` + `search_signals` agent tools
- Size warning on session start; `max_mb` hard cap enforcement

### Phase O2 — Turn and agent coverage ✓ complete

- `milk.turns.total{target,source}`, `milk.turns.latency_ms{target}`, `milk.turns.errors{target,kind}` — wired in both single-prompt (`main.go`) and REPL (`repl.go`) dispatch paths
- `milk.session.duration_ms` gauge at shutdown (both paths)
- `milk.session.state_transitions{from,to}` counter in `logStateTransition`
- `milk.router.decisions{target,rule}`, `milk.router.classify_latency_ms{model}`, `milk.router.score` — wired in `router.go` / `rules.go`
- `milk.router.escalation_signals{reason}` — wired at both self-escalation sites in `local.go`
- `milk.inference.latency_ms{model,agent,provider}`, `milk.inference.errors{model,agent,kind}` — wired in `streamCompletion` (OpenAI path) and `bedrockStreamCompletion` (Bedrock path)
- `milk.tools.calls{name,agent}`, `milk.tools.latency_ms{name}` — wired in `executeToolCalls`
- `milk.tools.permission_grants{name,source}`, `milk.tools.permission_denials{name}` — wired in `checkPermission`
- `milk.claude.turns{mode}`, `milk.claude.latency_ms{mode}`, `milk.claude.tool_uses{name}`, `milk.claude.permission_denials`, `milk.claude.errors{kind}` — wired in `claude.go` `run()`

---

## Resolved decisions

1. **Exporter for logs**: `stdoutlog` redirected to file (human-readable NDJSON). The output is readable with `cat`/`jq` without an OTel viewer, which matters for a local tool. Strict OTLP protobuf is unnecessary while there is no collector.
2. **Metrics export trigger**: export at session end AND on a periodic ticker (default every 5 minutes, configurable via `otel.metrics_flush_minutes`). The periodic flush ensures metrics are not lost when the process is killed or crashes mid-session. The ticker is started in a background goroutine after provider init and stopped on graceful shutdown.
3. **Trace sampling**: all spans — no sampling for Phase O1. At typical CLI usage (tens of turns per day) the trace file grows ~1–2 KB/turn; `warn_mb: 50` provides ample early warning.

Config addition for periodic metrics flush:
```json
{
  "otel": {
    "enabled": true,
    "log_level": "INFO",
    "traces": true,
    "metrics": true,
    "warn_mb": 50,
    "max_mb": 0,
    "metrics_flush_minutes": 5
  }
}
```

`metrics_flush_minutes: 0` disables the ticker (session-end export only).
