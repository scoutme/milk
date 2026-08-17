# Agent Evaluation Harness — Design & Plan

> **Historical planning doc.** Written before the harness was built; describes the original standalone `milk-eval` binary, later merged into `milk eval` as a subcommand of the main binary. For current usage — commands, scenario format, adapters and their per-adapter options, judging, reports — see [docs/eval.md](eval.md). Kept as a record of the original design, not maintained in sync with the implementation.

## Problem

No way to systematically compare milk TUI vs Claude Code (or any two agent configurations) on the same tasks. Need to measure quality, token usage, and latency side-by-side.

## Research Summary

### Key evaluation frameworks (industry standard)

| Framework | Approach | Key metrics |
|-----------|----------|-------------|
| **SWE-bench** | Real GitHub issues, binary pass/fail | Task completion rate |
| **AgentBench** | Multi-environment agent tasks | Success rate + efficiency |
| **GAIA** | Real-world assistant tasks (3 levels) | Accuracy, steps taken |
| **MT-Bench / Chatbot Arena** | Multi-turn conversation quality | LLM-as-judge score (1-10) |
| **OpenAI Evals** | Structured scenario runner | Custom rubrics, pass/fail |
| **LangSmith** | Tracing + automated eval | Rubric-based scoring, cost tracking |
| **DeepEval** | Open-source LLM eval | G-Eval, faithfulness, relevancy |
| **RAGAS** | RAG-focused | Faithfulness, answer relevancy, context precision |
| **τ-bench** | Tool-use scenarios | Task completion + tool call accuracy |

### Common patterns across frameworks

1. **Scenario as data**: tasks defined in YAML/JSON with prompt, expected behavior, scoring rubric
2. **LLM-as-judge**: use a strong model to score outputs against rubrics (MT-Bench pattern)
3. **Multi-dimensional scoring**: correctness, completeness, efficiency, cost
4. **Same-task comparison**: identical prompts to N agents, side-by-side results
5. **Token/cost tracking**: input + output + cache tokens, USD cost, latency

## Design

### Core principle: adapter-based architecture

The harness has a single standard flow. Each agent is an adapter implementing a common interface. Adding a new agent = writing a new adapter. The harness never changes.

**Uniformity boundary:** the adapter interface guarantees that all agents produce
the same output format (`RunResult` with `TokenUsage`, `CacheMetrics`). The
extraction logic is adapter-specific because each agent stores data differently.
The harness, judge, cache analyzer, and reporter operate only on the uniform
output — they never touch agent-specific files or formats.

```
┌─────────────────────────────────────────────────────┐
│                    Harness                           │
│                                                     │
│  for each scenario:                                 │
│    for each agent adapter:                          │
│      adapter.Start(workdir)                         │
│      for each turn in scenario:                     │
│        result = adapter.RunPrompt(turn.prompt)      │
│        results.append(result)                       │
│      scores = judge.Score(scenario, results)        │
│      cache = cacheAnalysis(results)                 │
│      adapter.Stop()                                 │
│                                                     │
│  report.Compare(all results, all scores, all cache) │
└─────────────────────────────────────────────────────┘
```

For single-turn scenarios: `turns` has one entry (or uses the legacy `prompt` field).
For multi-turn scenarios: `turns` has N entries, each with its own prompt and rubric.
The same tmux session is used for all turns — the adapter stays alive between prompts.

### Adapter interface

