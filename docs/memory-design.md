# Memory System Design for milk

## What the RFC describes vs. what milk needs

The RFC is an enterprise system: Kafka, PostgreSQL, Vector DB, Graph DB, NeoCortex, Redis, OpenShift — 5+ microservices. milk is a local CLI orchestrator with a single binary and file-based session store. The RFC's concepts are valuable; the RFC's infrastructure is not applicable.

The design below extracts the RFC's **cognitive model** and maps it onto milk's architecture, keeping memory self-contained so it can be extracted later into a standalone package with minimal friction.

---

## Cognitive model mapping

| RFC concept | milk mapping |
|---|---|
| Hippocampus | `internal/memory` package — sole memory store |
| Percept | `memory.Percept` — atomic memory unit |
| Engram | `memory.Engram` — Percepts grouped by subject |
| NREM Overnight Cycle | lightweight consolidation run at session end |
| REM / NeoCortex | deferred (no UKB equivalent in milk today) |
| Memory API Gateway | `get_memory` and `record_memory` local agent tools |
| Confidence Weight | `w float64` on each Percept, decays per session |
| Inter-Percept edges | stored in Engram as typed relation records |
| Raw signal ingestion | local agent and Claude both record Percepts; local via tool call, Claude via nonce-tagged emission |
| Vector DB + semantic recall | Phase 2 — currently keyword substring matching in `Store.Query` |
| NL synthesis | local model answers recall query using retrieved Percepts as context |

---

## Package structure

```
internal/memory/
  percept.go       # Percept and Engram types, typed edges, confidence weight
  store.go         # file-based persistence (~/.milk/memory/<session-or-global>.json)
                   #   Store.Delete(id string) (bool, error)
                   #   Store.FindByIDPrefix(prefix string) []Percept
  dedup.go         # Jaccard token-overlap deduplication (DuplicateSimilarityThreshold=0.60)
  recall.go        # query interface: by keyword, by recency, by weight threshold
  consolidate.go   # end-of-session NREM: decay, edge propagation, pruning
  tools.go         # OpenAI function schemas + dispatch for record_memory / get_memory / list_memory
  obs.go           # OpenTelemetry spans and metrics for memory operations
```

> **Note:** `embed.go` (llama.cpp `/embedding`) and `similarity.go` (cosine recall) are Phase 2 —
> not yet implemented. Current recall uses keyword substring matching in `Store.Query`.

The package exposes a `Store` interface. Everything outside `internal/memory` depends only on this interface — isolation boundary for future extraction.

---

## Data model

### Percept

```go
type Percept struct {
    ID        string    // uuid
    Content   string    // bracketed natural-language assertion
    Producer  string    // "user" | "local" | "claude" | "system"
    W         float64   // confidence weight [0,1]
    Roles     Roles     // Neo-Davidsonian: Action, Agent, Recipient, Theme, When, Where
    EngramID  string    // parent Engram (empty = unassigned)
    CreatedAt time.Time
    UpdatedAt time.Time
    Core      bool      // exempt from decay if true
}

type Roles struct {
    Action    string
    Agent     string
    Recipient string
    Theme     string
    When      string
    Where     string
}
```

Role extraction: roles are filled by the local model at record time via a structured extraction prompt. Roles not inferable from the content are left empty — the system never fabricates.

### Engram

```go
type Engram struct {
    ID          string
    SubjectLabel string
    CompositeW  float64    // mean of member Percept weights, recomputed at consolidation
    PerceptIDs  []string
    CreatedAt   time.Time
    UpdatedAt   time.Time
}
```

### Edge

```go
type Edge struct {
    From     string   // Percept ID
    To       string   // Percept ID
    Relation string   // "extends" | "updates" | "derives" | "contradicts"
}
```

Edges are stored as a flat list on the Store. Weight propagation on consolidation:
- `extends`, `updates`, `derives` → increment target `W` by `+0.05` (capped at 1.0)
- `contradicts` → decrement both ends by `−0.10`

### Store file format

```
~/.milk/memory/
  global.json          # cross-session Percepts (user preferences, persistent facts)
  <session-id>.json    # session-scoped Percepts, consolidated at session end
```

JSON file per scope containing `{ percepts: [...], engrams: [...], edges: [...] }`. No external DB. The Store merges global + session-scoped Percepts at query time.

---

## Embedding and recall

milk already calls llama.cpp. The `/embedding` endpoint (OpenAI-compatible) produces a float32 vector for any text. Cosine similarity is computed in-process (pure Go, no deps).

