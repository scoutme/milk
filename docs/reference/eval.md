# Evaluation harness (`milk eval`)

> Runs scenario prompts against real agent backends — the `claude` CLI (driven inside a tmux session exactly as a human would), the actual `milk` binary (driven as a one-shot batch run consuming its machine-readable `--output-format stream-json` event stream), or mimocode's `mimo` CLI (the same one-shot batch shape over `mimo run --format json`) — scores the responses with an LLM judge against a rubric, and reports token/cache/quality comparisons. This is for anyone validating milk against their own configured agents (which primary/escalation agent actually performs best on their real workloads, not just milk's own maintainers benchmarking milk internals), and for comparing against other harnesses/backends on the same scenarios. Measurement runs get the full toolkit: an [experiment file](#experiment-files) defines the whole suite (adapters, scenarios, repetitions, judge, [sandbox](#sandboxed-runs), [budgets](#metric-budgets)) in one YAML — the same request repeated against always-fresh sandboxed state seeded with your current agent set, verifying tokens, tool calls (count + full list) and time per run, with nth-run variance in the report. `--scenarios` defaults to `eval/scenarios` relative to your current working directory; `--results` defaults to `~/.milk/eval/results` (the eval artifact home under the milk state root — outside the source tree, see [docs/internal/eval-artifacts.md](../internal/eval-artifacts.md)) — see [Scenarios](#scenarios) for where those defaults come from and how to point at your own directory instead.
>
> `milk eval` is a subcommand of the main `milk` binary (`cmd/milk/main.go` mounts `eval.Command()`) — there is no separate `milk-eval` binary. Build with `task build` / `task build:local` like any other milk command.

## Quick start

```sh
milk eval --list                                          # available adapters
milk eval run --agents claude-code,milk-tui                # run every scenario against both
milk eval run --agents milk-tui,mimo-code --category smoke # milk vs mimocode on the smoke check
milk eval run --agents claude-code --category smoke        # fast pipeline sanity check
milk eval run --experiment eval/experiments/metrics-testbed.yaml   # a whole suite from one file
milk eval report --results ~/.milk/eval/results/<run>     # re-print a saved report
```

---

## Commands

### `milk eval run`

Runs scenarios against one or more adapters, judges each turn, and prints + persists a comparison report.

| Flag | Default | Description |
| --- | --- | --- |
| `--experiment` | (none) | Experiment YAML file defining the full suite (adapters, scenarios, repetitions, judge, sandbox, budgets) — see [Experiment files](#experiment-files). Explicitly set flags override its fields. |
| `--agents` | all registered | Comma-separated adapter names. Supports per-adapter options — see below. |
| `--scenarios` | `eval/scenarios` | Directory of scenario YAML files. |
| `--category` | (none) | Filter to one category (`smoke`, `code_generation`, `debugging`, `refactoring`, …). |
| `--multi-turn` | `false` | Only run scenarios with `multi_turn: true`. |
| `--repetitions` | `1` | Run every scenario N times, fully isolated (fresh temp workdir, fresh sandbox per run) — for nth-run variance. |
| `--results` | `~/.milk/eval/results` | Output directory (outside the source tree, under the milk state root); writes `results.json`. |
| `--judge-agent` | primary agent | Agent name from `~/.milk/config.json` to use as the LLM judge instead of the primary agent — see [Judging](#judging). |

### `milk eval judge`

Re-scores an existing `results.json` against the scenario rubrics without re-running the agents — useful after fixing a judge bug or rubric, or to try a different `--judge-agent`. Takes `--results`, `--scenarios`, `--judge-agent` (same meaning as above).

### `milk eval report`

Regenerates the printed report from an existing `results.json`. `--cache-only` prints just the cache-hit analysis; `--json` emits machine-readable output; `-o/--output` writes to a file instead of stdout.

### `milk eval --list`

Prints registered adapter names and a usage hint. New adapters register themselves via `eval.Register(name, factory)` in an `init()`, same pattern as `eval/adapter_claude.go` / `eval/adapter_milk.go`.

---

## Experiment files

An experiment file defines the **full evaluation suite in one YAML file** — adapters, scenario selection, repetitions, judge, sandboxing and metric budgets — so a complete, repeatable run needs no external scripting:

```sh
milk eval run --experiment eval/experiments/metrics-testbed.yaml
```

Explicitly set CLI flags override the experiment's fields one by one (e.g. `--repetitions 5` on the command line beats `repetitions: 3` in the file). `~/` is expanded in both path fields. Relative scenario paths resolve against the experiment file's directory first (an experiment is self-contained), falling back to the working directory; relative result paths behave like the `--results` flag (working directory). Run outputs default outside the source tree, under the eval artifact home (`~/.milk/eval/results/<name>`, i.e. `<milk state root>/eval/results/<name>` — see [docs/internal/eval-artifacts.md](../internal/eval-artifacts.md)).

```yaml
name: metrics-testbed
description: fixed request, repeated                               # optional, shown nowhere yet
agents:                                                           # adapter specs, same bracket syntax as --agents
  - milk-tui
scenarios: testbed-scenarios                                      # scenario dir (default eval/scenarios)
select:                                                           # optional filters
  category: metrics                                               # one category
  names: [fixed-request]                                          # exact scenario names (unknown names = error)
  multi_turn: false                                               # like --multi-turn
repetitions: 3                                                    # N fully isolated runs per scenario (default 1)
judge_agent: ""                                                   # "" = primary agent
results: ~/.milk/eval/results/metrics-testbed                     # default: ~/.milk/eval/results/<name>
sandbox:                                                          # see Sandboxed runs
  mode: fresh                                                     # fresh | off (default off)
  seed: config                                                    # config (default) | none
  allow_tools: true                                               # let batch runs execute mutating tools
  milk_binary: ""                                                 # optional: pin the milk binary to drive
budget:                                                           # metric thresholds asserted per run (see Metric budgets)
  max_input_tokens: 100000
  max_output_tokens: 20000
  max_tool_calls: 30
  max_duration: 5m
versions:                                                         # optional: milk version matrix (see Comparing milk versions)
  - label: head                                                   # report column (keep short; default: the ref / binary basename)
    ref: HEAD                                                     # git ref built on demand (cached per commit)
  - label: v0.8.0
    ref: v0.8.0
```

A working example lives at [`eval/experiments/metrics-testbed.yaml`](../../eval/experiments/metrics-testbed.yaml) (with its request scenario under `eval/experiments/testbed-scenarios/`) — the run-from-the-same-request testbed: one fixed request, repeated N times against always-fresh sandboxed state seeded with your current agent set, verifying tokens, tool calls and time per run and their variance across runs. Keep that example pristine: for your own comparisons, copy it into `~/.milk/eval/experiments/` (machine-local, outside the repo) and edit your copy — what belongs in the repo versus machine-local is defined in [docs/internal/eval-artifacts.md](../internal/eval-artifacts.md).

---

## Sandboxed runs

Without sandboxing, every milk-tui run lives in the real `~/.milk` state root — and the *n*th run then sees the first run's sessions, percept memory, and prompt history, exactly what a measurement run must not. `sandbox.mode: fresh` (or the `milk-tui[--sandbox]` adapter option) gives **every adapter start a brand-new state root** (a `milk-eval-home-*` temp dir, removed on adapter stop — one per scenario run, one per repetition):

- **seeded with your current agent set** (`seed: config`, the default): the fresh root's `config.json` is a verbatim copy of your current one (`~/.milk/config.json`, honoring `--config-root`/`$MILK_CONFIG_ROOT`) — same agents, settings and routing, none of the state (sessions, memory, history, jobs, otel, mcp_oauth all start empty).
- **`seed: none`** seeds nothing — milk starts from first-run defaults.
- **`allow_tools: true`** additionally writes `dangerously_skip_permissions: true` onto every seeded agent: batch runs have no interactive permission ask, so mutating tools (`bash`, `edit_file`, `write_file`, …) are otherwise denied ("requires permission — run interactively or enable skip-permissions") and a code-editing request silently degrades to read-only. Only the sandbox's seeded copy is modified — your live config is never touched.
- **`milk_binary:`** pins the binary the adapter drives (default `~/.local/bin/milk` — run `task build` after code changes, or the harness silently scores a stale binary).

Per-adapter pseudo-flags do the same without an experiment file: `milk-tui[--sandbox]`, `milk-tui[--sandbox-seed,/path/config.json]`, `milk-tui[--allow-tools]`, `milk-tui[--milk-bin,/path/to/milk]`, `milk-tui[--label,<name>]`. Explicit pseudo-flags win over experiment values.

Two caveats: `claude-code` has no sandbox support (it drives a tmux session against the real `~/.claude`; an experiment's `sandbox:` section is skipped for it with a note), and MCP servers needing OAuth are not seeded — their tokens live in the (empty) state root, so the run warns `not authorized` and continues without them.

---

## Comparing milk versions

The version matrix runs the **same suite against several milk binaries side by side** — one labeled `milk-tui` adapter per version — so a single run compares their effectiveness (judge scores, tokens, tool calls, time) on identical scenarios, judge, budgets and fresh sandboxes. Declare it in the experiment file:

```yaml
versions:
  - label: head            # report column / results.json key (default: the ref or binary basename)
    ref: HEAD              # git ref (tag / branch / commit) built on demand
  - label: v0.8.0
    ref: v0.8.0
  - label: patched
    milk_binary: ~/bin/milk-patched   # prebuilt binary instead (wins over ref)
```

- **`ref:`** builds `./cmd/milk` via `go build` in a throwaway git worktree of the milk repo (found via the current working directory) and caches the binary under `~/.cache/milk-eval-builds/`, keyed by the resolved commit — re-runs skip the build; a moving branch rebuilds when its commit changes. Build failures (e.g. an old ref with a different toolchain) surface with the compiler's output before any scenario runs.
- **`milk_binary:`** drives a prebuilt binary as-is (`~` is expanded) — anything from `task build` output to a CI artifact.
- Labels must be unique (they name the report columns and the `results.json` keys — two adapters sharing one would overwrite each other's data).
- `versions` combines with `agents` — e.g. a `claude-code` baseline column alongside the version columns — and with `repetitions` (each version × each repetition fully isolated) and `budget:` (asserted per version per run).
- The report is the usual per-agent comparison with versions as columns (`Weighted Score`, `Tokens`, `Tool calls`, `Duration`, `Budget` per version), plus `Repetition Statistics` per version when repeating.

Without an experiment file the same shape works through the adapter pseudo-flags (each `milk-tui` becomes its own column):

```sh
milk eval run --category smoke --results /tmp/cmp \
  --agents 'milk-tui[--label,head,--milk-bin,~/.local/bin/milk],milk-tui[--label,v0.8.0,--milk-bin,/tmp/milk-v080]'
```

---

## Metric budgets

The automated **verify** half of an evaluation: every run's metrics are checked against thresholds, whatever the judge scores. Budgets come from the experiment file's `budget:` block (defaults for every scenario) and an optional per-scenario `budget:` block, which wins field-by-field. Zero/omitted = no limit. For multi-turn scenarios every metric is the scenario's total across its turns.

| YAML key | Checks | Run metric |
| --- | --- | --- |
| `max_input_tokens` | input tokens ≤ N | sum of `tokens.input_tokens` |
| `max_output_tokens` | output tokens ≤ N | sum of `tokens.output_tokens` |
| `max_total_tokens` | in+out ≤ N | sum of both |
| `max_tool_calls` / `min_tool_calls` | tool-call count within bounds | `len(tool_calls)` |
| `max_num_turns` | model iterations ≤ N | `num_turns` (absent = not checked) |
| `max_duration` | wall time ≤ duration string (`"90s"`, `"2m30s"`); `max_duration_ms` for the integer form | turn `duration` |

Verdicts render per scenario (`Budget [milk-tui]: PASS` / `FAIL — tool calls 25 > max_tool_calls 20`), as a `Budget` row in the metrics table, and as `N/M pass` in the repetition statistics; `results.json` carries the full verdict per run (`agents[].budget`).

---

## Scenarios

The default scenarios ship as YAML files under `eval/scenarios/` in the milk repo — there's nothing to install or generate, they're just checked-in examples you can read, copy, or run as-is. `--scenarios` (see [Commands](#commands)) resolves relative to your current working directory and defaults to `eval/scenarios`, so running `milk eval run` from the repo root picks them up automatically; `--results` defaults outside the source tree, under `~/.milk/eval/results`. Running from elsewhere, or want your own scenario set entirely (a different directory, a different repo, scenarios specific to your own project)? Pass `--scenarios <path>` — any directory with the same file format works, the default is just a convenient starting point, not a requirement.

A file starting with `_` (e.g. `_base.yaml`) is a shared-defaults file, not an executable scenario — it's skipped when loading, and its `default_scoring`/`default_weight` backfill any rubric criterion that omits them in the same directory.

A file holds either one scenario (flat) or several under a `scenarios:` list; a top-level `category:` is inherited by every scenario in the file unless overridden per-scenario.

### Field reference

Matches `Scenario` in `eval/scenario.go` — that struct is the source of truth; this table is a convenience.

| Field | YAML key | Type | Notes |
| --- | --- | --- | --- |
| `Name` | `name` | string | Scenario identifier, shown in reports. |
| `Description` | `description` | string | Human-readable summary — shown in reports, never sent to the agent. |
| `Category` | `category` | string | Inherited from the file's top-level `category:` if omitted per-scenario. Filter with `--category`. |
| `Difficulty` | `difficulty` | `easy` \| `medium` \| `hard` | Informational only — not enforced, not scored. |
| `MultiTurn` | `multi_turn` | bool | Must be `true` for `turns:` to take effect, and to be selected by `--multi-turn`. |
| `Setup.Files` | `setup.files` | list of `{path, content}` | Files written into the scenario's temp workdir before the first turn — see [Adapters](#adapters) for how each adapter's workdir is created. |
| `Prompt` | `prompt` | string | Single-turn shorthand. Ignored once `turns:` is set. |
| `Turns` | `turns` | list of `{prompt, rubric}` | Multi-turn steps, sent in order as separate turns in the *same* adapter session (same conversation — a persistent tmux session for `claude-code`, milk's default session continuation across one-shot batch runs for `milk-tui`, the first turn's captured session resumed via `-s` for `mimo-code`). |
| `Rubric` | `rubric` | list of criteria | Top-level rubric for single-turn scenarios (i.e. those using `prompt:`, not `turns:`). |
| `Expected` | `expected` → `{contains, files_changed}` | — | Parsed but **not currently consumed** — see caveat below. |
| `CacheExpect` | `cache_expect` → `{min_cache_hit_rate_after_turn1, no_cache_recreate_after_turn1}` | — | Automated cache-reuse assertions for multi-turn scenarios — see [Multi-turn example](#multi-turn-example-with-cache_expect). |
| `Budget` | `budget` → `{max_input_tokens, max_output_tokens, max_total_tokens, max_tool_calls, min_tool_calls, max_num_turns, max_duration}` | — | Per-scenario metric budget, winning field-by-field over the experiment's `budget:` — see [Metric budgets](#metric-budgets). |

Each rubric criterion (`rubric:` items, top-level or per-turn):

| Field | YAML key | Type | Notes |
| --- | --- | --- | --- |
| `Criterion` | `criterion` | string | Dimension name, e.g. `correctness`, `efficiency` — shown as its own report column. |
| `Description` | `description` | string | Given to the LLM judge as the scoring instruction for this dimension. |
| `Weight` | `weight` | int | Relative weight in `WeightedScore`. Backfilled from `_base.yaml`'s `default_weight` if omitted. |
| `Scoring` | `scoring` | `binary` \| `scale_1_5` | `binary`: judge returns 0.0 or 1.0. `scale_1_5`: judge returns 1.0–5.0. Backfilled from `_base.yaml`'s `default_scoring` if omitted. |

> **`expected` is currently a no-op.** The YAML parses (`Contains []string`, `FilesChanged []FileExpect{Path, Contains}`) but nothing in `eval/harness.go` or `eval/judge.go` reads `scenario.Expected` — no automated pass/fail assertion is actually checked against it today. Scoring is entirely LLM-judge-driven. Don't rely on `expected:` doing anything until this is wired up; the LLM judge's rubric is the only thing currently gating a score.

### `_base.yaml` — shared defaults

```yaml
# Shared rubric defaults applied to all scenarios in this directory.
# Scenarios can override these per-criterion.
default_scoring: scale_1_5
default_weight: 1
```

Applied per rubric criterion that omits `scoring`/`weight` — see `applyBaseDefaults` in `eval/harness.go`.

### Single-turn example

`eval/scenarios/code_generation.yaml`'s `fix-typo` — the shape every single-turn scenario in `code_generation.yaml` and `debugging.yaml` follows:

```yaml
category: code_generation

scenarios:
  - name: fix-typo
    description: >
      Agent must find and fix a typo in a README file. The word "projcet"
      should be corrected to "project".
    difficulty: easy
    setup:
      files:
        - path: README.md
          content: |
            # My Projcet

            This is a sample projcet that does amazing things.

            ## Installation

            Clone the repo and run `make install`.
    prompt: >
      There's a typo in README.md — the word "projcet" should be "project".
      Please fix it.
    rubric:
      - criterion: correctness
        description: >
          Did the agent replace "projcet" with "project" in both occurrences?
        weight: 3
        scoring: binary
      - criterion: efficiency
        description: >
          Did the agent use a minimal number of tool calls (read + edit, or a
          single edit)?
        weight: 1
        scoring: scale_1_5
```

### Multi-turn example (with `cache_expect`)

`eval/scenarios/refactoring.yaml`'s `add-validation` — `multi_turn: true`, a `turns:` list instead of `prompt:`/`rubric:`, and `cache_expect` asserting the *adapter's own conversation cache* reuses context from turn 2 onward (a different concern from `--cache-cooldown`, which is about cross-*run* reproducibility, not within-session reuse):

```yaml
category: refactoring

scenarios:
  - name: add-validation
    description: >
      Agent adds validation to a config loader, then refactors the validation
      into a separate function. Two turns; tests that the second turn benefits
      from cached context.
    difficulty: easy
    multi_turn: true
    cache_expect:
      min_cache_hit_rate_after_turn1: 0.3
      no_cache_recreate_after_turn1: true
    setup:
      files:
        - path: config/config.go
          content: |
            package config
            # ... (see the real file for the full Load() implementation)
    turns:
      - prompt: >
          Add validation to the `Load` function in config/config.go:
          port must be between 1 and 65535, host must not be empty,
          and db_url must not be empty. Return an error describing
          all validation failures.
        rubric:
          - criterion: correctness
            description: >
              Does the validation check port range, non-empty host, and
              non-empty db_url?
            weight: 3
            scoring: binary
          - criterion: error_messages
            description: >
              Does the error message list all validation failures, not just
              the first one?
            weight: 2
            scoring: scale_1_5

      - prompt: >
          Refactor: extract the validation logic into a separate
          `Validate(*Config) error` function. Add validation for api_key
          (must not be empty). Keep the error accumulation pattern.
        rubric:
          - criterion: correctness
            description: >
              Is validation extracted into a standalone Validate function
              that is called from Load?
            weight: 3
            scoring: binary
          - criterion: completeness
            description: >
              Does Validate also check api_key is non-empty?
            weight: 2
            scoring: binary
```

### Current scenarios

Everything that ships in `eval/scenarios/` today:

| File | Scenario | Category | Difficulty | Turns | Notable fields |
| --- | --- | --- | --- | --- | --- |
| `smoke.yaml` | `say-pong` | smoke | easy | 1 | Minimal pipeline sanity check — no real code editing. |
| `code_generation.yaml` | `fix-typo` | code_generation | easy | 1 | `setup.files` |
| `code_generation.yaml` | `add-function` | code_generation | medium | 1 | `setup.files` |
| `code_generation.yaml` | `write-test` | code_generation | medium | 1 | `setup.files` |
| `debugging.yaml` | `fix-nil-pointer` | debugging | medium | 1 | `setup.files` |
| `debugging.yaml` | `fix-off-by-one` | debugging | easy | 1 | `setup.files` |
| `refactoring.yaml` | `iterative-refactor` | refactoring | medium | 3 | `multi_turn`, `cache_expect` |
| `refactoring.yaml` | `add-validation` | refactoring | easy | 2 | `multi_turn`, `cache_expect` |

None of the shipped scenarios use `expected:` (see the no-op caveat above). `iterative-refactor` is the fullest multi-turn example (3 turns, higher `min_cache_hit_rate_after_turn1`) if `add-validation` above isn't enough — read it directly in `eval/scenarios/refactoring.yaml`.

---

## Adapters

| Adapter | Drives | Requires |
| --- | --- | --- |
| `claude-code` | The `claude` CLI, launched inside a fresh tmux session per scenario, in a throwaway git-initialized temp workdir. Reads Claude Code's own transcript JSONL for the response, token usage, and tool calls. | `claude` CLI on `PATH`, logged in |
| `milk-tui` | The installed `~/.local/bin/milk` binary (built via `task build`, or pinned with `--milk-bin`/`sandbox.milk_binary`), spawned per turn as `milk [flags] "<prompt>" --output-format stream-json`; the adapter consumes the batch JSONL event stream (ADR-0050) for the response, token usage, and tool calls — no session-file scraping. Tool calls come from the stream's `tool_use`/`tool_result` message pairs (local-provider turns; a `claude-cli` turn inside milk emits none). | `milk` built and configured (`~/.milk/config.json`) |
| `mimo-code` | The installed `mimo` (mimocode) CLI, spawned per turn as `mimo run --format json --yolo [-s <sessionID>] -- "<prompt>"` in the scenario workdir; the adapter consumes the JSONL event stream for the response (the final assistant message), token usage (summed over the run's `step_finish` events, reasoning folded into output), cost (populated — mimo reports it per step), and tool calls (deduped by `callID`; mimo re-emits each completed call). Multi-turn turns resume the first turn's captured session via `-s`. | `mimo` on `PATH` (mimocode install) with configured providers |

The `claude-code` adapter types the prompt into the target program's interactive prompt via `tmux send-keys`/`send-enter` — the same input path a human would use, not an API shortcut — then polls for turn completion (Claude Code: transcript reaches the text block of a `stop_reason: "end_turn"` response, or any non-assistant record after one). Claude Code writes one transcript line per content block, each repeating the response's usage, so token usage is counted once per `message.id`. The `milk-tui` adapter instead runs milk headlessly per turn and waits on the stream's terminal `result` event, which carries the response, `usage`/`model_usage` (incl. `cache_read`/`cache_creation`), `num_turns`, `ttft_ms`, `output_tokens_per_second` and `route_history` directly; tool calls are collected from the turn's `tool_use`/`tool_result` message pairs. The `mimo-code` adapter follows `milk-tui`'s headless shape against mimocode: one `mimo run --format json` process per turn, the response taken from the final assistant message's `text` events (earlier pre-tool narration is excluded), usage summed from the per-step `step_finish` tokens (with `reasoning` folded into output tokens — thinking bills as output in Anthropic-style usage too), the run's cost summed into `CostUSD`, and `tool_use` events deduped by `callID` into the tool-call list.

### Per-adapter options

`--agents` supports a bracket syntax to pass adapter-specific options: `name[opt1,val1,opt2,val2,...]`. Commas inside brackets don't split across adapters — `--agents "claude-code[--cache-cooldown,5m],milk-tui[--agent,mimo-local]"` is two adapters, not four.

| Adapter | Option | Example | Effect |
| --- | --- | --- | --- |
| `milk-tui` | any `milk` CLI flag | `milk-tui[--agent,mimo-local]` | Forwarded verbatim onto the `milk` launch command — e.g. pin the primary or escalation agent for this eval run without touching `~/.milk/config.json`. |
| `milk-tui` | `--sandbox` | `milk-tui[--sandbox]` | Fresh milk state root per run, seeded with the current config — see [Sandboxed runs](#sandboxed-runs). |
| `milk-tui` | `--sandbox-seed <path>` | `milk-tui[--sandbox-seed,cfg.json]` | Sandbox seeded from an explicit config.json (implies `--sandbox`). |
| `milk-tui` | `--allow-tools` | `milk-tui[--allow-tools]` | Seed the sandbox with `dangerously_skip_permissions` on every agent so batch runs may execute mutating tools. |
| `milk-tui` | `--milk-bin <path>` | `milk-tui[--milk-bin,./milk]` | Pin the milk binary to drive (adapter pseudo-flag — never forwarded to milk). |
| `claude-code` | `--cache-cooldown <duration>` | `claude-code[--cache-cooldown,5m]` | Before starting a new session, wait until at least `<duration>` has elapsed since the last `claude-code` turn completed — anywhere, including in a *previous* `milk eval run` invocation (state persists to `~/.milk/eval-claude-cache-activity`). Opt-in; omit for unchanged (possibly cache-warm) behavior. See [Prompt caching](#prompt-caching-and---cache-cooldown). |
| `claude-code` | any other flag | `claude-code[--append-system-prompt,"..."]` | Forwarded verbatim onto the `claude` launch command. |
| `mimo-code` | any `mimo run` flag | `mimo-code[-m,anthropic/claude-sonnet-4-5]` | Forwarded verbatim onto the `mimo run` command — e.g. pin the model or agent for this eval run without touching mimocode's config. |
| `mimo-code` | `--mimo-bin <path>` | `mimo-code[--mimo-bin,./mimo]` | Pin the mimo binary to drive (adapter pseudo-flag — never forwarded to mimo). |
| `mimo-code` | `--no-yolo` | `mimo-code[--no-yolo]` | Drop the default `--yolo` from the spawn. Batch runs have no one to answer a permission ask, so removing it lets mimo's own permission defaults deny mutating tools. |

### Prompt caching and `--cache-cooldown`

Claude Code's tool-definitions/system-prompt block is cached server-side (Anthropic prompt caching) independent of workdir or session — a fresh temp dir does **not** force a cache miss, and neither does varying the prompt text, because caching is a prefix match and both of those live *after* the cached prefix in the request (`tools → system → messages`). Back-to-back `claude-code` runs will therefore show `cache_read` tokens on that layer for as long as the cache's ~5-minute TTL hasn't expired, which makes token counts vary depending purely on *when* a run happens relative to other `claude-code` activity — not on anything the scenario or adapter did differently.

If you want reproducible, TTL-independent measurements, add `--cache-cooldown <duration>` (e.g. `5m`, matching the TTL) to the `claude-code` adapter spec. It's opt-in — real usage benefits from this caching, so forcing every run cold isn't "more correct," just more comparable run-to-run.

---

## Judging

`eval.NewJudgeFromConfig` resolves the judge model from `~/.milk/config.json` — by default the same **primary agent** milk itself uses (`cfg.ActiveAgent()`); pass `--judge-agent <name>` to point it at a different configured agent instead.

**Self-evaluation bias:** if you run `--agents milk-tui` where `milk-tui`'s own primary agent is the same model as the default judge, that model is scoring its own output. This isn't an issue for `claude-code` runs (different model), but for `milk-tui` runs consider `--judge-agent` pointed at an independent, ideally stronger model.

The judge prompt asks for a JSON array of `{criterion, score, reasoning}` and tolerates two categories of malformed LLM output: markdown code fences around the array, and literal (unescaped) `"` characters inside `reasoning` text — `parseJudgeScores` repairs the latter before giving up.

---

## Reports

`GenerateReport` prints per-scenario tables (`correctness`/etc. scores, `Weighted Score`) and per-agent metrics (`Tokens (in/out)`, `Cache (create/read)`, `Cache hit rate`, `Cost USD`, `Duration`, `Tool calls`, `Budget`), followed by each agent's **tool-call list in call order** (`Tools [milk-tui] (5): read_file, edit_file, bash`) and the **budget verdict** (`Budget [milk-tui]: PASS` / `FAIL — <violations>`), then an aggregate section (by weighted score, and by cost-efficiency). With `repetitions > 1` (or `--repetitions`), each run is its own labeled section (`Scenario: fixed-request (run 2/3)`) and a **Repetition Statistics** section summarizes nth-run variance per scenario as mean ± sd (`Weighted score`, `Tokens in/out`, `Tool calls`, `Duration`) plus the budget pass rate (`2/3 pass`). `GenerateCacheReport` (`--cache-only`) prints just the cache analysis. `GenerateJSON` (`--json`) is the same data as a machine-readable structure — see `ScenarioResult`/`AgentResult` in `eval/scenario.go` and `eval/harness.go`; per-turn results carry the full tool-call list (name/args/result), `model`, `num_turns`, `ttft_ms`, `output_tokens_per_second`, `stop_reason` and `route_history`, and each run carries its `budget` verdict.

Real output shape (from milk's own report-generator test fixture, not a live run — trimmed to one scenario):

```
=== Agent Evaluation Report ===

Scenario: fix-typo-in-readme
Category: code_generation | Difficulty: easy

                     claude-code    milk-tui
────────────────────────────────────────────
         correctness         1.0         1.0
          efficiency         3.0         4.0
────────────────────────────────────────────
      Weighted Score         3.6         4.2

                     claude-code    milk-tui
────────────────────────────────────────────
     Tokens (in/out)   25,477/18   1,200/340
 Cache (create/read)    25,477/0         0/0
      Cache hit rate        0.0%        0.0%
            Cost USD      $0.096      $0.001
            Duration        2.5s        2.1s
          Tool calls           1           2

=== Aggregate Evaluation Report ===
...
By cost-efficiency (quality per dollar):
                     claude-code    milk-tui
────────────────────────────────────────────
   Avg cost/scenario      $0.180      $0.002
      Quality/dollar        20.9     2,050.0
      Cache hit rate       25.0%       27.9%

Overall winner: milk-tui (90x cheaper)
```

(`Cost USD` is `$0.001`-ish here only because the fixture's numbers are illustrative — see the caveat below; token/cache columns are the reliable comparison today.)

`Cost USD` is currently always `$0.000` — `RunResult.CostUSD` isn't populated by either adapter yet (out of scope for now; token counts and cache columns are accurate and are the more meaningful comparison in the meantime).

---

## Known limitations

- **`milk-tui` shells to the *installed* binary** (`~/.local/bin/milk`), not whatever's in your working tree — run `task build` after code changes before evaluating, or the harness silently scores a stale binary (or pin one with `--milk-bin` / `sandbox.milk_binary`, or let `versions: ref:` build the exact commits — see [Comparing milk versions](#comparing-milk-versions)).
- **`CostUSD` is unpopulated by the milk adapters** (see [Reports](#reports)) — `RunResult.CostUSD` stays `$0.000` for `milk-tui`/`claude-code`. The `mimo-code` adapter *does* populate it (mimo reports per-step cost), so the cost columns are real for the mimo column while milk's remain zero; token counts and cache columns are the reliable cross-adapter comparison either way.
- **`--cache-cooldown` is `claude-code`-only** — `milk-tui` doesn't exhibit this caching behavior (no Anthropic-style prompt cache in the local-model path), so there's nothing for it to control there.
- **`expected:` is a no-op** (see [Scenarios](#scenarios)) — it parses but nothing checks it. Scoring is entirely LLM-judge-driven today (metric `budget:`, by contrast, *is* enforced — see [Metric budgets](#metric-budgets)).
- **Tool-call args/results are the stored trail's** — captured size-capped (2000 chars per arg payload, 8KB per result, older results cleared under a 256KB per-turn budget), so a list built from the trail can show cleared-result gaps on tool-heavy turns.
- **`claude-code` and `mimo-code` are not sandboxed** — `sandbox:` applies to `milk-tui` only (see [Sandboxed runs](#sandboxed-runs)); `claude-code` turns also report tool calls from Claude Code's transcript, not milk's trail. `mimo-code` runs against the live `~/.mimocode` state root (each scenario still starts a fresh mimo session — the adapter resets its `-s` resume key per scenario), and its `RunResult.Model` stays empty because mimo's event stream carries no model identity — the model shows blank in the report instead of being guessed.
- **Older `milk-tui` binaries under-report tools** — the tool-trail expansion and real `num_turns` shipped together with `versions:`, so a column built before them reports `Tool calls 0` / `num_turns 1` even when the model called tools. For version comparisons spanning that boundary, read tool metrics as a lower bound and fall back to the judge and `file_changes` for ground truth (observed comparing `v0.7.0`/`v0.8.0` against current).
- **Judge parse failures are retried, then terminal** — malformed judge JSON (e.g. an unescaped quote inside `reasoning`, a trailing comma) is first salvaged by repair passes and, if still unparseable, re-judged up to 2 more times with an explicit correction ("output only the JSON array, escape all quotes"). A turn is scored `ERR` and dropped from the stats only when all 3 attempts fail to parse.