```go
// AgentAdapter wraps a terminal-based agent for automated evaluation.
// Each adapter manages its own tmux session and knows how to detect
// turn completion by polling side-effect files (read-only, no impact
// on the tested process).
//
// Token extraction is adapter-specific because each agent stores tokens
// differently (milk: cumulative per session; Claude Code: per-turn in JSONL).
// The adapter is responsible for producing a uniform RunResult with TokenUsage.
type AgentAdapter interface {
    // Name returns the display name for reports (e.g. "milk-tui", "claude-code").
    Name() string

    // Start launches the agent in a tmux session with the given working directory.
    // The workdir is pre-populated with scenario files by the harness.
    Start(ctx context.Context, workdir string) error

    // RunPrompt sends a prompt and waits for the agent's complete response.
    // Internally: sends input via tmux send-keys, polls the agent's state
    // file for turn completion, extracts response + tokens from the file.
    //
    // Token extraction strategy:
    //   - Agents that store per-turn tokens (Claude Code JSONL): read directly.
    //   - Agents that store cumulative tokens (milk session JSON): snapshot
    //     the totals before the turn, run the turn, snapshot after, diff.
    //
    // Either way, the returned RunResult.Tokens contains THIS turn's counts.
    RunPrompt(ctx context.Context, prompt string) (RunResult, error)

    // Stop kills the tmux session and cleans up.
    Stop() error
}
```

### RunResult (common output from all adapters)

```go
// RunResult captures one turn's output. For multi-turn scenarios,
// the harness collects []RunResult (one per turn).
type RunResult struct {
    Response     string        // agent's final text output
    Tokens       TokenUsage    // input, output, cache_create, cache_read
    CostUSD      float64       // if available from the agent
    Duration     time.Duration // wall-clock time for the turn
    ToolCalls    []ToolCall    // tool invocations during the turn
    Error        error         // non-nil if the turn failed
}

// TokenUsage captures all token categories. Cache metrics are critical
// for measuring prompt caching efficiency across turns.
type TokenUsage struct {
    InputTokens  int64   // non-cached input tokens
    OutputTokens int64   // generated tokens
    CacheCreate  int64   // tokens written to prompt cache this turn
    CacheRead    int64   // tokens read from prompt cache this turn
}

// CacheMetrics derives caching efficiency from TokenUsage.
// Computed per-turn and aggregated across the scenario.
type CacheMetrics struct {
    CacheHitRate    float64 // CacheRead / (CacheRead + CacheCreate + InputTokens)
    CacheCreateRate float64 // CacheCreate / (CacheRead + CacheCreate + InputTokens)
    CachedTokens    int64   // CacheRead + CacheCreate (total cache-touching tokens)
    CacheSavedCost  float64 // estimated cost saving from cache reads vs uncached
}

type ToolCall struct {
    Name   string
    Args   string
    Result string
}
```

### How each adapter detects turn completion

All adapters use the same pattern: **read-only polling of a side-effect file**.

#### milk-tui adapter

```
File:     ~/.milk/sessions/<id>.json
Signal:   state == "ROUTING" AND last history entry role == "assistant"
Tokens:   turn.TokensFull (from session JSON)
Response: turn.Content of the last assistant turn
```

Flow:
1. `Start`: `tmux new-session -d -s milk-eval`, `tmux send-keys "task run" Enter`
2. Wait for TUI ready: poll pane for input prompt indicator
3. `RunPrompt`:
   a. Snapshot `len(History)` AND cumulative token totals from session file
   b. `tmux send-keys "<prompt>" Enter`
   c. Poll session file every 500ms: new assistant turn + state=ROUTING → done
   d. Snapshot cumulative token totals again
   e. Diff: this turn's tokens = after_totals − before_totals
   f. Extract response text from the new assistant turn's Content
4. `Stop`: `tmux kill-session -t milk-eval`

Token strategy: **snapshot-diff**. Milk's session stores cumulative `TokensFull`
per (model, role) pair. Per-turn counts are derived by diffing snapshots.

Session file discovery: `ls -lt ~/.milk/sessions/<uuid>.json` (newest file, or
read `~/.milk/sessions/index.json` for the cwd → session mapping).

#### claude-code adapter

```
File:     ~/.claude/projects/<project>/<session-id>.jsonl
Signal:   new line with type=="assistant" AND stop_reason=="end_turn"
Tokens:   message.usage (input_tokens, output_tokens, cache_*)
Response: message.content[].text
```

