# Evaluation harness (`milk eval`)

> Runs scenario prompts against real agent backends — the `claude` CLI or the actual `milk` TUI, each driven inside a tmux session exactly as a human would — scores the responses with an LLM judge against a rubric, and reports token/cache/quality comparisons. This is for anyone validating milk against their own configured agents (which primary/escalation agent actually performs best on their real workloads, not just milk's own maintainers benchmarking milk internals), and for comparing against other harnesses/backends on the same scenarios. `--scenarios`/`--results` default to `eval/scenarios`/`eval/results` relative to your current working directory — see [Scenarios](#scenarios) for where those defaults come from and how to point at your own directory instead.
>
> `milk eval` is a subcommand of the main `milk` binary (`cmd/milk/main.go` mounts `eval.Command()`) — there is no separate `milk-eval` binary. Build with `task build` / `task build:local` like any other milk command.

## Quick start

```sh
milk eval --list                                          # available adapters
milk eval run --agents claude-code,milk-tui                # run every scenario against both
milk eval run --agents claude-code --category smoke        # fast pipeline sanity check
milk eval report --results eval/results                    # re-print the last report
```

---

## Commands

### `milk eval run`

Runs scenarios against one or more adapters, judges each turn, and prints + persists a comparison report.

| Flag | Default | Description |
| --- | --- | --- |
| `--agents` | all registered | Comma-separated adapter names. Supports per-adapter options — see below. |
| `--scenarios` | `eval/scenarios` | Directory of scenario YAML files. |
| `--category` | (none) | Filter to one category (`smoke`, `code_generation`, `debugging`, `refactoring`, …). |
| `--multi-turn` | `false` | Only run scenarios with `multi_turn: true`. |
| `--results` | `eval/results` | Output directory; writes `results.json`. |
| `--judge-agent` | primary agent | Agent name from `~/.milk/config.json` to use as the LLM judge instead of the primary agent — see [Judging](#judging). |

### `milk eval judge`

Re-scores an existing `results.json` against the scenario rubrics without re-running the agents — useful after fixing a judge bug or rubric, or to try a different `--judge-agent`. Takes `--results`, `--scenarios`, `--judge-agent` (same meaning as above).

### `milk eval report`

Regenerates the printed report from an existing `results.json`. `--cache-only` prints just the cache-hit analysis; `--json` emits machine-readable output; `-o/--output` writes to a file instead of stdout.

### `milk eval --list`

Prints registered adapter names and a usage hint. New adapters register themselves via `eval.Register(name, factory)` in an `init()`, same pattern as `eval/adapter_claude.go` / `eval/adapter_milk.go`.

---

## Scenarios

The default scenarios ship as YAML files under `eval/scenarios/` in the milk repo — there's nothing to install or generate, they're just checked-in examples you can read, copy, or run as-is. `--scenarios`/`--results` (see [Commands](#commands)) resolve relative to your current working directory and default to `eval/scenarios`/`eval/results`, so running `milk eval run` from the repo root picks them up automatically. Running from elsewhere, or want your own scenario set entirely (a different directory, a different repo, scenarios specific to your own project)? Pass `--scenarios <path>` — any directory with the same file format works, the default is just a convenient starting point, not a requirement.

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
| `Turns` | `turns` | list of `{prompt, rubric}` | Multi-turn steps, sent in order as separate turns in the *same* adapter session (same tmux session, same conversation). |
| `Rubric` | `rubric` | list of criteria | Top-level rubric for single-turn scenarios (i.e. those using `prompt:`, not `turns:`). |
| `Expected` | `expected` → `{contains, files_changed}` | — | Parsed but **not currently consumed** — see caveat below. |
| `CacheExpect` | `cache_expect` → `{min_cache_hit_rate_after_turn1, no_cache_recreate_after_turn1}` | — | Automated cache-reuse assertions for multi-turn scenarios — see [Multi-turn example](#multi-turn-example-with-cache_expect). |

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
| `milk-tui` | The installed `~/.local/bin/milk` binary (built via `task build`), launched inside a fresh tmux session per scenario. Reads `~/.milk/sessions/<id>.json` for the response and token usage. | `milk` built and configured (`~/.milk/config.json`) |

Both adapters type the prompt into the target program's interactive prompt via `tmux send-keys`/`send-enter` — the same input path a human would use, not an API shortcut — then poll for turn completion (Claude Code: transcript reaches `stop_reason: "end_turn"`; milk: session state returns to `ROUTING`).

### Per-adapter options

`--agents` supports a bracket syntax to pass adapter-specific options: `name[opt1,val1,opt2,val2,...]`. Commas inside brackets don't split across adapters — `--agents "claude-code[--cache-cooldown,5m],milk-tui[--agent,mimo-local]"` is two adapters, not four.

| Adapter | Option | Example | Effect |
| --- | --- | --- | --- |
| `milk-tui` | any `milk` CLI flag | `milk-tui[--agent,mimo-local]` | Forwarded verbatim onto the `milk` launch command — e.g. pin the primary or escalation agent for this eval run without touching `~/.milk/config.json`. |
| `claude-code` | `--cache-cooldown <duration>` | `claude-code[--cache-cooldown,5m]` | Before starting a new session, wait until at least `<duration>` has elapsed since the last `claude-code` turn completed — anywhere, including in a *previous* `milk eval run` invocation (state persists to `~/.milk/eval-claude-cache-activity`). Opt-in; omit for unchanged (possibly cache-warm) behavior. See [Prompt caching](#prompt-caching-and---cache-cooldown). |
| `claude-code` | any other flag | `claude-code[--append-system-prompt,"..."]` | Forwarded verbatim onto the `claude` launch command. |

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

`GenerateReport` prints per-scenario tables (`correctness`/etc. scores, `Weighted Score`) and per-agent metrics (`Tokens (in/out)`, `Cache (create/read)`, `Cache hit rate`, `Cost USD`, `Duration`, `Tool calls`), followed by an aggregate section (by weighted score, and by cost-efficiency). `GenerateCacheReport` (`--cache-only`) prints just the cache analysis. `GenerateJSON` (`--json`) is the same data as a machine-readable structure — see `ScenarioResult`/`AgentResult` in `eval/scenario.go` and `eval/harness.go`.

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

- **`milk-tui` shells to the *installed* binary** (`~/.local/bin/milk`), not whatever's in your working tree — run `task build` after code changes before evaluating, or the harness silently scores a stale binary.
- **`CostUSD` is unpopulated** (see [Reports](#reports)).
- **`--cache-cooldown` is `claude-code`-only** — `milk-tui` doesn't exhibit this caching behavior (no Anthropic-style prompt cache in the local-model path), so there's nothing for it to control there.
- **`expected:` is a no-op** (see [Scenarios](#scenarios)) — it parses but nothing checks it. Scoring is entirely LLM-judge-driven today.
