# Eval Harness — Implementation Spec

## Module and package layout

```
github.com/scoutme/milk/eval/         # package eval
github.com/scoutme/milk/eval/cmd/milk-eval/   # package main (CLI entry point)
```

Module: `github.com/scoutme/milk` (existing `go.mod`).
All eval code lives under `eval/` at the repo root. The CLI binary is `eval/cmd/milk-eval/main.go`.

```
eval/
  scenario.go          # Scenario, Turn, RubricCriterion, ScenarioFile
  types.go             # RunResult, TokenUsage, CacheMetrics, ToolCall, JudgeScore
  adapter.go           # AgentAdapter interface, AdapterRegistry
  adapter_milk.go      # milk-tui adapter
  adapter_claude.go    # claude-code adapter
  harness.go           # Orchestrator: RunConfig, RunScenario, RunAll
  judge.go             # LLM-as-judge scorer
  cache.go             # CacheMetrics computation + cache_expect assertions
  report.go            # Report generator (markdown + JSON)
  tmux.go              # tmux helpers (send-keys, poll pane, kill session)
  cmd/milk-eval/
    main.go            # cobra CLI
scenarios/
  _base.yaml           # shared rubric defaults
  code_generation/
    fix-typo.yaml
    add-function.yaml
  debugging/
    null-pointer.yaml
    logic-error.yaml
  refactoring/
    iterative-refactor.yaml
  explanation/
    explain-code.yaml
```

## Type definitions

### scenario.go

```go
package eval

// Scenario is a parsed scenario YAML file.
type Scenario struct {
    Name        string           `yaml:"name"`
    Description string           `yaml:"description"`
    Category    string           `yaml:"category"`
    Difficulty  string           `yaml:"difficulty"` // easy, medium, hard
    MultiTurn   bool             `yaml:"multi_turn"`
    Setup       ScenarioSetup    `yaml:"setup"`
    Prompt      string           `yaml:"prompt"`            // single-turn shorthand
    Turns       []Turn           `yaml:"turns"`             // multi-turn
    Rubric      []RubricCriterion `yaml:"rubric"`           // single-turn rubric (top-level)
    CacheExpect *CacheExpect     `yaml:"cache_expect,omitempty"`
}

type ScenarioSetup struct {
    Files []ScenarioFile `yaml:"files"`
}

type ScenarioFile struct {
    Path    string `yaml:"path"`
    Content string `yaml:"content"`
}

// Turn is one step in a multi-turn scenario.
type Turn struct {
    Prompt string            `yaml:"prompt"`
    Rubric []RubricCriterion `yaml:"rubric"`
}

// RubricCriterion defines one scoring dimension.
type RubricCriterion struct {
    Criterion   string `yaml:"criterion"`
    Description string `yaml:"description"`
    Weight      int    `yaml:"weight"`
    Scoring     string `yaml:"scoring"` // "binary", "scale_1_5"
}

// CacheExpect holds optional automated cache assertions for multi-turn scenarios.
type CacheExpect struct {
    MinCacheHitRateAfterTurn1 float64 `yaml:"min_cache_hit_rate_after_turn1"`
    NoCacheRecreateAfterTurn1 bool    `yaml:"no_cache_recreate_after_turn1"`
}
```

### types.go

```go
package eval

import "time"

// RunResult captures one turn's output from an adapter.
type RunResult struct {
    Response  string        `json:"response"`
    Tokens    TokenUsage    `json:"tokens"`
    CostUSD   float64       `json:"cost_usd"`
    Duration  time.Duration `json:"duration"`
    ToolCalls []ToolCall    `json:"tool_calls,omitempty"`
    Error     error         `json:"error,omitempty"`
}

// TokenUsage captures all token categories for one turn.
type TokenUsage struct {
    InputTokens  int64 `json:"input_tokens"`
    OutputTokens int64 `json:"output_tokens"`
    CacheCreate  int64 `json:"cache_create"`
    CacheRead    int64 `json:"cache_read"`
}

// CacheMetrics derives caching efficiency from TokenUsage.
type CacheMetrics struct {
    CacheHitRate    float64 `json:"cache_hit_rate"`
    CacheCreateRate float64 `json:"cache_create_rate"`
    CachedTokens    int64   `json:"cached_tokens"`
    CacheSavedCost  float64 `json:"cache_saved_cost"`
}

// ToolCall records one tool invocation during a turn.
type ToolCall struct {
    Name   string `json:"name"`
    Args   string `json:"args"`
    Result string `json:"result,omitempty"`
}

// JudgeScore is one scored criterion from the LLM judge.
type JudgeScore struct {
    Criterion string  `json:"criterion"`
    Score     float64 `json:"score"`
    Reasoning string  `json:"reasoning"`
}
```

