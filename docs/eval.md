# Evaluation harness (`milk eval`)

> Runs scenario prompts against real agent backends — the `claude` CLI or the actual `milk` TUI, each driven inside a tmux session exactly as a human would — scores the responses with an LLM judge against a rubric, and reports token/cache/quality comparisons. This is for anyone validating milk against their own configured agents (which primary/escalation agent actually performs best on their real workloads, not just milk's own maintainers benchmarking milk internals), and for comparing against other harnesses/backends on the same scenarios. Current caveat: the default scenario/results paths (`eval/scenarios`, `eval/results`) are relative and only resolve from a milk source checkout — scenario YAML ships in the repo, not with pre-built release binaries, so a from-release install can't run `milk eval` yet without cloning the repo for the scenario files. See [Known limitations](#known-limitations).
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

YAML files under `eval/scenarios/`. A file starting with `_` (e.g. `_base.yaml`) is a shared-defaults file, not an executable scenario — it's skipped when loading, and its `default_scoring`/`default_weight` backfill any rubric criterion that omits them in the same directory.

A file holds either one scenario (flat) or several under a `scenarios:` list; a top-level `category:` is inherited by every scenario in the file unless overridden per-scenario.

```yaml
category: smoke

scenarios:
  - name: say-pong
    description: Agent must reply with the single word "pong".
    difficulty: easy          # easy | medium | hard
    prompt: >                 # single-turn shorthand
      Reply with only the single word: pong
    rubric:
      - criterion: correctness
        description: Did the response contain "pong"?
        weight: 1
        scoring: binary       # binary (0/1) | scale_1_5
```

Multi-turn scenarios use `multi_turn: true` and a `turns:` list (each with its own `prompt`/`rubric`) instead of the flat `prompt`/`rubric` fields — see `eval/scenarios/refactoring.yaml` for a real example, including `setup.files` (workdir files to create before the first turn) and `cache_expect` (asserts on cache reuse across turns, since a real multi-turn conversation should hit cache from turn 2 onward — this is about the *adapter's own* conversation cache, not the cross-run concern `--cache-cooldown` addresses below).

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

`Cost USD` is currently always `$0.000` — `RunResult.CostUSD` isn't populated by either adapter yet (out of scope for now; token counts and cache columns are accurate and are the more meaningful comparison in the meantime).

---

## Known limitations

- **Relative default paths** (`eval/scenarios`, `eval/results`) only resolve when run from a milk source checkout — scenario YAML isn't packaged with pre-built release binaries, so a from-release install currently needs `git clone` (at least for the `eval/scenarios/` directory) before `milk eval run` has anything to run. Point `--scenarios`/`--results` at any directory to work around this in the meantime; making the harness fully standalone (e.g. embedding default scenarios in the binary) is open.
- **`milk-tui` shells to the *installed* binary** (`~/.local/bin/milk`), not whatever's in your working tree — run `task build` after code changes before evaluating, or the harness silently scores a stale binary.
- **`CostUSD` is unpopulated** (see [Reports](#reports)).
- **`--cache-cooldown` is `claude-code`-only** — `milk-tui` doesn't exhibit this caching behavior (no Anthropic-style prompt cache in the local-model path), so there's nothing for it to control there.
