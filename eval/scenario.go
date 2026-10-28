package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Scenario is a parsed scenario YAML file.
type Scenario struct {
	Name        string            `yaml:"name"`
	Description string            `yaml:"description"`
	Category    string            `yaml:"category"`
	Difficulty  string            `yaml:"difficulty"` // easy, medium, hard
	MultiTurn   bool              `yaml:"multi_turn"`
	Setup       ScenarioSetup     `yaml:"setup"`
	Prompt      string            `yaml:"prompt"` // single-turn shorthand
	Turns       []Turn            `yaml:"turns"`  // multi-turn
	Rubric      []RubricCriterion `yaml:"rubric"` // single-turn rubric (top-level)
	Expected    *Expected         `yaml:"expected,omitempty"`
	CacheExpect *CacheExpect      `yaml:"cache_expect,omitempty"`
	// Budget is this scenario's metric thresholds, merged over the
	// experiment-level budget (this scenario wins field-by-field) and checked
	// against every run — see Budget.
	Budget *Budget `yaml:"budget,omitempty"`
}

// ScenarioSetup holds files to create in the working directory before the scenario runs.
type ScenarioSetup struct {
	Files []FileDef `yaml:"files"`
}

// FileDef describes a file to create in the scenario's working directory.
type FileDef struct {
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

// Expected holds optional automated assertions about the agent's response.
type Expected struct {
	Contains     []string     `yaml:"contains"`
	FilesChanged []FileExpect `yaml:"files_changed"`
}

// FileExpect asserts that a file was modified and its content matches a substring.
type FileExpect struct {
	Path     string `yaml:"path"`
	Contains string `yaml:"contains"`
}

// CacheExpect holds optional automated cache assertions for multi-turn scenarios.
type CacheExpect struct {
	MinCacheHitRateAfterTurn1 float64 `yaml:"min_cache_hit_rate_after_turn1"`
	NoCacheRecreateAfterTurn1 bool    `yaml:"no_cache_recreate_after_turn1"`
}

// RunResult captures one turn's output from an adapter.
type RunResult struct {
	Response    string        `json:"response"`
	Tokens      TokenUsage    `json:"tokens"`
	CostUSD     float64       `json:"cost_usd"`
	Duration    time.Duration `json:"duration"`
	ToolCalls   []ToolCall    `json:"tool_calls,omitempty"`
	FileChanges []FileChange  `json:"file_changes,omitempty"`
	// Model is the model that answered the turn (milk-tui: system/init's
	// primary-agent model; claude-code: the transcript's message.model).
	Model string `json:"model,omitempty"`
	// NumTurns is the turn's model iterations (1 + tool-calling iterations).
	// Zero when the adapter has no such signal.
	NumTurns int64 `json:"num_turns,omitempty"`
	// StopReason is the terminal result's stop_reason (e.g. "end_turn").
	StopReason string `json:"stop_reason,omitempty"`
	// RouteHistory lists the routing targets that served the turn
	// ("primary" / "escalation"), one entry per hop.
	RouteHistory []string `json:"route_history,omitempty"`
	// TTFTMS is the mean time-to-first-token across the turn's measured
	// completion requests; OutputTokensPerSecond is the generation throughput.
	// Both are 0 when no timing source reported (never fabricated).
	TTFTMS                int64   `json:"ttft_ms,omitempty"`
	OutputTokensPerSecond float64 `json:"output_tokens_per_second,omitempty"`
	// Warnings surfaced by the run (config warnings, system warnings).
	Warnings []string `json:"warnings,omitempty"`
	Error    error    `json:"error,omitempty"`
}

// MarshalJSON writes Error as its message string. A bare `error` interface
// otherwise marshals to `{}` (no exported fields), which cannot decode back
// into an `error`-typed field — turning any run containing a failed turn
// into an unloadable results.json for `milk eval report` ("json: cannot
// unmarshal object into Go struct field RunResult…error of type error").
func (r RunResult) MarshalJSON() ([]byte, error) {
	type wire RunResult // shed RunResult's methods; the embedded Error field below is shadowed
	out := struct {
		wire
		ErrText string `json:"error,omitempty"`
	}{wire: wire(r)}
	if r.Error != nil {
		out.ErrText = r.Error.Error()
	}
	return json.Marshal(out)
}

// UnmarshalJSON restores Error from its message string and tolerates the
// legacy `{}` object form (files written before the MarshalJSON fix): it
// carries no message, so the turn is still flagged as failed.
func (r *RunResult) UnmarshalJSON(data []byte) error {
	type wire RunResult
	in := struct {
		wire
		ErrText json.RawMessage `json:"error,omitempty"`
	}{}
	if err := json.Unmarshal(data, &in); err != nil {
		return err
	}
	*r = RunResult(in.wire)
	r.Error = nil
	switch {
	case len(in.ErrText) == 0 || string(in.ErrText) == "null":
	case in.ErrText[0] == '"':
		var msg string
		if err := json.Unmarshal(in.ErrText, &msg); err != nil {
			return err
		}
		if msg != "" {
			r.Error = errors.New(msg)
		}
	default:
		r.Error = errors.New("failed turn (error detail not serialised: legacy object form)")
	}
	return nil
}

// FileChange records a file that was created or modified during a turn.
type FileChange struct {
	Path     string `json:"path"`               // relative path in workdir
	Before   string `json:"before,omitempty"`   // content before turn (empty if new file)
	After    string `json:"after,omitempty"`    // content after turn (empty if deleted)
	IsNew    bool   `json:"is_new,omitempty"`   // true if file was created this turn
	Modified bool   `json:"modified,omitempty"` // true if file content changed
}

// TokenUsage captures all token categories for one turn.
// Subagent and workflow fields are populated when the transcript includes
// subagent_usage or workflow_usage data (e.g. from Claude Code's Agent tool
// subagents or background workflows). When absent, they remain zero.
type TokenUsage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	CacheCreate  int64 `json:"cache_create"`
	CacheRead    int64 `json:"cache_read"`
	// Subagent token usage — populated when Claude Code's transcript includes
	// subagent_usage (subagents spawned via the Agent tool).
	SubagentInputTokens  int64 `json:"subagent_input_tokens,omitempty"`
	SubagentOutputTokens int64 `json:"subagent_output_tokens,omitempty"`
	SubagentCacheCreate  int64 `json:"subagent_cache_create,omitempty"`
	SubagentCacheRead    int64 `json:"subagent_cache_read,omitempty"`
	// Workflow token usage — populated when Claude Code's transcript includes
	// workflow_usage (background workflows).
	WorkflowInputTokens  int64 `json:"workflow_input_tokens,omitempty"`
	WorkflowOutputTokens int64 `json:"workflow_output_tokens,omitempty"`
	WorkflowCacheCreate  int64 `json:"workflow_cache_create,omitempty"`
	WorkflowCacheRead    int64 `json:"workflow_cache_read,omitempty"`
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

// Budget holds metric thresholds asserted against every run of a scenario —
// the automated "verify" half of an evaluation: whatever the judge scores,
// a run that exceeds a budget is a FAIL. Zero means "no limit" for every
// field. Budgets come from the experiment file's `budget:` block (defaults
// for every scenario) and/or a scenario's own `budget:` block, which wins
// field-by-field (see MergeBudget).
type Budget struct {
	MaxInputTokens  int64 `yaml:"max_input_tokens,omitempty" json:"max_input_tokens,omitempty"`
	MaxOutputTokens int64 `yaml:"max_output_tokens,omitempty" json:"max_output_tokens,omitempty"`
	MaxTotalTokens  int64 `yaml:"max_total_tokens,omitempty" json:"max_total_tokens,omitempty"`
	MaxToolCalls    int   `yaml:"max_tool_calls,omitempty" json:"max_tool_calls,omitempty"`
	MinToolCalls    int   `yaml:"min_tool_calls,omitempty" json:"min_tool_calls,omitempty"`
	MaxNumTurns     int64 `yaml:"max_num_turns,omitempty" json:"max_num_turns,omitempty"`
	// MaxDurationMS bounds the turn's wall-clock time. In YAML set it via
	// max_duration ("90s", "2m30s" — time.ParseDuration syntax) or
	// max_duration_ms; JSON reports it as max_duration_ms.
	MaxDurationMS int64 `yaml:"-" json:"max_duration_ms,omitempty"`
}

// UnmarshalYAML accepts max_duration as a duration string ("2m") in addition
// to the max_duration_ms integer, so experiment and scenario files read
// naturally.
func (b *Budget) UnmarshalYAML(unmarshal func(any) error) error {
	var raw struct {
		MaxInputTokens  int64  `yaml:"max_input_tokens"`
		MaxOutputTokens int64  `yaml:"max_output_tokens"`
		MaxTotalTokens  int64  `yaml:"max_total_tokens"`
		MaxToolCalls    int    `yaml:"max_tool_calls"`
		MinToolCalls    int    `yaml:"min_tool_calls"`
		MaxNumTurns     int64  `yaml:"max_num_turns"`
		MaxDuration     string `yaml:"max_duration"`
		MaxDurationMS   int64  `yaml:"max_duration_ms"`
	}
	if err := unmarshal(&raw); err != nil {
		return err
	}
	b.MaxInputTokens = raw.MaxInputTokens
	b.MaxOutputTokens = raw.MaxOutputTokens
	b.MaxTotalTokens = raw.MaxTotalTokens
	b.MaxToolCalls = raw.MaxToolCalls
	b.MinToolCalls = raw.MinToolCalls
	b.MaxNumTurns = raw.MaxNumTurns
	b.MaxDurationMS = raw.MaxDurationMS
	if raw.MaxDuration != "" {
		d, err := time.ParseDuration(raw.MaxDuration)
		if err != nil {
			return fmt.Errorf("budget max_duration: %w", err)
		}
		b.MaxDurationMS = d.Milliseconds()
	}
	return nil
}

// IsZero reports whether no threshold is set (an empty budget checks nothing).
func (b *Budget) IsZero() bool {
	if b == nil {
		return true
	}
	return *b == Budget{}
}

// MergeBudget returns a copy of base with every threshold that override sets
// (non-zero) taking precedence — a scenario's own budget refines the
// experiment-level defaults without restating them.
func MergeBudget(base, override *Budget) *Budget {
	if base.IsZero() && override.IsZero() {
		return nil
	}
	out := Budget{}
	if base != nil {
		out = *base
	}
	if override == nil {
		return &out
	}
	if override.MaxInputTokens != 0 {
		out.MaxInputTokens = override.MaxInputTokens
	}
	if override.MaxOutputTokens != 0 {
		out.MaxOutputTokens = override.MaxOutputTokens
	}
	if override.MaxTotalTokens != 0 {
		out.MaxTotalTokens = override.MaxTotalTokens
	}
	if override.MaxToolCalls != 0 {
		out.MaxToolCalls = override.MaxToolCalls
	}
	if override.MinToolCalls != 0 {
		out.MinToolCalls = override.MinToolCalls
	}
	if override.MaxNumTurns != 0 {
		out.MaxNumTurns = override.MaxNumTurns
	}
	if override.MaxDurationMS != 0 {
		out.MaxDurationMS = override.MaxDurationMS
	}
	return &out
}

// BudgetVerdict is the pass/fail outcome of checking one run against a Budget.
type BudgetVerdict struct {
	Passed     bool     `json:"passed"`
	Violations []string `json:"violations,omitempty"`
}

// Evaluate checks one scenario's runs against the budget. A nil/empty budget
// passes everything. Every metric is the scenario's total across its turns
// (a multi-turn scenario's whole trajectory must fit the budget).
func (b *Budget) Evaluate(results []RunResult) BudgetVerdict {
	if b.IsZero() {
		return BudgetVerdict{Passed: true}
	}
	var total TokenUsage
	var duration time.Duration
	var toolCalls, numTurns int
	var hasTurns bool
	for _, r := range results {
		total.InputTokens += r.Tokens.InputTokens
		total.OutputTokens += r.Tokens.OutputTokens
		duration += r.Duration
		toolCalls += len(r.ToolCalls)
		if r.NumTurns > 0 {
			hasTurns = true
			numTurns += int(r.NumTurns)
		}
	}
	var v BudgetVerdict
	check := func(cond bool, format string, args ...any) {
		if !cond {
			v.Violations = append(v.Violations, fmt.Sprintf(format, args...))
		}
	}
	check(b.MaxInputTokens == 0 || total.InputTokens <= b.MaxInputTokens,
		"input tokens %d > max_input_tokens %d", total.InputTokens, b.MaxInputTokens)
	check(b.MaxOutputTokens == 0 || total.OutputTokens <= b.MaxOutputTokens,
		"output tokens %d > max_output_tokens %d", total.OutputTokens, b.MaxOutputTokens)
	check(b.MaxTotalTokens == 0 || total.InputTokens+total.OutputTokens <= b.MaxTotalTokens,
		"total tokens %d > max_total_tokens %d", total.InputTokens+total.OutputTokens, b.MaxTotalTokens)
	check(b.MaxToolCalls == 0 || toolCalls <= b.MaxToolCalls,
		"tool calls %d > max_tool_calls %d", toolCalls, b.MaxToolCalls)
	check(b.MinToolCalls == 0 || toolCalls >= b.MinToolCalls,
		"tool calls %d < min_tool_calls %d", toolCalls, b.MinToolCalls)
	check(b.MaxNumTurns == 0 || !hasTurns || int64(numTurns) <= b.MaxNumTurns,
		"model turns %d > max_num_turns %d", numTurns, b.MaxNumTurns)
	check(b.MaxDurationMS == 0 || duration.Milliseconds() <= b.MaxDurationMS,
		"duration %s > max_duration %s",
		duration.Round(time.Millisecond), time.Duration(b.MaxDurationMS)*time.Millisecond)
	v.Passed = len(v.Violations) == 0
	return v
}
