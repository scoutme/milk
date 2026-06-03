# Context bloat analysis and reduction plan

Date: 2026-06-01

## Method

Analysis based on live payload logging (`otel.log_context: true`, `otel.log_format: text`) and
`~/.milk/claude_debug.ndjson` from 360 real escalation sessions. All char counts are from actual
payloads captured during development sessions on the milk codebase.

---

## Primary agent (OpenAI-compat / llama.cpp)

### What is sent on every turn

A representative fresh-session payload measured 15,180 chars total:

| Component | Chars | % of total | Changes between turns? |
|---|---|---|---|
| System prompt (rules block) | 4,317 | 28% | Never within session |
| Tool names in system prompt intro | 175 | 1% | Never (also redundant) |
| Working directory listing | 578 | 4% | Rarely |
| **Tools JSON array** | **9,500** | **63%** | Almost never |
| Conversation history + user message | 610 | 4% | Every turn (the actual signal) |

**94% of each request is static overhead.** Conversation content is less than one twentieth of the payload.

### Tools array breakdown (9,500 chars, 63% of payload)

18 schemas, sent in full on every request, every turn:

| Tool | Chars |
|---|---|
| `record_memory` | 1,138 |
| `get_session_context` | 858 |
| `list_memory` | 800 |
| `search_signals` | 686 |
| `get_memory` | 587 |
| `export_session` | 574 |
| `edit_file` | 553 |
| `grep` | 526 |
| `find_files` | 478 |
| `read_file` | 460 |
| `current_need` | 421 |
| `write_file` | 419 |
| `http_get` | 393 |
| `forget_memory` | 380 |
| `escalate` | 324 |
| `get_metrics` | 324 |
| `list_dir` | 296 |
| `bash` | 283 |

The descriptions are written for Claude-level reasoning. A 7B local model doesn't need
3-sentence parameter descriptions with usage guidance — it needs the parameter name, type, and
a one-line description. `record_memory` alone at 1,138 chars has a 5-field schema with long
enum descriptions; it could be reduced to under 400 chars with no capability loss.

### History management

Context budget: 24,000 chars (default). Trimming is FIFO — oldest user+assistant pairs dropped
first. Works correctly; no structural problem here. The budget itself is the right lever if
models with smaller context windows are added.

---

## Escalation agent (Claude CLI)

### Token data from 360 sessions

| Metric | Value |
|---|---|
| Total sessions | 360 |
| Total cost | $216.54 |
| Average cost per session | $0.60 |
| Cache hit rate | 88.8% |
| Avg cache_write per session | 83,541 tokens (~334k chars) |
| Avg cache_read per session | 662,244 tokens (~2.6M chars) |

### What drives cache_write growth

Cache_write grows from ~50k tokens at the start of a session to 267k tokens by session end.
This is Claude Code reading project source files — not milk overhead. Milk's
`--append-system-prompt-file` contribution is at most ~13,500 chars (~3,375 tokens), under 4%
of the first-session write.

### What milk injects via `--append-system-prompt-file`

Context is split across two files sent as separate `--append-system-prompt-file` flags,
so the stable prefix can be cached independently of the changing dynamic suffix.

**File 1 — static (cacheable across turns):**

| Component | Chars | Dynamic? |
|---|---|---|
| `NeedInstruction` | 327 | Stable (per-session nonce) |
| `MemoryInstruction` | 299 | Stable (per-session nonce + agent names) |
| Percepts | 0–2,500 | Up to 25 × ~100 chars |

**File 2 — dynamic (changes per turn):**

| Component | Chars | Dynamic? |
|---|---|---|
| `identityBlock` | 207 | Static text |
| `EscalationBrief` | 0–500 | Set on `escalate()` calls |
| `CurrentNeed` | 0–200 | Updated by model tags |
| `LastLocalSummary` | 0–12,000 | Updated after each primary turn |

The nonce (`EscalationNonce`) is generated once at the first escalation of a session and
persisted in the session file. Reusing it keeps file 1 byte-identical across turns, preserving
Claude's prompt-cache prefix.

### The cache invalidation problem (resolved)

Previously, all components were bundled into one file and the nonce was regenerated on every
turn, guaranteeing a cache miss on every instruction re-injection. The split-file approach
(implemented 2026-06-03) means:

- File 1 (large, ~3k+ chars) hits cache on every turn except the first and reinjection thresholds.
- File 2 (small, ~200–12k chars) changes on primary→escalation transitions but no longer
  invalidates file 1's cache prefix.

