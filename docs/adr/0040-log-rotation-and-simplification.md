# ADR 0040 — Log Rotation and Simplification

**Status:** Proposed  
**Date:** 2026-08-29

---

## Context

milk writes seven distinct log/debug files with no rotation on the three largest:

| File | Typical size | Writer | Rotation |
|------|-------------|--------|----------|
| `~/.milk/claude_debug.ndjson` | 300–400 MB | Raw NDJSON from Claude CLI subprocess | ❌ None |
| `~/.milk/local_debug.log` | 300–400 MB | Raw SSE from local agent HTTP stream | ❌ None |
| `~/.milk/subprocess_debug.log` | 0 (small) | Raw stdout from subprocess agent | ❌ None |
| `~/.milk/otel/milk.log` | 1–1.5 GB/day | slog structured log | ⚠️ Manual `/otel trim` only |
| `~/.milk/otel/logs.jsonl` | Small | OTel log exporter | ⚠️ Manual trim |
| `~/.milk/otel/traces.jsonl` | Small | OTel trace exporter | ⚠️ Manual trim |
| `~/.milk/otel/metrics.jsonl` | Small | OTel metric exporter | ⚠️ Manual trim |

During extended sessions the debug logs and `milk.log` grow unbounded. Users have reported OOM crashes when tools (or the user) attempt to read a >1 GB log file. The three agent debug logs open file descriptors with `O_APPEND` and write raw streams via `fmt.Fprintln` — the `*os.File` is passed as an `io.Writer` with no size awareness. `CheckFileSizes()` only inspects the three OTel signal files and ignores the debug logs entirely.

The `milk.log` size is dominated by `LogPayload()`, which dumps entire request JSON at DEBUG level — including the full system prompt and tool schemas on every turn.

---

## Decision

### 1. RotatingWriter — a size-based `io.Writer` wrapper

Create `internal/obs/rotatingwriter.go`:

```go
type RotatingWriter struct {
    mu        sync.Mutex
    basePath  string
    maxBytes  int64
    maxFiles  int
    current   *os.File
    written   int64
}

func NewRotatingWriter(basePath string, maxBytes int64, maxFiles int) (*RotatingWriter, error)
func (w *RotatingWriter) Write(p []byte) (n int, err error)  // io.Writer
func (w *RotatingWriter) Close() error
```

- `maxBytes` — per-file cap (default 100 MB)
- `maxFiles` — number of rotated files to keep (default 5, so ~500 MB total per log type)
- Rotation: `foo.log` → `foo.1.log` → `foo.2.log` → ... → `foo.4.log` (oldest dropped)
- Thread-safe (multiple goroutines may write concurrently via agent streams)
- Drop-in for every current `WithDebugLog(w)` call site

### 2. Wire RotatingWriter into all six log files

Replace `*os.File` with `*RotatingWriter` in:

- `openCLIDebugLog()` → `claude_debug.ndjson`
- `openLocalDebugLog()` → `local_debug.log`
- `openSubprocessDebugLog()` → `subprocess_debug.log`
- `initMilkLogger()` → `milk.log`

The three OTel signal files (`logs.jsonl`, `traces.jsonl`, `metrics.jsonl`) are already small and manageable via `/otel trim`. No change needed for these unless the user wants consistency.

### 3. Update `CheckFileSizes()` to include debug logs

Current `CheckFileSizes()` in `internal/obs/otel.go` only checks `logs.jsonl`, `traces.jsonl`, `metrics.jsonl`. Extend it to also check `claude_debug.ndjson`, `local_debug.log`, `subprocess_debug.log`, and `milk.log`. Emit a warning when any exceeds `warn_mb`.

### 4. Update `Trim()` to include debug logs

The `/otel trim` handler currently archives and resets only the three OTel signal files. Extend it to also truncate (or archive) the three agent debug logs and `milk.log`.

### 5. Configuration

Add to `OtelConfig` in `internal/config/config.go`:

```go
type OtelConfig struct {
    // ... existing fields ...

    DebugLogMaxBytes int64 `json:"debug_log_max_bytes"` // per-file cap, default 104857600 (100 MB)
    DebugLogMaxFiles int   `json:"debug_log_max_files"` // rotated files to keep, default 5
}
```

These control the `RotatingWriter` for all debug/structured logs. Defaults are safe for disk-constrained machines (total ~500 MB per log type, ~2 GB across all four rotated files).

### 6. Payload logging — rotation over truncation

`milk.log`'s size is dominated by `LogPayload()`, which dumps full request JSON at DEBUG level. Rather than truncating payloads (which loses diagnostic value), the `RotatingWriter` from §1 handles size via rotation. Once `milk.log` hits `debug_log_max_bytes`, it rolls over. Full payloads remain intact in all rotated files for post-hoc analysis. This is simpler to implement and more useful for debugging than truncation.

---

