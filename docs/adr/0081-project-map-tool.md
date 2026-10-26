# 81. project_map: Heuristic Multi-Language Outlines, Ranked Pruning, Persistent Index

- **Status:** accepted
- **Date:** 2026-10-25

## Context

Issue #324 asked for a token-cheap project map — files → symbols/signatures,
never bodies — so agents stop paying `find_files` + `grep` + `read_file`
tokens just to orient in an unfamiliar repo. The v1 slice shipped Go-only via
`go/ast`; the follow-up asked for JS/TS, Python, Java and Rust, non-code
framing, persistent indexing, and to take Aider's repo-map
(https://aider.chat/2023/10/22/repomap.html) into account.

Aider's design, and what this tool takes from it:

- **definition lines, not bodies** — already the v1 contract;
- **a token budget with ranked selection** — Aider keeps the most
  *referenced* identifiers under `--map-tokens` instead of truncating
  arbitrarily. This ADR adopts the idea in reduced form (see below);
- **tree-sitter for extraction** — deliberately *not* adopted (see below).

## Decision

### Extraction: stdlib + heuristics, no parser dependency

`go/ast` for Go (stdlib, exact). JS/TS/JSX/TSX, Python, Java and Rust go
through line/brace scanners (`project_map_langs.go`): string/comment-stripped
line scans with depth/indent stacks that outline types, funcs/methods with
their signatures, and consts — qualified as `(Class).method` like the Go
form. Known and accepted imprecision (documented per extractor): Java
package-private methods without modifiers, exotic multiline signatures,
constructors identified by name shape.

Why not tree-sitter: milk ships pure-Go single binaries
(`release.yml` builds with `CGO_ENABLED: 0`), and the CGO bindings rule
themselves out. The pure-Go alternative is bundling WASM grammars per
language (tree-sitter + wazero) — hundreds of KB of binary assets and a
grammar-versioning burden, for precision these five languages' declaration
syntaxes rarely need. Why not universal-ctags: an external binary would make
the outline differ between machines (nondeterministic output for tests *and*
for agent behavior) and its output carries no signatures. The WASM path
stays open as the precision upgrade if the heuristics ever mislead.

### Framing: a non-code orientation header

Before the symbols, the map names the project (first known manifest: go.mod
module, package.json, Cargo.toml, pyproject.toml) and its non-code
landmarks (`docs:` top-level markdown + docs tree; `build/config:` curated
manifests, task/make files, CI under `.github/`) — the issue's open question
5, answered yes: agents need the project's shape *beyond* symbols.

### Budget: degrade, then rank, then bound

The v1 ladder grows one rung: signatures → names-only → **reference-ranked
names** → bare file tree, with `internal/textbudget` as the hard bound. The
ranked rung is Aider's idea, reduced: instead of a dependency-graph
PageRank, each symbol's rank is the count of *other* files in the map that
mention its identifier (word-boundary match, ≤256 KB per file, computed
only when the budget forces pruning). Least-referenced symbols drop first
(deterministic tie-break: file order, then later declarations first); each
file keeps at least one symbol and shows `…+N` for its losses. A PageRank
over imports/calls is the fidelity upgrade; the count is what pruning needs
at this size.

### Persistent index: `~/.milk/outline-cache/<base>-<hash>.json`

Per-file summaries (mtime + size keyed) persist across sessions and agents —
multi-agent sessions multiply the orientation cost, so re-parsing the same
tree twice is exactly the waste this tool exists to remove. The file follows
the folder-bound `<name>-<hash>.json` convention of ADR-0078 (project root
hashed with `session.CWDHash`), lives under the state root (`config.Dir()`,
so sandboxed roots stay sandboxed), writes atomically, and is an
optimization only: every entry is re-verified by mtime+size on use, a
missing or corrupt index simply means "parse again". Stale entries for
deleted files are left behind (bounded by project size, never consulted).

## Amendment — pushed context (`project_map_context`, 2026-10-26)

The "tool vs injected context" question (issue #324, PR #330) is resolved as
**hybrid**: the tool stays the pull path, and a per-agent *push* was added on
top. `project_map_context` (agent field, see docs/guides/providers.md) is a
tri-state — string or boolean aliases (`true`=full, `false`=off):

- **`minimal` (default)** — the framing header (project identity, docs,
  build/config landmarks) plus the top-level directory shape, ≤1200 chars,
  rebuilt per turn. Symbols stay behind the tool pull: this captures
  Aider's "oriented from turn one" property at a fraction of the token
  cost and cannot go stale at the symbol level.
- **`off`** — pure tooling (the pre-amendment behavior).
- **`full`** — Aider's regime: the budgeted symbol map (the tool's
  full → names → reference-ranked → tree ladder) rebuilt every turn and
  injected into the prompt.

Both paths share one builder (`projectMapBuild`) — one map implementation,
two delivery routes. The block rides the system prompt's optional-guidance
layer, so the context-reduction ladder (issue #236) sheds it at its first
level under pressure, and the closing pointer at the tool is included only
when `project_map` is actually registered (never advertise a missing tool,
issue #172). Why push at all: orientation maps pay off for capable models
(Aider's measurements were GPT-4-class) and the first turn of an
unfamiliar repo is where a pull-only design is weakest. Why not push
symbols by default: per-turn cost and stale-map trust — both opt-in via
`full`.

## Amendment — window-derived default caps (2026-10-26)

The tool's default caps are no longer fixed constants: they derive
growth-only from the agent's resolved context window
(`config.Config.AgentContextWindowTokens` — explicit `context_window_tokens`,
else the models.dev catalog lookup):

- `files = clamp(window/640, 200, 1000)`
- `chars = clamp(window/6, 6000, 50000)`

Floored at the historical 200/6000, so unknown and small windows behave
exactly as before the derivation existed — nothing shrinks; capped at the
tool's hard caps. The same budget sizes the `full` push block. Mechanics: the
turn-runner chain wires `WithProjectMapBudget(cfg.AgentContextWindowTokens(ac))`
next to the other window-derived turn settings; the budget rides the run's
context (`withProjectMapBudget`, the `WithImageParts` precedent) so
`runProjectMap` and `projectMapContextBlock` read one source without widening
`schemas()`/`dispatchTool` signatures; `applyProjectMapSchemaBudget` stamps
the actual defaults into the tool's parameter descriptions so the model sees
its real limits instead of stale constants; `cloneForBackground` copies the
fields so background jobs keep the parent's derivation (the supportsVision
lesson). Call-site `max_files`/`max_chars` args and the hard caps still win
per call; unwired runs (bare `New()`, eval adapters) keep the constants.

## Consequences

- Five languages outline with signatures; everything else degrades to the
  file tree (the v1 contract) rather than guessing.
- Repeated `project_map` calls — same session, later session, sibling agent —
  parse nothing on an unchanged tree (pinned by tests, including a
  simulated restart).
- The ranked rung makes small budgets useful: the *hub* symbols survive
  instead of the alphabetically early files.
- The precision ceiling is the scanner set; if a language's heuristics
  mislead in practice, the WASM tree-sitter path (or per-language native
  parsers like `go/ast`) is the documented upgrade, behind the same
  `projectMapExtract` seam.