### adapter.go

```go
package eval

import "context"

// AgentAdapter wraps a terminal-based agent for automated evaluation.
type AgentAdapter interface {
    // Name returns the display name (e.g. "milk-tui", "claude-code").
    Name() string

    // Start launches the agent in a tmux session with the given working directory.
    Start(ctx context.Context, workdir string) error

    // RunPrompt sends a prompt and waits for the agent's complete response.
    // Returns a RunResult with per-turn TokenUsage (snapshot-diff or direct read).
    RunPrompt(ctx context.Context, prompt string) (RunResult, error)

    // Stop kills the tmux session and cleans up.
    Stop() error
}

// AdapterFactory creates a new adapter instance.
type AdapterFactory func() AgentAdapter

// registry is the global adapter registry.
var registry = map[string]AdapterFactory{}

// Register adds an adapter factory under the given name.
// Call from init() in each adapter file.
func Register(name string, factory AdapterFactory) {
    registry[name] = factory
}

// Get returns a new adapter instance by name, or nil if not found.
func Get(name string) AgentAdapter {
    f, ok := registry[name]
    if !ok {
        return nil
    }
    return f()
}

// ListNames returns all registered adapter names.
func ListNames() []string {
    names := make([]string, 0, len(registry))
    for k := range registry {
        names = append(names, k)
    }
    return names
}
```

## Adapter registration pattern

Each adapter file has an `init()` that calls `Register`:

```go
// adapter_milk.go
func init() {
    Register("milk-tui", func() AgentAdapter { return &milkAdapter{} })
}

// adapter_claude.go
func init() {
    Register("claude-code", func() AgentAdapter { return &claudeAdapter{} })
}
```

The CLI entry point imports the adapter packages for side effects:

```go
// cmd/milk-eval/main.go
import (
    _ "github.com/scoutme/milk/eval" // registers adapters via init()
)
```

## Milk TUI adapter — exact implementation

### tmux commands

```
Start:
  tmux new-session -d -s milk-eval -x 200 -y 50
  tmux send-keys -t milk-eval "cd <workdir> && task run" Enter

Stop:
  tmux kill-session -t milk-eval
```

### Session file discovery

milk writes sessions to `~/.milk/sessions/<uuid>.json`. The index at `~/.milk/sessions/index.json` maps `cwd -> [{id, name, last_used}]` sorted by last_used desc.

Discovery flow:
1. After TUI is ready, read `~/.milk/sessions/index.json`
2. Look up the entry for `workdir` (the cwd the adapter passed to `Start`)
3. The first entry (most recent) is the active session
4. Session file path: `~/.milk/sessions/<id>.json`

### TUI-ready detection

Poll the tmux pane output every 500ms for the prompt indicator. The milk TUI shows a prompt label like `milk ❯ ` or `<agent> ❯ `. Detection:

```go
func (a *milkAdapter) waitForReady(ctx context.Context) error {
    // Poll pane content for the prompt marker.
    // The textarea prompt contains "❯" when the TUI is ready for input.
    return pollPaneContains(ctx, a.sessionName, "❯", 500*time.Millisecond)
}
```

### Turn completion detection

Poll the session JSON file every 500ms. A turn is complete when:
1. `state` is `"ROUTING"` (milk returns to routing state after each turn)
2. The last entry in `history` has `role == "assistant"`

```go
func (a *milkAdapter) waitForTurn(ctx context.Context, prevHistoryLen int) (*session.Session, error) {
    for {
        sess, err := session.Load(a.sessionID)
        if err != nil { continue }
        if sess.State == session.StateRouting && len(sess.History) > prevHistoryLen {
            last := sess.History[len(sess.History)-1]
            if last.Role == session.RoleAssistant {
                return sess, nil
            }
        }
        select {
        case <-ctx.Done(): return nil, ctx.Err()
        case <-time.After(500 * time.Millisecond):
        }
    }
}
```