Flow:
1. `Start`: `tmux new-session -d -s claude-eval`, `tmux send-keys "claude" Enter`
2. Wait for Claude Code ready: poll pane for `>` prompt
3. `RunPrompt`:
   a. Snapshot current file size of the transcript JSONL
   b. `tmux send-keys "<prompt>" Enter`
   c. Tail the JSONL file every 500ms, parse new lines
   d. Find new `type=="assistant"` line with `stop_reason=="end_turn"` → done
   e. Extract response text from `message.content[].text`
   f. Extract per-turn tokens directly from `message.usage`
4. `Stop`: `tmux kill-session -t claude-eval`

Token strategy: **direct read**. Claude Code's JSONL stores per-turn usage in
each assistant message's `usage` field. No snapshot-diff needed.

Session file discovery: launch Claude Code with `--session-id <known-uuid>` so
the transcript path is deterministic, OR detect by watching for new `.jsonl`
files in the project directory after launch.

#### Adding a new adapter

To add e.g. an `aider-tui` or `cursor` adapter:

1. Create `eval/adapter_aider.go`
2. Implement `AgentAdapter` interface
3. Know how to: launch in tmux, send input, detect completion, extract result
4. Register in the adapter registry (`eval/adapters.go`)

The harness, judge, reporter, and scenario format are unchanged.

### Scenario format

Scenarios can be single-turn or multi-turn. Multi-turn scenarios test
prompt caching efficiency across a conversation.

**Single-turn scenario:**

```yaml
name: "fix-typo-in-readme"
description: "Agent should find and fix a typo in a README file"
category: code_generation
difficulty: easy

setup:
  files:
    - path: "README.md"
      content: |
        # My Project
        This is a projcet with a typo.

prompt: "There's a typo in the README. Find and fix it."

rubric:
  - criterion: "correctness"
    description: "Did the agent fix the actual typo?"
    weight: 3
    scoring: "binary"
  - criterion: "efficiency"
    description: "Did the agent use minimal tool calls?"
    weight: 1
    scoring: "scale_1_5"
```

**Multi-turn scenario (tests caching across turns):**

```yaml
name: "iterative-refactoring"
description: "Agent refactors code across multiple follow-up requests"
category: refactoring
difficulty: medium
multi_turn: true

setup:
  files:
    - path: "auth.go"
      content: |
        package auth
        func Login(u, p string) bool { return u == "admin" && p == "1234" }
        func Logout(s string) bool { delete(sessions, s); return true }

turns:
  - prompt: "Refactor auth.go to use a struct-based approach."
    rubric:
      - criterion: "correctness"
        weight: 3
        scoring: "binary"
  - prompt: "Now add input validation for empty username/password."
    rubric:
      - criterion: "correctness"
        weight: 2
        scoring: "binary"
  - prompt: "Add a hash function for the password instead of storing plaintext."
    rubric:
      - criterion: "correctness"
        weight: 3
        scoring: "binary"

# Per-turn cache expectations (optional, for automated cache assertions)
cache_expect:
  # Turn 2+ should have cache reads (context from turn 1 is cached)
  min_cache_hit_rate_after_turn1: 0.3
  # Cache creation should happen on turn 1, not re-create on turn 2
  no_cache_recreate_after_turn1: true
```

### Judge (LLM-as-judge)

Uses the configured escalation agent (or a dedicated judge model) to score
outputs against the scenario rubric. The judge receives the scenario description,
the prompt, and the agent's response — never the tool call details or token data
(to avoid bias toward verbose agents).

```go
type JudgeScore struct {
    Criterion string  `json:"criterion"`
    Score     float64 `json:"score"`
    Reasoning string  `json:"reasoning"`
}
```

Judge prompt:
```
You are evaluating an AI agent's response to a task.

## Task
{scenario.description}

## Prompt
{scenario.prompt}

## Agent Response
{run_result.response}

## Scoring Criteria
{scenario.rubric as YAML}

Score each criterion. Output a JSON array of objects with fields:
criterion, score, reasoning.
```

