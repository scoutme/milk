package eval

import "time"

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
	Error       error         `json:"error,omitempty"`
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
