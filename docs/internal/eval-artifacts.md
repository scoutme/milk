# Eval artifact policy: repo vs local vs elsewhere

What belongs in the milk source repo, what stays machine-local, and what is
maintained outside the repo. The tree otherwise mixes three kinds of eval
material — generic machinery, pristine examples, and one-off run files — and
without a boundary the run files creep toward being committed and the examples
decay into someone's scratch pad.

## 1. In the milk repo (tracked, versioned)

Generic machinery and the shared, versioned benchmark. Litmus test: *would
another milk user or contributor benefit from this, and does its meaning not
depend on one machine's config or one specific run?*

- **Harness code**: `eval/*.go` — adapters (`claude-code`, `milk-tui`,
  `mimo-code`, …), harness, judge, report, cache, experiment engine, plus all
  `*_test.go`.
- **Recorded stream fixtures**: `eval/testdata/**` (e.g. `testdata/mimo/*.jsonl`).
  Real captured outputs, but pinned golden *inputs* for parser tests: small,
  reviewable, deterministic. Keep them trimmed to what the test asserts on.
- **The standard scenario suite**: `eval/scenarios/**` including `_base.yaml`.
  Scenarios and rubric weights are versioned here on purpose — a score is only
  comparable to another score if the rubric that produced it is pinned to a
  commit (the routing run proved this: a missing criterion silently renormalized
  the weighted score).
- **Pristine examples**: `eval/experiments/metrics-testbed.yaml` and
  `eval/experiments/testbed-scenarios/`. These are the worked example
  `docs/reference/eval.md` links to. They must stay generic and unmodified by
  real use — copy them, never repurpose them (see their headers).
- **Docs and ADRs** covering eval (`docs/reference/eval.md`, ADR-0085, this file).
- **Core fixes** that eval work surfaces (e.g. the one-shot `--turn-timeout`
  fix) — those are product changes and ship normally.

## 2. Machine-local (outside the source tree)

Everything whose content encodes one machine, one config, or one specific run.
It lives under the **eval artifact home** — `<milk state root>/eval`
(`~/.milk/eval`, or `$MILK_CONFIG_ROOT/eval` in a sandbox; `eval/paths.go`, and
`milk eval`'s default `--results`) — deliberately **not** in the working tree.
`/tmp` is only for throwaway work: run results must survive a reboot.

- **`~/.milk/eval/results/<run>/`** — every run's `results.json`, generated or
  hand-made reports, partials. This is where `milk eval run` / `judge` / `report`
  write and read by default; nothing lands in the source tree anymore.
- **`~/.milk/eval/experiments/`** — your own experiment definitions (comparison
  suites like `mimo-3way.yaml`, repetitions studies, budget probes). They embed
  your agent specs (`--sandbox-seed,~/.milk/config.json`, `--label`s), your
  result paths and today's question — not upstream material. Copy an example
  from `eval/experiments/` to start one.
- **Scratch build/runtime state**: detached worktrees used to build merged
  test binaries (e.g. `/tmp/milk-both`), rebuilt `~/.local/bin/milk`, sandbox
  state roots.
- **Agent runtime state**: `~/.milk/` (sessions, memory, otel, mcp_oauth),
  `~/.config/mimocode/`, `~/.local/state/mimocode/` — never anywhere near the
  repo.

## 3. Maintained elsewhere (published artifacts)

Outputs made for humans to read, not for the tree to carry.

- **Shareable reports** (HTML/Markdown): attach to the PR/issue that acts on
  them, paste into release notes, or keep in a dedicated companion repo (e.g.
  `milk-eval-results`) if you want durable history. The source repo keeps the
  *mechanism* (report code, later `milk eval report --html`), never an instance
  of a report.
- **Long-term result archives** and comparison write-ups: same homes. If a run
  must be reproducible later, commit its *experiment definition* copy
  (annotated) next to the write-up — the `results.json` stays out.

## Why this split

- **Scores are only comparable against a pinned rubric** — scenarios live in
  the repo; run outputs don't, because they are derived data (regenerable from
  a definition + a binary + a config).
- **Run files encode private config** — agent names, seeded config copies,
  token/cost telemetry, machine paths. Committing them leaks config detail and
  rots; committing *definitions* (examples) keeps the methodology.
- **Examples must stay pristine** — docs link at them; an example that someone
  "swapped the prompt" in stops teaching the format and starts confusing it.

## Containment guarantee

What eval does and does not promise about the machine during a run
(the guarantee state per issue [#355](https://github.com/scoutme/milk/issues/355),
[ADR-0098](../adr/0098-eval-filesystem-isolation.md)):

- **With `isolate.mode: auto` / `bwrap` (or `--isolate`)**: writes outside the
  allowlist — scenario workdir, `isolate.rw`, the fresh state root, the
  per-run HOME seed copies — **fail at the OS level** (bubblewrap read-only
  root): no other checkout, no `$HOME` file, no system path can be modified
  by an agent run, and nothing of the run survives outside its workdir,
  seeds and results dir. Enforced, not assumed; proven by the LLM-free
  escape-matrix test in `eval/isolate_test.go`.
- **By default (isolation off)**: there is **no guarantee** — the temp
  workdir and fresh state roots scope what a scenario *invites*, not what an
  agent *can* touch; `file_changes` is a lower bound (workdir only), and
  escapes have happened (the routing-reps incident). `auto` degrades to this
  mode with a loud warning where bwrap is unavailable, so check the run's
  stderr line before trusting the containment claim.
- **Unaffected either way**: run outputs still live under the eval artifact
  home (never the repo), and the judge/scoring path runs unsandboxed —
  isolation wraps the agent processes only.

## Quick reference

| artifact | home |
|---|---|
| adapter/harness/judge code + tests | repo: `eval/` |
| parser fixtures (recorded streams) | repo: `eval/testdata/` |
| scenario suite + rubrics | repo: `eval/scenarios/` |
| worked example experiment | repo: `eval/experiments/` (pristine) |
| your experiment definitions | local: `~/.milk/eval/experiments/` |
| run outputs (results.json, reports) | local: `~/.milk/eval/results/<run>/` |
| reports for sharing/reading | PR/issue notes, `milk-eval-results` repo |
| sessions/memory/otel, sandboxes, worktrees | local dotdirs / tmp |