### Token extraction — snapshot-diff

milk's `Session.Tokens` is `map[string]*TokenUsage` keyed by `"model\x00role"`. Each `TokenUsage` holds cumulative counts:

```go
// session.TokenUsage fields:
type TokenUsage struct {
    Model         string `json:"model"`
    Agent         string `json:"agent"`     // "primary" or "escalation"
    Prompt        int64  `json:"prompt"`
    Completion    int64  `json:"completion"`
    CacheRead     int64  `json:"cache_read,omitempty"`
    CacheCreation int64  `json:"cache_creation,omitempty"`
}
```

Per-turn extraction:

```go
func (a *milkAdapter) snapshotTokens(sess *session.Session) TokenUsage {
    var total TokenUsage
    for _, tu := range sess.Tokens {
        total.InputTokens += tu.Prompt
        total.OutputTokens += tu.Completion
        total.CacheRead += tu.CacheRead
        total.CacheCreate += tu.CacheCreation
    }
    return total
}

// In RunPrompt:
before := a.snapshotTokens(sess)
// ... send prompt, wait for turn ...
after := a.snapshotTokens(newSess)
turnTokens := TokenUsage{
    InputTokens:  after.InputTokens - before.InputTokens,
    OutputTokens: after.OutputTokens - before.OutputTokens,
    CacheRead:    after.CacheRead - before.CacheRead,
    CacheCreate:  after.CacheCreate - before.CacheCreate,
}
```

### Response extraction

The last assistant turn's `Content` field:

```go
last := newSess.History[len(newSess.History)-1]
response := last.Content
```

### Full RunPrompt flow

```go
func (a *milkAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
    start := time.Now()

    // 1. Load current session and snapshot history length + tokens
    sess, err := session.Load(a.sessionID)
    if err != nil { return RunResult{}, err }
    prevHistoryLen := len(sess.History)
    beforeTokens := a.snapshotTokens(sess)

    // 2. Send prompt via tmux
    tmuxSendKeys(a.sessionName, prompt)
    tmuxSendEnter(a.sessionName)

    // 3. Wait for turn completion
    newSess, err := a.waitForTurn(ctx, prevHistoryLen)
    if err != nil { return RunResult{}, err }

    // 4. Extract response
    last := newSess.History[len(newSess.History)-1]
    response := last.Content

    // 5. Diff tokens
    afterTokens := a.snapshotTokens(newSess)
    turnTokens := TokenUsage{
        InputTokens:  afterTokens.InputTokens - beforeTokens.InputTokens,
        OutputTokens: afterTokens.OutputTokens - beforeTokens.OutputTokens,
        CacheRead:    afterTokens.CacheRead - beforeTokens.CacheRead,
        CacheCreate:  afterTokens.CacheCreate - beforeTokens.CacheCreate,
    }

    // 6. Extract tool calls from the new turns
    var toolCalls []ToolCall
    for _, t := range newSess.History[prevHistoryLen:] {
        for _, tc := range t.ToolCalls {
            toolCalls = append(toolCalls, ToolCall{Name: tc.Name, Args: tc.Arguments})
        }
    }

    return RunResult{
        Response:  response,
        Tokens:    turnTokens,
        Duration:  time.Since(start),
        ToolCalls: toolCalls,
    }, nil
}
```

## Claude Code adapter — exact implementation

### tmux commands

```
Start:
  tmux new-session -d -s claude-eval -x 200 -y 50
  tmux send-keys -t claude-eval "cd <workdir> && claude --session-id <uuid>" Enter

Stop:
  tmux kill-session -t claude-eval
```

The `--session-id <uuid>` flag makes the transcript path deterministic:
`~/.claude/projects/<encoded-cwd>/<uuid>.jsonl`

The encoded cwd replaces `/` with `-` and strips leading `-`. For example:
`/home/scoutme/altworkspace/milk` -> `-home-scoutme-altworkspace-milk`

### TUI-ready detection

Poll the tmux pane for the `>` input prompt that Claude Code shows when ready:

```go
func (a *claudeAdapter) waitForReady(ctx context.Context) error {
    return pollPaneContains(ctx, a.sessionName, ">", 500*time.Millisecond)
}
```