Scoring types:
- `binary`: 0 or 1 (did it do the thing?)
- `scale_1_5`: integer 1-5 (how well did it do the thing?)
- Custom: any numeric scale defined in the scenario

### Report format

**Single-turn scenario report:**

```
=== Agent Evaluation Report ===

Scenario: fix-typo-in-readme
Category: code_generation | Difficulty: easy

                    milk-tui        claude-code
─────────────────────────────────────────────────
Correctness         1.0             1.0
Efficiency          4.0             3.0
Explanation         5.0             4.0
─────────────────────────────────────────────────
Weighted Score      4.2             3.6

Tokens (in/out)     1200/340        25477/18
Cache (create/read) 0/0             25477/0
Cache hit rate      0.0%            0.0%
Cost USD            $0.001          $0.096
Duration            2.1s            2.5s
Tool calls          2               1

Winner: milk-tui (+0.6 score, -98.9% cost)
```

**Multi-turn scenario report (caching focus):**

```
=== Cache Efficiency Report ===

Scenario: iterative-refactoring (3 turns)

Per-turn breakdown:
                milk-tui                          claude-code
Turn  InTok  OutTok  CacheR  CacheW  HitRate     InTok  OutTok  CacheR  CacheW  HitRate
────  ─────  ──────  ──────  ──────  ────────    ─────  ──────  ──────  ──────  ────────
  1    1200     340       0    1100   0.0%        25477      18       0   25477   0.0%
  2    1300     280    1100       0   84.6%       26000      22   25477       0   98.0%
  3    1400     310    1300       0   92.9%       26500      25   26000       0   98.1%

Aggregate:
                    milk-tui        claude-code
─────────────────────────────────────────────────
Total tokens        5,030           103,519
Cache hit rate      59.2%           65.4%
Cache create        1,100           25,477
Cache read          2,400           51,477
Total cost          $0.003          $0.291
Cost w/o cache      $0.005          $0.437
Cache savings       40.0%           33.4%

Winner: milk-tui (-99.0% cost, similar cache hit rate)
```

**Aggregate report across all scenarios:**

```
=== Aggregate Evaluation Report ===

Scenarios: 15 | Categories: 4 | Agents: 2

By weighted score (quality):
                    milk-tui        claude-code
─────────────────────────────────────────────────
code_generation     4.1             4.4
debugging           3.8             4.2
refactoring         4.0             3.9
explanation         4.5             4.3
─────────────────────────────────────────────────
Overall             4.1             4.2

By cost-efficiency (quality per dollar):
                    milk-tui        claude-code
─────────────────────────────────────────────────
Avg cost/scenario   $0.002          $0.150
Quality/dollar      2,050           28.0
Cache hit rate      62.3%           71.1%
Cache savings       38.5%           35.2%

Overall winner: milk-tui (comparable quality, 75x cheaper)
```

Aggregate report: per-category breakdowns, mean/median/p95 per metric,
overall winner by weighted score and by cost-efficiency.
For multi-turn scenarios: cache hit rate progression, cache creation
stability (no re-creates), estimated cost savings from caching.

### CLI interface

```bash
# Run all scenarios against all registered adapters
milk-eval run --scenarios eval/scenarios/

# Run specific category
milk-eval run --scenarios eval/scenarios/ --category code_generation

# Run specific scenario against specific agents
milk-eval run --scenario eval/scenarios/fix-typo.yaml --agents milk-tui,claude-code

# Run only multi-turn scenarios (cache efficiency focus)
milk-eval run --scenarios eval/scenarios/ --multi-turn

# Re-score existing results (skip running, just re-judge)
milk-eval judge --results eval/results/ --scenarios eval/scenarios/

# Generate report from results (includes cache analysis)
milk-eval report --results eval/results/ --output eval/report.md

# Generate cache-only report (skip quality scoring)
milk-eval report --results eval/results/ --cache-only --output eval/cache-report.md
```