The `MemoryReinjectionTurns` threshold (default: 20) still governs when file 1 is re-sent
(e.g. after Claude context compaction).

### What is NOT a problem

- The 88.8% hit rate is good. The system is working as intended at the macro level.
- The growing cache_write is expected and unavoidable — Claude needs to hold the codebase in context.
- The base 15,031-token cache read seen on single-turn escalations is Claude's own CLAUDE.md +
  permissions system prompt, unrelated to milk.

---

## Action plan

### A1 — Remove tool names from system prompt intro
**Impact:** ~175 chars/turn | **Effort:** Trivial | **Risk:** None

The first line of `systemPromptPrimary` lists all tool names:
*"You are an assistant with access to tools: bash, find_files, grep…"*
The model already receives the full schemas via the `tools` array. Remove this sentence.

Files: `internal/agent/local/local.go` (`systemPromptPrimary`, `systemPromptEscalationFmt`)

---

### A2 — Cache working directory listing within session
**Impact:** ~578 chars × (turns − 1) per session | **Effort:** Low | **Risk:** Low

The cwd listing is injected as a fresh `system` message on every turn (`cwdContext()` in
`Run()`). It only changes if files are created or deleted. Cache it on the agent after first
injection; invalidate only when the model calls `list_dir` on the working directory (explicit
stale signal) or on `/new`.

Implementation: add `cachedCwdContext string` and `cachedCwd string` fields to `Agent`; skip
`cwdContext()` injection when `cachedCwd == sess.CWD` and cache is populated.

Files: `internal/agent/local/local.go`

---

### A3 — Trim tool schema descriptions for local models
**Impact:** ~2,500–3,500 chars/turn | **Effort:** Medium | **Risk:** Low

Tool schemas written for Claude describe each parameter with a sentence or two. A local 7B
model only needs the parameter name, type, and a ≤10-word description. Priority targets:

| Tool | Current | Target | Saving |
|---|---|---|---|
| `record_memory` | 1,138 | ~380 | ~758 |
| `get_session_context` | 858 | ~350 | ~508 |
| `list_memory` | 800 | ~300 | ~500 |
| `search_signals` | 686 | ~280 | ~406 |
| `get_memory` | 587 | ~250 | ~337 |

Approach: keep current verbose schemas as the authoritative version; add a `compact` variant
used when the agent is a local model (not `claude-cli`). Flag on `AgentConfig` or detect via
provider type.

Files: `internal/agent/local/tools.go`

---

### A4 — Split static/dynamic context into separate files ✓ Done (2026-06-03)
**Impact:** Eliminates cache invalidation on primary→escalation transitions | **Effort:** Medium

Implemented via `BuildStaticContext` / `BuildDynamicContext` in `internal/escalation/builder.go`.
`RunFirst` and `RunResume` in `internal/agent/claude/claude.go` accept both and pass them as
separate `--append-system-prompt-file` flags. `EscalationNonce` is now persisted in the session
file and reused across turns. The dynamic-content hash guard (previously on the full combined
file) now guards only the dynamic file.

---

### A5 — Track `LastLocalSummary` change separately ✓ Done (implicitly, via split)
**Impact:** Reduces unnecessary cache invalidations

`LastLocalSummaryInjected` deduplication in `BuildDynamicContext` suppresses re-sends of an
unchanged summary. Combined with the split-file approach, a no-op primary turn no longer
produces any file update at all.

---

## Priority and sequencing

| Action | Chars saved/turn | Cache benefit | Effort | Status |
|---|---|---|---|---|
| A1 — Remove tool list from prompt | 175 | None | 5 min | Open |
| A2 — Cache cwd listing | 578 × (n−1) | None | 1h | Open |
| A3 — Compact tool schemas | 2,500–3,500 | None | 3–4h | Open |
| A4 — Split static/dynamic context | — | High ($0.10–0.30/session) | 4h | **Done** |
| A5 — Track summary hash | — | Medium | 1h | **Done** (via A4 split) |

A1 and A2 are pure local-agent improvements with no risk. A3 requires validating that compact
schemas don't degrade local model tool-call accuracy — run a few tool-heavy sessions before
committing.

**Combined effect of A1+A2+A3:** reduce per-turn payload from ~15k to ~11k chars (~27%
reduction). For a 10-turn session: ~40k chars saved from context window pressure, letting
history accumulate longer before the 24k budget forces trimming.