## Consequences

**Positive:**
- Eliminates the OOM risk from reading unbounded log files.
- `/otel trim` becomes a single command that covers all log files, not just OTel signals.
- `CheckFileSizes()` gives an accurate warning before any log file becomes a problem.
- `RotatingWriter` is a reusable `io.Writer` — any future log file gets rotation for free by wrapping it.
- Full payloads are preserved across all rotated files, retaining diagnostic value for post-hoc analysis.

**Negative / risks:**
- Rotation may lose recent log lines if the user is mid-session and a rotation fires. Mitigation: 100 MB per file is generous; rotation only fires between `Write` calls, never mid-line.
- The `RotatingWriter` adds a mutex to every log write. Impact is negligible (log writes are already I/O-bound), but the lock is held only for the duration of the underlying `Write()` + byte-count check.
- Old behavior: `milk.log` grows until manual `/otel trim`. New behavior: automatic rotation at 100 MB. Users who relied on the full unrotated log for post-hoc analysis will now get at most 500 MB of history per log type. This is configurable via `debug_log_max_bytes` and `debug_log_max_files`.

---

## Implementation Plan

### Sprint 1 — RotatingWriter + wire into all log files

**Scope:** Create `internal/obs/rotatingwriter.go` with `NewRotatingWriter`, `Write`, `Close`, and rotation logic. Replace `*os.File` with `*RotatingWriter` in `openCLIDebugLog()`, `openLocalDebugLog()`, `openSubprocessDebugLog()`, and `initMilkLogger()`. Update `Close()` calls in session lifecycle to close the rotating writer instead of raw file handles.

**Done when:** `go build ./...` succeeds; `go test ./internal/obs/...` passes; starting milk and running a session produces rotated log files when the 100 MB threshold is crossed (testable by temporarily setting `maxBytes` to a small value like 1 KB).

**Testing strategy:** Unit tests in `internal/obs/rotatingwriter_test.go`:
- Write less than `maxBytes` → single file, no rotation.
- Write more than `maxBytes` → file rotates (`foo.1.log` exists, main file is fresh).
- Write more than `maxBytes * maxFiles` → oldest rotated file is deleted.
- Concurrent writes from multiple goroutines → no data corruption (test with `t.Parallel` and a race-enabled run).
- `Close()` flushes any buffered data.

Integration test: set `DebugLogMaxBytes` to 1024 in a test config, start a mock session that writes >5 KB of log lines, verify that multiple rotated files exist and the main file is ≤ 1024 bytes.

---

### Sprint 2 — Extend CheckFileSizes + Trim

**Scope:** Update `CheckFileSizes()` in `internal/obs/otel.go` to include the three agent debug log paths and `milk.log` in its size scan. Update `Trim()` to truncate (or archive) those files alongside the OTel signal files.

**Done when:** `/otel trim` reports and resets all seven log files; `CheckFileSizes()` emits a warning when any debug log exceeds `warn_mb`.

**Testing strategy:** Unit test `CheckFileSizes()` with a temp directory containing oversized dummy files for each log type; verify all seven are reported. Unit test `Trim()` with the same setup; verify all files are truncated to zero (or archived).

---

### Sprint 3 — Config wiring + milk.log payload policy

**Scope:** Add `DebugLogMaxBytes` and `DebugLogMaxFiles` to `OtelConfig` in `internal/config/config.go`. Thread the config values through to `NewRotatingWriter` calls in `cmd/milk/start.go` and `cmd/milk/run.go`.

`milk.log` already inherits rotation from Sprint 1 — no truncation is applied to payload entries. Rotation handles the size: once `milk.log` hits `debug_log_max_bytes`, it rolls over. This preserves full diagnostic fidelity in all log files. The `RotatingWriter` cap (~500 MB total per log type with defaults) is sufficient to prevent OOM while retaining complete payloads for post-hoc analysis.

**Done when:** `go build ./...` and `go test ./...` pass; a session with `debug_local: true` + `log_context: true` produces a `milk.log` that rotates cleanly at the configured size; the rotated file sizes respect the configured `debug_log_max_bytes` and `debug_log_max_files`.

**Testing strategy:** Config integration test: set `debug_log_max_bytes` to a custom value in JSON, load config, verify `OtelConfig.DebugLogMaxBytes` returns the custom value. Integration test: with small `debug_log_max_bytes`, confirm `milk.log` rotates and old payload entries are preserved in `.1.log` files, not lost.

---

## Commit sequence

```
feat(obs): add RotatingWriter with size-based rotation          ← Sprint 1
feat(obs): wire RotatingWriter into all agent and milk logs     ← Sprint 1
feat(obs): extend CheckFileSizes and Trim to cover debug logs   ← Sprint 2
feat(config): add DebugLogMaxBytes and DebugLogMaxFiles to OtelConfig  ← Sprint 3
```