Note: this is a heuristic. The `>` prompt appears on a line by itself when Claude Code is idle. Refinement: check that the last non-empty line of the pane content ends with `>` followed by a space or is just `>`.

### Turn completion detection — JSONL polling

Claude Code writes one JSON object per line to `~/.claude/projects/<project>/<session-id>.jsonl`.

Completion signal: a new line where:
- `type == "assistant"`
- `message.stop_reason == "end_turn"`

```go
type claudeJSONLLine struct {
    Type      string          `json:"type"`
    Message   claudeMessage   `json:"message"`
    Timestamp string          `json:"timestamp"`
}

type claudeMessage struct {
    Role    string              `json:"role"`
    Content []claudeContentBlock `json:"content"`
    Usage   claudeUsage         `json:"usage"`
    StopReason string           `json:"stop_reason"`
}

type claudeContentBlock struct {
    Type string `json:"type"` // "text", "thinking", "tool_use"
    Text string `json:"text,omitempty"`
    Thinking string `json:"thinking,omitempty"`
    Name  string `json:"name,omitempty"`
    Input json.RawMessage `json:"input,omitempty"`
}

type claudeUsage struct {
    InputTokens              int64 `json:"input_tokens"`
    OutputTokens             int64 `json:"output_tokens"`
    CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
    CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}
```

Polling logic:

```go
func (a *claudeAdapter) waitForTurn(ctx context.Context, prevSize int64) (claudeJSONLLine, error) {
    for {
        f, err := os.Open(a.transcriptPath)
        if err != nil { /* retry */ }
        // Seek to prevSize, scan new lines
        f.Seek(prevSize, io.SeekStart)
        scanner := bufio.NewScanner(f)
        for scanner.Scan() {
            var line claudeJSONLLine
            if json.Unmarshal(scanner.Bytes(), &line) != nil { continue }
            if line.Type == "assistant" && line.Message.StopReason == "end_turn" {
                f.Close()
                return line, nil
            }
        }
        f.Close()
        select {
        case <-ctx.Done(): return claudeJSONLLine{}, ctx.Err()
        case <-time.After(500 * time.Millisecond):
        }
    }
}
```

### Token extraction — direct read

Claude Code's JSONL stores per-turn usage in each assistant message. The `end_turn` message has the cumulative usage for that turn's API call. Extract directly:

```go
tokens := TokenUsage{
    InputTokens:  line.Message.Usage.InputTokens,
    OutputTokens: line.Message.Usage.OutputTokens,
    CacheCreate:  line.Message.Usage.CacheCreationInputTokens,
    CacheRead:    line.Message.Usage.CacheReadInputTokens,
}
```

Note: when Claude makes multiple tool calls in a single turn, there are multiple `assistant` messages with `stop_reason: "tool_use"`. The final `end_turn` message contains only the last API call's tokens. To get the full turn's token usage, sum all assistant messages since the user message:

```go
func (a *claudeAdapter) sumTurnTokens(lines []claudeJSONLLine) TokenUsage {
    var total TokenUsage
    for _, l := range lines {
        if l.Type == "assistant" {
            total.InputTokens += l.Message.Usage.InputTokens
            total.OutputTokens += l.Message.Usage.OutputTokens
            total.CacheCreate += l.Message.Usage.CacheCreationInputTokens
            total.CacheRead += l.Message.Usage.CacheReadInputTokens
        }
    }
    return total
}
```

### Response extraction

Collect all `text` content blocks from assistant messages in the turn, concatenate:

```go
func extractResponse(lines []claudeJSONLLine) string {
    var sb strings.Builder
    for _, l := range lines {
        if l.Type != "assistant" { continue }
        for _, block := range l.Message.Content {
            if block.Type == "text" {
                sb.WriteString(block.Text)
            }
        }
    }
    return sb.String()
}
```

### Tool call extraction

```go
func extractToolCalls(lines []claudeJSONLLine) []ToolCall {
    var calls []ToolCall
    for _, l := range lines {
        if l.Type != "assistant" { continue }
        for _, block := range l.Message.Content {
            if block.Type == "tool_use" {
                calls = append(calls, ToolCall{
                    Name: block.Name,
                    Args: string(block.Input),
                })
            }
        }
    }
    return calls
}
```

### Full RunPrompt flow