## Architecture diagram

```
eval/
├── adapter.go              # AgentAdapter interface + registry
├── adapter_milk.go         # milk-tui adapter (tmux + session file poll)
├── adapter_claude.go       # claude-code adapter (tmux + jsonl poll)
├── harness.go              # Scenario loader + orchestrator
├── judge.go                # LLM-as-judge scorer
├── cache.go                # CacheMetrics computation + assertions
├── report.go               # Comparison report generator (incl. cache section)
├── scenario.go             # Scenario + RunResult + CacheMetrics types
├── cmd/
│   └── milk-eval/
│       └── main.go         # CLI entry point (cobra)
└── scenarios/
    ├── _base.yaml          # Shared rubric defaults
    ├── code_generation.yaml
    ├── debugging.yaml
    ├── refactoring.yaml
    └── explanation.yaml
```

## Implementation plan

### Phase 1: Types & interfaces

1. `scenario.go` — `Scenario`, `RunResult`, `TokenUsage`, `ToolCall`, `RubricCriterion` types
2. `adapter.go` — `AgentAdapter` interface, adapter registry (`Register(name, factory)`, `Get(name)`)
3. `harness.go` — `RunConfig`, `RunScenario`, `RunAll` orchestrator skeleton

### Phase 2: Adapters (tmux + file polling)

4. `adapter_milk.go` — milk TUI adapter
   - tmux session lifecycle
   - session file discovery (`~/.milk/sessions/index.json`)
   - TUI-ready detection (poll pane for prompt)
   - turn completion poll (state=ROUTING + new assistant turn)
   - token extraction from session JSON

5. `adapter_claude.go` — Claude Code TUI adapter
   - tmux session lifecycle
   - `--session-id` flag for deterministic transcript path
   - Claude Code ready detection (poll pane for `>` prompt)
   - turn completion poll (tail JSONL, find `stop_reason=="end_turn"`)
   - token extraction from `message.usage`

### Phase 3: Judge & scoring

6. `judge.go` — judge prompt template with rubric injection
7. JSON-structured output parsing for `JudgeScore`
8. Binary + scale_1_5 scoring type support
9. Weighted score calculation

### Phase 4: Cache analysis & reporting

10. `cache.go` — `CacheMetrics` computation from `[]RunResult`:
    - Per-turn: hit rate, create rate, cached tokens
    - Scenario-level: aggregate hit rate, total cached tokens
    - Cost savings estimation: `CacheRead * (uncached_price - cached_price)`
    - Cache stability check: no re-creation after turn 1 (static prefix preserved)
    - Assertions from `cache_expect` in scenario YAML

11. `report.go` — per-scenario comparison table (single-turn + multi-turn variants)
12. Aggregate statistics (mean, median, p95 per metric)
13. Category-level breakdown
14. Cache efficiency section (hit rate progression, savings, stability)
15. Markdown + JSON output formats

### Phase 5: CLI & scenarios

14. `cmd/milk-eval/main.go` — cobra CLI (run, judge, report commands)
15. YAML scenario loader with file setup/teardown
16. Write 10-15 scenarios across categories
17. Validate scoring consistency (same scenario 3x, check variance)
18. Tune judge prompt for reliable scoring

### Phase 6: Workflow orchestration

19. Create the agentic workflow script:
   - **Designer/Planner** agent: reviews plan, refines scenario rubrics, defines scoring policy
   - **Developer** agent: implements adapter, harness, judge, report, CLI
   - **Validator** agent: runs the eval harness on a small scenario set, verifies results

## Dependencies

- `gopkg.in/yaml.v3` — YAML parsing
- `encoding/json` — session file + JSONL parsing
- `os/exec` — tmux subprocess management
- `text/template` — judge prompt rendering
- `github.com/spf13/cobra` — CLI framework (already used by milk)
- Existing milk packages: `internal/config`, `internal/session` (for session file reading)
