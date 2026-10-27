# ADR-0085: Eval experiment files, fresh sandboxed runs, and metric budgets

- **Status**: Accepted, implemented
- **Date**: 2026-10-27
- **Files**: `eval/experiment.go`, `eval/harness.go`, `eval/scenario.go`, `eval/report.go`, `eval/adapter_milk.go`, `eval/adapter.go`, `eval/cli.go`, `cmd/milk/outputformat.go`, `cmd/milk/main.go`, `eval/experiments/metrics-testbed.yaml`

## Context

Driving `milk eval` for measurement runs (the same request, repeated, verifying
tokens / tool calls / time) had four gaps:

1. **No suite definition.** A run was assembled from flags; repetitions,
   sandboxing and assertions needed external scripting.
2. **State contamination.** milk-tui runs used the live `~/.milk` root, so the
   *n*th run saw the first run's sessions, percept memory and history — the
   exact influence a measurement must exclude.
3. **Tool calls invisible.** The batch `--output-format stream-json` wire never
   emitted `tool_use`/`tool_result`, so the milk-tui adapter reported
   `Tool calls: 0` regardless of reality.
4. **Mutating tools denied.** Batch runs have no interactive permission ask, so
   `bash`/`edit_file`/`write_file` were denied and code-editing requests
   degraded to read-only. Worse, the per-agent `dangerously_skip_permissions`
   flag was only ever read from the *claude-cli* entry
   (`cliAgentConfig(cfg)`), so setting it on a local primary did nothing.

## Decision

1. **Tool activity on the batch wire.** `runSinglePrompt` records the run's
   history start and, before the terminal `result`, expands every new
   assistant turn's recorded tool trail (`session.TrailStep`) into §6.2 message
   pairs: one `assistant` message per tool-calling iteration (narration text +
   `tool_use` blocks) followed by one `user` message with the matching
   `tool_result` blocks. Post-hoc reconstruction, not live deltas — the batch
   stream already emits the final text after the turn; conversation order
   (tools → answer → result) is preserved on the wire. `result.num_turns`
   becomes `1 + tool iterations` instead of a hardcoded 1. Trail fidelity
   limits (size caps, cleared results) are inherited from capture — documented,
   not re-solved here.
2. **Experiment files** (`eval/experiment.go`): one YAML defines the full
   suite — adapter specs, scenario dir + `select` filters, `repetitions`,
   judge, results dir, `sandbox`, `budget`. `milk eval run --experiment f.yaml`
   executes it; explicitly set flags override fields one by one. Scenario paths
   resolve relative to the experiment file first (self-contained suites).
3. **Fresh sandboxed runs** (`eval/adapter.go` `Sandboxable`, `milk-tui`'s
   implementation): every adapter start gets a brand-new state root
   (`MILK_CONFIG_ROOT=<temp>`), seeded with a verbatim copy of the *current*
   `config.json` — "my current agent set, none of the state". `allow_tools`
   additionally writes `dangerously_skip_permissions: true` onto the seeded
   copy only (the live config is never touched). Per-repetition freshness is
   free from the Start/Stop lifecycle. Adapter pseudo-flags (`--sandbox`,
   `--sandbox-seed`, `--allow-tools`, `--milk-bin`) expose the same without an
   experiment file; experiment values never override explicit pseudo-flags.
   `claude-code` is not sandboxable and runs unsandboxed with a note.
4. **Metric budgets** (`Budget` in `eval/scenario.go`): max/min thresholds on
   input/output/total tokens, tool-call count, model turns and wall duration,
   from the experiment's `budget:` block refined per scenario (`MergeBudget`),
   evaluated per run (`Evaluate`) and reported as PASS/FAIL with violations in
   the table, verdict lines and `results.json`. Repetitions are labeled per run
   and summarized as mean ± sd (Repetition Statistics), keeping nth-run
   variance visible instead of averaged away.
5. **Permission wiring fix**: `skipPermissionsFor(cfg, ac)` = the agent's own
   `dangerously_skip_permissions` **OR** the claude-cli entry's (the historical
   session-wide switch). OR — never replace — so both documented usages keep
   working; applies to every local-agent construction site (single-prompt,
   tool-agents, TUI seed).

## Consequences

- A full measurement suite (fixed request × N repetitions × agent set, with
  verified token/tool/time budgets) runs from one file with zero scripting —
  `eval/experiments/metrics-testbed.yaml` is the checked-in testbed.
- Run-to-run numbers become comparable: fresh state per run removes
  memory/session contamination; `milk-tui[--cache-cooldown]`-style provider
  cache warmth is out of scope for milk (documented), and the repetition stats
  surface whatever variance remains (e.g. provider-side prompt-cache effects).
- Tool-call counts/lists are accurate for local-provider turns only; a
  `claude-cli` turn inside milk emits no trail (its eval path is the
  `claude-code` adapter, which reads Claude Code's transcript).
- Sandboxed runs lose `~/.milk/mcp_oauth` tokens (empty root) — MCP servers
  needing auth warn and are skipped; this is by design (state isolation) and
  documented in `docs/reference/eval.md`.
- The batch wire grows variable-length output (message pairs per tool
  iteration); consumers that assumed exactly three lines must iterate events
  instead (`docs/guides/tooling.md` updated).