Recall flow:
1. Embed the query string
2. Cosine-compare against all Percept embeddings (stored alongside each Percept)
3. Filter by `w >= min_confidence` (default 0.4)
4. Sort by `α×w + β×recency` (α=0.6, β=0.4, configurable)
5. Return top-k Percepts as context injected into the model prompt

Embeddings are lazily computed: stored per-Percept on first embed, reused thereafter.

---

## End-of-session consolidation (NREM)

Runs automatically when a session ends (on `milk` exit or `/new`/`/drop`). Steps:

1. **Decay** — apply `−0.03` to all non-core session Percepts
2. **Edge propagation** — adjust weights per edge relations (see above)
3. **Prune** — remove Percepts with `w ≤ 0`
4. **Promote** — Percepts with `w ≥ 0.6` and `Core=false` are candidates for promotion to `global.json`
5. **Merge** — promoted Percepts are written to global store; session file is archived/deleted

Promotion to global is the milk equivalent of the RFC's REM phase — long-term consolidation without a UKB.

---

## Agent integration

### Local agent tools

Five tools added to `internal/agent/local/tools.go` via `memory.Schemas()`:

**`record_memory`** — agent explicitly records a Percept:
```json
{
  "content": "User prefers flat file output over JSON when possible",
  "subject": "user preferences",
  "producer": "user"
}
```

The optional `producer` field accepts `"user"` | `"local"` | `"claude"` | `"system"`. When `producer: "user"` is set, the Percept is recorded at `W=1.0` (instead of the default `W=0.7`), ensuring it survives consolidation and promotes to GLOBAL reliably.

**`get_memory`** — agent retrieves relevant Percepts by keyword:
```json
{
  "query": "user's preference for output format",
  "min_confidence": 0.4,
  "max_results": 5
}
```

**`list_memory`** — agent lists all Percepts with optional filters:
```json
{
  "scope": "global",
  "producer": "user",
  "min_w": 0.5,
  "pattern": "output format"
}
```

**`forget_memory`** — agent removes a Percept by ID:
```json
{
  "id": "a1b2c3d4"
}
```
The `id` field accepts a short prefix (4–64 hex chars) with or without a leading `#`. Returns an error if the prefix matches multiple Percepts (ambiguous) or none.

**`export_session`** — agent exports the current session transcript (metadata + full history):
```json
{
  "format": "text",
  "output_path": "/tmp/session-2026-05-15.txt"
}
```
Format `text` (default) produces a human-readable transcript; `json` produces raw session JSON.

System prompt addition in `local.go`:
```
- Use get_memory before answering questions that reference past context or stated preferences. Use record_memory when the user states a preference, makes a decision, or shares a fact worth remembering across sessions. Use list_memory when the user asks what is stored in memory. Use forget_memory when the user asks to forget or remove a specific percept (accepts short ID with or without leading #). Use export_session when the user asks to save or view the full session.
```

### Auto-recording

At the end of each turn, the system (not the model) auto-records a summary Percept:
- Role: `system` producer
- Content: compressed 1-sentence summary of what happened this turn (generated by local model via a short summarization call)
- `W = 0.5` initial weight (neutral, decays if never reinforced)

This mirrors the RFC's automatic ingestion path — the agent doesn't need to explicitly record everything.

### Claude agent

Claude participates in memory both as a reader and as a writer (see ADR-0022).

**Read path.** At the start of each Claude turn, relevant Percepts (top-5, filtered to
`ConsumerEscalation` or `ConsumerAll`) are rendered as a `[Remembered facts]` block inside
`BuildContext` and injected via `--append-system-prompt`.

**Write path.** Every Claude turn also receives a `MemoryInstruction` fragment (re-injected on
every `--resume` turn to survive context compaction). The instruction tells Claude to emit atomic
facts using session-specific nonce tags:

```
<milk:percept:NONCE>fact in one sentence</milk:percept:NONCE>
```

The nonce is a 6-character alphanumeric string generated fresh per session by
`claude.GenerateNonce()`. milk's stream layer (`perceptWriter`) intercepts and strips these tags,
recording each fact into the store with `ProducerEscalation`. The tags never appear in Claude's
displayed output.

**Consumer hints.** Claude may prefix a tag body with `@local: ` or `@claude: ` to restrict which
agent receives the percept at injection time. Unprefixed facts are visible to both agents.

---

## Router integration

The router (`internal/router`) currently scores prompts by keywords and token count. Memory integration: when routing, the router can query the store for Percepts matching the prompt — high-`w` Percepts about "user prefers local for X" can nudge the routing score toward local even if the signal scorer would otherwise escalate.

This is optional / phase 2.

---

## Modularity and reuse boundary

