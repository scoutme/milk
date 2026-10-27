# ADR-0086: Eval version matrix — compare milk versions side by side

- **Status**: Accepted, implemented
- **Date**: 2026-10-27
- **Files**: `eval/experiment.go`, `eval/adapter_milk.go`, `eval/cli.go`, `eval/versions_test.go`

## Context

ADR-0085's experiment files made a measurement suite repeatable, but the
scenario that motivated it — *repeating the same request across milk versions
to verify their effectiveness* — still needed external scripting: one `milk
eval run` per version, separate results dirs, and hand-rolled comparison. Two
milk-tui adapters in one run were impossible anyway: results are keyed by
`adapter.Name()`, which was the fixed `"milk-tui"`, so the second adapter
would silently overwrite the first.

## Decision

**A `versions:` matrix in the experiment file: one labeled milk-tui adapter
per milk binary, all running the same suite in one run.**

1. **Labels.** `milk-tui[--label,<name>]` renames an adapter in report
   columns and `results.json` keys (default: `"milk-tui"`). Labels are
   validated unique — a collision would silently drop one version's data.
2. **Sources.** Each `versions:` entry is `ref:` (a git ref of the milk repo,
   built via `go build ./cmd/milk` in a throwaway git worktree) or
   `milk_binary:` (a prebuilt binary, `~` expanded, wins over `ref`).
3. **Build caching.** Ref-built binaries are cached under
   `~/.cache/milk-eval-builds/` keyed by the resolved commit — re-runs skip
   the build; a moving branch rebuilds only when its commit changes. Builds
   write to a temp name and rename (concurrent evals can't tear the cache);
   worktrees are removed either way and compiler output surfaces on failure
   before any scenario runs.
4. **Comparison for free.** Versions are ordinary adapters, so the existing
   side-by-side report (weighted scores, tokens, tool calls, duration, budget
   verdicts) and `Repetition Statistics` (mean ± sd per version) are the
   version comparison; `versions` combines with `agents` (e.g. a claude-code
   baseline), `repetitions`, `sandbox:` and `budget:` unchanged.

## Consequences

- One command compares effectiveness across versions on identical scenarios,
  judge, budgets and always-fresh sandboxed state — no external scripting.
- Version runs build from source on first use: an old ref that no longer
  builds with the current toolchain fails loudly at resolution time, with the
  compiler output, before any run starts.
- The adapter identity in reports is the label; which exact commit was behind
  a `ref:` label is in the eval output (build/cached path), not in
  `results.json` — label refs explicitly when that matters.
