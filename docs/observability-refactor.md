# milk observability — refactor spec

## Current state (what's actually instrumented)

The assessment confirms the observation: OTel coverage today is almost entirely memory-layer.

| Layer | Metrics | Spans | Debug logs |
|---|---|---|---|
| Memory | percepts_recorded/recalled, consolidation decayed/pruned/promoted, global_size | record, recall, consolidation | — |
| Token counting | milk.tokens.prompt/completion/total (model, agent labels) | — | per-call debug entry |
| Router | — | — | one entry per decision (target, reason) |
| Local agent — inference | — | — | full payload if log_context=true |
| Local agent — tool loop | — | — | one entry per tool call (name, args) |
| Claude CLI agent | — | — | full payload if log_context=true |
| Session state | — | — | one entry per state transition |
| Context trimming | — | — | one entry per trim (msgs before/after) |
| Errors | — | — | — |

Everything not in that table is dark: tool latency, inference latency, routing outcomes, permission decisions, turn latency, error rates.

---

## Goals and constraints

- **Lean**: add only signals with clear operational value — no vanity counters
- **No new dependencies**: the existing OTel SDK, file exporters, and `obs` package are sufficient
- **Additive only**: no existing call site changes required; add calls alongside existing code
- **Queryable**: every metric must be answerable by the local agent via `get_metrics` / `search_signals`
- **Debug-log parity**: wherever a metric is added, the debug log entry should carry the same fields (already the pattern in `RecordTokens`)

---

## New signals by layer

### 1 — Turn lifecycle (`cmd/milk/main.go`, `repl.go`)

**What's missing**: total turn count is only in-memory (`IncrementTurnCount`); it never reaches `metrics.jsonl`. Turn latency is not recorded at all.

| Signal | Type | Labels | Instrument location |
|---|---|---|---|
| `milk.turns.total` | counter | `target=local\|escalation\|escalation_local`, `source=user\|sticky\|auto_sticky` | `main.go` after each completed turn |
| `milk.turns.latency_ms` | histogram | `target=` (same) | wrap the `runLocal` / `runEscalation` call with `time.Now()` |
| `milk.turns.errors` | counter | `target=`, `kind=inference\|tool\|permission\|timeout` | on non-nil turn error |

**Session duration**: record a single `milk.session.duration_ms` gauge at shutdown (already have a shutdown hook in `main.go`).

---

### 2 — Router (`internal/router/router.go`)

**What's missing**: the one `obs.Debug` call tells you the outcome but there are no metrics, so you can't answer "what fraction of turns escalate?" or "which rule is firing?".

| Signal | Type | Labels |
|---|---|---|
| `milk.router.decisions` | counter | `target=local\|escalation`, `rule=explicit\|state\|hard_threshold\|soft_score\|classifier\|default` |
| `milk.router.classify_latency_ms` | histogram | `model=` | only when LLM classifier runs |
| `milk.router.score` | histogram | — | record the raw soft-signal score on every non-conclusive pass (omit when hard rule fires first) |

**Instrument in**: `Route()` function, at the point where `Decision` is returned. One `obs.Inc` + one `obs.RecordDuration` call.

---

### 3 — Inference latency (`internal/agent/local/local.go`, `bedrock.go`)

**What's missing**: tokens are counted but no histogram exists for how long calls take. No time-to-first-token. This is the single most useful missing metric for diagnosing slow turns.

| Signal | Type | Labels |
|---|---|---|
| `milk.inference.latency_ms` | histogram | `model=`, `agent=primary\|escalation\|router`, `provider=local\|bedrock` |
| `milk.inference.errors` | counter | `model=`, `agent=`, `kind=http\|parse\|timeout` |

**Instrument in**: `streamCompletion` (OpenAI path) and `bedrockConverse` — wrap the HTTP call with `time.Now()`, record after `httpResp` arrives or on error. The Bedrock path already has `RecordTokens` at the right place; latency goes right after.

No time-to-first-token in this iteration — requires threading a channel through the scanner; defer to a follow-on.

---

### 4 — Tool execution (`internal/agent/local/local.go`)

**What's missing**: only a debug log entry per tool call. No counter, no latency, no permission tracking.

| Signal | Type | Labels |
|---|---|---|
| `milk.tools.calls` | counter | `name=bash\|read_file\|write_file\|edit_file\|http_get\|…`, `agent=primary\|escalation_local` |
| `milk.tools.latency_ms` | histogram | `name=` |
| `milk.tools.permission_grants` | counter | `name=`, `source=store\|interactive\|skip_perms` |
| `milk.tools.permission_denials` | counter | `name=` |