The `internal/memory` package is self-contained:
- No imports from `internal/agent`, `internal/router`, `internal/session`
- Depends only on stdlib + llama.cpp HTTP client (extracted to a tiny `embed.go`)
- `Store` is an interface — filesystem implementation is the default; future implementations (HTTP, SQLite) can be swapped in

To extract as a standalone package later: `go mod` rename + replace the llama.cpp client with a configurable `EmbedFunc func(string) ([]float32, error)`.

---

## What is NOT in scope (RFC concepts deferred)

| RFC feature | Reason deferred |
|---|---|
| NeoCortex / UKB reconciliation | No enterprise KB in milk |
| Kafka ingestion | Not applicable to a local CLI |
| PII encryption | Percepts are local-only; user controls the file |
| Multi-user partitioning | milk is single-user by design |
| Connector plugins | No external sources (calendar, email) wired yet |
| Core memory scoping per agent/session | Phase 2 — global Core flag is sufficient for v1 |

---

## Implementation phases

### Phase 1 — Foundation ✓ complete

- `internal/memory`: Percept, Engram, Edge types
- File-based store (read/write/merge global+session)
- Decay + pruning at session end via `Consolidate()`
- `record_memory`, `get_memory`, `list_memory` tools in local agent
- `export_session` tool — session transcript as text or JSON, optional file write
- `/learn <statement>` slash command — writes a user Percept directly to global store (`Core=true`, `W=1.0`, `Producer="user"`)
- `/memory [global|session|<pattern>]` slash command — lists Percepts with optional scope/pattern filter
- `/export [json|<path>]` slash command — dumps the current session transcript

#### Phase 1 additions ✓ complete

- **Memory panel** (`cmd/milk/panel_memory.go`) — right-side TUI panel (34 chars wide), open by default, toggled with `/panel memory`. Shows SESSION / GLOBAL / GLOBAL (core) sections. Polls every 5s. Each percept shows a short ID (`#<first-6-hex-chars-of-UUID>`, dim) on the first line, content wrapped to 2 lines max, weight right-aligned. Percepts updated within 60s are highlighted bold+yellow.
- **Percept short IDs** — every percept in the panel shows `#<first-6-hex-chars>` for cross-command referencing.
- **`/forget <pattern or #id>`** slash command — fuzzy search by content substring or exact `#id` prefix. Shows numbered list with short IDs on ambiguity, asks for confirmation (`y` / position / `#id`), then calls `Store.Delete(id)`.
- **`/memory show <pattern or #id>`** slash command — shows full percept details (ID, scope, W, producer, core, content, timestamps, roles) using `FormatListVerbose`.
- **`Store.Delete(id string) (bool, error)`** — removes a percept by exact UUID from the store.
- **`Store.FindByIDPrefix(prefix string) []Percept`** — finds percepts whose UUID begins with the given prefix (used for short-ID resolution).
- **`record_memory` `producer` field** — agents can pass `producer: "user"` to record user-stated facts at `W=1.0` instead of the default `W=0.7`.
- **`promoteThreshold` lowered from 0.8 to 0.6** — local/claude-produced session percepts (W=0.7, decaying to 0.67 after one session) now reach the promotion threshold and can enter GLOBAL storage via normal use.

### Phase 2 — Recall quality
- Embedding via llama.cpp `/embedding`
- Cosine similarity recall
- Auto-record turn summaries
- `/unlearn <statement>` slash command — removes or invalidates a matching user Percept from global store

### Phase 3 — Graph
- Edge classification (extends/updates/contradicts) via local model
- Weight propagation on consolidation
- Engram grouping by subject label

### Phase 4 — Routing and Claude injection ✓ partially complete

- Percept injection into Claude `--append-system-prompt` ✓ complete (see ADR-0022)
- Claude percept emission via nonce tags ✓ complete (see ADR-0022)
- Memory-aware routing nudge — deferred

---

## Open decisions (need resolution before Phase 1)

1. **Decay model**: unconditional −0.03 per session end (Model A) vs. skip decay if used this session (Model B). Recommendation: Model A — simpler, no zombie Percepts.
2. **Session vs. global scope**: should all Percepts start session-scoped and promote on consolidation, or should `record_memory` accept an explicit `scope: "global"` flag? Recommendation: start session-scoped, auto-promote at consolidation.
3. **Role extraction at record time**: call the local model for a structured role-extraction prompt, or accept roles as optional fields from the caller? Recommendation: optional at record time, lazy extraction at consolidation (cheaper).
4. **Embedding storage**: store raw float32 vectors in the JSON file alongside each Percept (simple, no extra dep) or use a sidecar SQLite with vector extension? Recommendation: JSON for Phase 1, revisit at scale.