```go
func (a *claudeAdapter) RunPrompt(ctx context.Context, prompt string) (RunResult, error) {
    start := time.Now()

    // 1. Snapshot current transcript file size
    prevSize := fileSize(a.transcriptPath)

    // 2. Send prompt via tmux
    tmuxSendKeys(a.sessionName, prompt)
    tmuxSendEnter(a.sessionName)

    // 3. Wait for end_turn, collecting all new lines
    var turnLines []claudeJSONLLine
    // ... poll from prevSize, collect all assistant lines until end_turn ...

    // 4. Extract tokens (sum all assistant messages in the turn)
    tokens := a.sumTurnTokens(turnLines)

    // 5. Extract response text
    response := extractResponse(turnLines)

    // 6. Extract tool calls
    toolCalls := extractToolCalls(turnLines)

    return RunResult{
        Response:  response,
        Tokens:    tokens,
        Duration:  time.Since(start),
        ToolCalls: toolCalls,
    }, nil
}
```

## CacheMetrics formulas

```go
func ComputeCacheMetrics(tokens TokenUsage) CacheMetrics {
    total := float64(tokens.CacheRead + tokens.CacheCreate + tokens.InputTokens)
    if total == 0 {
        return CacheMetrics{}
    }
    return CacheMetrics{
        CacheHitRate:    float64(tokens.CacheRead) / total,
        CacheCreateRate: float64(tokens.CacheCreate) / total,
        CachedTokens:    tokens.CacheRead + tokens.CacheCreate,
    }
}

// AggregateCacheMetrics computes metrics across multiple turns.
func AggregateCacheMetrics(results []RunResult) CacheMetrics {
    var total TokenUsage
    for _, r := range results {
        total.InputTokens += r.Tokens.InputTokens
        total.OutputTokens += r.Tokens.OutputTokens
        total.CacheRead += r.Tokens.CacheRead
        total.CacheCreate += r.Tokens.CacheCreate
    }
    return ComputeCacheMetrics(total)
}

// CacheSavedCost estimates the cost saving from cache reads.
// Uses Anthropic pricing: cache reads are 10% of base input price,
// cache writes are 125% of base input price.
// Base input price varies by model; caller provides pricePerMToken.
func CacheSavedCost(tokens TokenUsage, pricePerMToken float64) float64 {
    // If these cache-read tokens were not cached, they'd cost full price.
    // They actually cost 0.1 * full price.
    // Savings = cacheRead * pricePerMToken/1M * (1.0 - 0.1)
    return float64(tokens.CacheRead) * pricePerMToken / 1_000_000 * 0.9
}
```

## Multi-turn flow

The harness iterates turns within a single adapter lifecycle:

```go
func RunScenario(ctx context.Context, adapter AgentAdapter, scenario Scenario) ([]RunResult, []JudgeScore, error) {
    // 1. Create temp workdir, write setup files
    workdir := setupWorkdir(scenario.Setup.Files)
    defer os.RemoveAll(workdir)

    // 2. Start adapter
    if err := adapter.Start(ctx, workdir); err != nil {
        return nil, nil, err
    }
    defer adapter.Stop()

    // 3. Determine turns
    turns := scenario.Turns
    if len(turns) == 0 && scenario.Prompt != "" {
        turns = []Turn{{Prompt: scenario.Prompt, Rubric: scenario.Rubric}}
    }

    // 4. Run each turn (same tmux session, adapter stays alive)
    var results []RunResult
    for _, turn := range turns {
        result, err := adapter.RunPrompt(ctx, turn.Prompt)
        if err != nil {
            result.Error = err
        }
        results = append(results, result)
    }

    // 5. Score each turn
    var allScores []JudgeScore
    for i, turn := range turns {
        scores, err := judge.Score(ctx, scenario, turn, results[i])
        if err != nil { /* log and continue */ }
        allScores = append(allScores, scores...)
    }

    return results, allScores, nil
}
```

## Judge prompt template

```go
const judgePromptTemplate = `You are evaluating an AI agent's response to a task.
Score each criterion precisely. Output ONLY a JSON array.

## Task
{{.Description}}

## Prompt
{{.Prompt}}

## Agent Response
{{.Response}}