**Instrument in**: `executeToolCalls` — wrap each `dispatchTool` call. Permission counters go in `checkPermission`.

---

### 5 — Claude CLI agent (`internal/agent/claude/claude.go`)

**What's missing**: the subprocess path has no spans or metrics. `LogPayload` is there but that requires `log_context=true`.

| Signal | Type | Labels |
|---|---|---|
| `milk.claude.turns` | counter | `mode=first\|resume` |
| `milk.claude.latency_ms` | histogram | `mode=` |
| `milk.claude.tool_uses` | counter | `name=` | from existing `OnToolUse` callback |
| `milk.claude.permission_denials` | counter | — | from `ParseResult.PermissionDenials` (already populated) |
| `milk.claude.errors` | counter | `kind=subprocess\|parse\|is_error` |

**Instrument in**: `run()` — wrap subprocess execution with `time.Now()`, record after `Stream()` returns. `permission_denials` counter is just `len(res.PermissionDenials)`. Tool counter hooks into `OnToolUse` in `StreamOpts`, already wired in the repl path.

---

### 6 — Session state transitions (`cmd/milk/main.go`)

**What's missing**: `obs.Debug("state transition", ...)` exists but no metric. Can't query "how often does a session end in ESCALATION_WAITING?".

| Signal | Type | Labels |
|---|---|---|
| `milk.session.state_transitions` | counter | `from=`, `to=` |

**Instrument in**: `logStateTransition` — add one `obs.Inc` call alongside the existing `obs.Debug`. Zero new logic.

---

### 7 — Escalation signals (`internal/agent/local/local.go`)

**What's missing**: self-escalation from the local tool loop is invisible.

| Signal | Type | Labels |
|---|---|---|
| `milk.router.escalation_signals` | counter | `reason=repeated_prompt\|explicit_tool_call` |

**Instrument in**: the two places that return `&EscalationSignal{}` in `local.go`.

---

## Implementation order

These are independent; any can be done in isolation.

| Priority | Layer | Effort | Value |
|---|---|---|---|
| 1 | Turn lifecycle + latency | Small — two `time.Now()` wraps + 3 `obs.*` calls | Directly answers "how long do turns take?" |
| 2 | Inference latency | Small — one wrap per provider path | Separates "slow model" from "slow tool loop" |
| 3 | Router decisions metric | Tiny — one `obs.Inc` in `Route()` | Answers "how often does escalation fire and why?" |
| 4 | Tool call counters | Small — two `obs.*` calls in `executeToolCalls` | Shows what the local agent actually does |
| 5 | Claude CLI metrics | Small — three `obs.*` calls in `run()` | Brings CLI path to parity with local path |
| 6 | Permission counters | Tiny — two `obs.Inc` calls in `checkPermission` | Useful when debugging interactive permission prompts |
| 7 | State transition counter | Trivial — one `obs.Inc` in existing `logStateTransition` | Low effort, closes the last gap |
| 8 | Escalation signals | Trivial | Niche but useful |

---

## What does NOT need to change

- `obs` package internals — all primitives (`Inc`, `Add`, `RecordDuration`, `StartSpan`) already exist
- `FormatMetrics` / `search_signals` — new metrics appear automatically in their output
- Memory instrumentation — already thorough; no changes
- Token accumulation — already thorough; no changes
- `LogPayload` / `log_context` mode — already covers full request payloads; no changes

---

## New metrics inventory (complete list)

```
milk.turns.total              {target, source}
milk.turns.latency_ms         {target}
milk.turns.errors             {target, kind}
milk.session.duration_ms      {}
milk.session.state_transitions {from, to}
milk.router.decisions         {target, rule}
milk.router.classify_latency_ms {model}
milk.router.score             {}
milk.router.escalation_signals {reason}
milk.inference.latency_ms     {model, agent, provider}
milk.inference.errors         {model, agent, kind}
milk.tools.calls              {name, agent}
milk.tools.latency_ms         {name}
milk.tools.permission_grants  {name, source}
milk.tools.permission_denials {name}
milk.claude.turns             {mode}
milk.claude.latency_ms        {mode}
milk.claude.tool_uses         {name}
milk.claude.permission_denials {}
milk.claude.errors            {kind}
```

All existing metrics (`milk.tokens.*`, `milk.memory.*`) are unchanged.

---

## Validation checklist (before implementing)

- [ ] Run a session with `/otel` and confirm only `milk.memory.*` and `milk.tokens.*` appear in metrics output
- [ ] Confirm `milk.turns.total` and `milk.inference.latency_ms` are absent
- [ ] Confirm router decisions only appear in the debug log, not in metrics
- [ ] Confirm tool call counts are not queryable via `search_signals type=metrics`