## Scoring Criteria
{{range .Criteria}}- **{{.Criterion}}** ({{.Scoring}}, weight {{.Weight}}): {{.Description}}
{{end}}

Output a JSON array of objects with fields: criterion, score, reasoning.
For "binary" scoring: score is 0 or 1.
For "scale_1_5" scoring: score is an integer 1-5.

Output ONLY the JSON array, no other text.`
```

The judge uses the configured escalation agent (or a dedicated judge model) via the local inference API. It receives the scenario description, the prompt, and the response text — never tool call details or token data.

```go
type judgeInput struct {
    Description string
    Prompt      string
    Response    string
    Criteria    []RubricCriterion
}
```

## Scenario YAML format

### Single-turn

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

### Multi-turn

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

cache_expect:
  min_cache_hit_rate_after_turn1: 0.3
  no_cache_recreate_after_turn1: true
```

## CLI command structure

```
milk-eval run
  --scenarios <dir>       # directory of scenario YAML files (default: eval/scenarios/)
  --scenario <file>       # single scenario file
  --agents <list>         # comma-separated adapter names (default: all registered)
  --category <name>       # filter by category
  --multi-turn            # only multi-turn scenarios
  --timeout <duration>    # per-turn timeout (default: 5m)
  --results <dir>         # output directory for results (default: eval/results/)
  --judge-model <name>    # model for LLM-as-judge (default: escalation agent)
  --verbose               # show live adapter output

milk-eval judge
  --results <dir>         # results directory from a prior run
  --scenarios <dir>       # scenario files (for rubric lookup)
  --judge-model <name>    # model for re-scoring

milk-eval report
  --results <dir>         # results directory
  --output <file>         # output file (default: stdout)
  --format <fmt>          # "markdown" or "json" (default: markdown)
  --cache-only            # skip quality scoring, show only cache analysis
```

## Tmux helpers

```go
package eval

import (
    "context"
    "os/exec"
    "strings"
    "time"
)

func tmuxNewSession(name string, width, height int) error {
    return exec.Command("tmux", "new-session", "-d", "-s", name,
        "-x", fmt.Sprint(width), "-y", fmt.Sprint(height)).Run()
}

func tmuxKillSession(name string) error {
    return exec.Command("tmux", "kill-session", "-t", name).Run()
}

func tmuxSendKeys(session, text string) error {
    // Use tmux send-keys with literal flag to avoid special char interpretation
    return exec.Command("tmux", "send-keys", "-t", session, "-l", text).Run()
}

func tmuxSendEnter(session string) error {
    return exec.Command("tmux", "send-keys", "-t", session, "Enter").Run()
}

func tmuxCapturePane(session string) (string, error) {
    out, err := exec.Command("tmux", "capture-pane", "-t", session, "-p").Output()
    return string(out), err
}

// pollPaneContains polls the tmux pane until it contains the given substring.
func pollPaneContains(ctx context.Context, session, substr string, interval time.Duration) error {
    for {
        content, err := tmuxCapturePane(session)
        if err == nil && strings.Contains(content, substr) {
            return nil
        }
        select {
        case <-ctx.Done():
            return ctx.Err()
        case <-time.After(interval):
        }
    }
}
```

## Result persistence

Each run produces a JSON file per scenario per agent:

```
eval/results/<timestamp>/
  run.json                          # run metadata (timestamp, agents, scenarios)
  fix-typo-in-readme/
    milk-tui.json                   # RunResult[] + JudgeScore[]
    claude-code.json
  iterative-refactoring/
    milk-tui.json
    claude-code.json
```

Each per-agent JSON file:

```json
{
  "scenario": "fix-typo-in-readme",
  "agent": "milk-tui",
  "turns": [
    {
      "response": "...",
      "tokens": {"input_tokens": 1200, "output_tokens": 340, "cache_create": 0, "cache_read": 0},
      "cost_usd": 0.001,
      "duration_ms": 2100,
      "tool_calls": [{"name": "read_file", "args": "..."}, {"name": "edit_file", "args": "..."}]
    }
  ],
  "scores": [
    {"criterion": "correctness", "score": 1.0, "reasoning": "..."},
    {"criterion": "efficiency", "score": 4.0, "reasoning": "..."}
  ],
  "cache_metrics": {
    "cache_hit_rate": 0.0,
    "cache_create_rate": 0.0,
    "cached_tokens": 0,
    "cache_saved_cost": 0.0
  }
}
```

## Dependencies

```go
// go.mod additions (most already present):
require (
    github.com/spf13/cobra     // CLI framework
    gopkg.in/yaml.v3            // YAML parsing
    // existing: encoding/json, os/exec, text/template, time
)
```

## Key field name reference

### milk session JSON (`~/.milk/sessions/<uuid>.json`)

```
Session.ID              string
Session.CWD             string
Session.State           string    // "ROUTING", "LOCAL", "ESCALATION", "ESCALATION_WAITING"
Session.History         []Turn
  Turn.Role             string    // "user", "assistant", "tool_result"
  Turn.Agent            string    // "primary", "escalation"
  Turn.Content          string
  Turn.Thinking         string
  Turn.ToolCalls        []ToolCall
    ToolCall.ID         string
    ToolCall.Name       string
    ToolCall.Arguments  string
  Turn.Timestamp        time.Time
Session.Tokens          map[string]*TokenUsage   // key: "model\x00role"
  TokenUsage.Model      string
  TokenUsage.Agent      string    // "primary" or "escalation"
  TokenUsage.Prompt     int64     // cumulative
  TokenUsage.Completion int64     // cumulative
  TokenUsage.CacheRead  int64     // cumulative
  TokenUsage.CacheCreation int64  // cumulative
```

### Claude Code JSONL (`~/.claude/projects/<project>/<session-id>.jsonl`)

Each line is a JSON object. Key types:

```
type: "user"
  message.role: "user"
  message.content: [{type: "text", text: "..."}]

type: "assistant"
  message.role: "assistant"
  message.model: "claude-sonnet-4-6"
  message.content: [
    {type: "thinking", thinking: "..."},
    {type: "text", text: "..."},
    {type: "tool_use", id: "...", name: "Bash", input: {...}}
  ]
  message.stop_reason: "tool_use" | "end_turn"
  message.usage.input_tokens: int64
  message.usage.output_tokens: int64
  message.usage.cache_creation_input_tokens: int64
  message.usage.cache_read_input_tokens: int64

type: "tool_result"
  tool_use_id: "..."
  content: "..."
```

### Claude stream-json (milk's `--output-format stream-json`)

Not used by the eval harness (adapters run agents interactively via tmux). Documented for reference — the event types are: `system`, `assistant`, `content_block_start`, `content_block_delta`, `content_block_stop`, `result`. Token fields are in the `result` event: `usage.input_tokens`, `usage.output_tokens`, `usage.cache_creation_input_tokens`, `usage.cache_read_input_tokens`.

## Implementation phases

### Phase 1: Types and registry
- `eval/types.go` — RunResult, TokenUsage, CacheMetrics, ToolCall, JudgeScore
- `eval/scenario.go` — Scenario, Turn, RubricCriterion, ScenarioFile, CacheExpect
- `eval/adapter.go` — AgentAdapter interface, Register/Get/ListNames

### Phase 2: Tmux helpers
- `eval/tmux.go` — tmuxNewSession, tmuxKillSession, tmuxSendKeys, tmuxSendEnter, tmuxCapturePane, pollPaneContains

### Phase 3: Milk adapter
- `eval/adapter_milk.go` — milkAdapter struct, Start/RunPrompt/Stop, session file discovery, snapshot-diff token extraction

### Phase 4: Claude Code adapter
- `eval/adapter_claude.go` — claudeAdapter struct, Start/RunPrompt/Stop, JSONL polling, direct-read token extraction

### Phase 5: Harness orchestrator
- `eval/harness.go` — RunConfig, RunScenario, RunAll, workdir setup/teardown

### Phase 6: Judge
- `eval/judge.go` — judge prompt template, Score function, JSON parsing

### Phase 7: Cache analysis
- `eval/cache.go` — ComputeCacheMetrics, AggregateCacheMetrics, CacheSavedCost, cache_expect assertions

### Phase 8: Report
- `eval/report.go` — per-scenario table, multi-turn cache table, aggregate report, markdown + JSON output

### Phase 9: CLI
- `eval/cmd/milk-eval/main.go` — cobra commands: run, judge, report

### Phase 10: Scenarios
- Write 10-15 scenarios across code_generation, debugging, refactoring, explanation categories
