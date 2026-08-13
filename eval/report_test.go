package eval

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// Test fixtures
// ---------------------------------------------------------------------------

// sampleScenarioResults returns two ScenarioResults (one single-turn, one
// multi-turn) exercising all report paths.
func sampleScenarioResults() []ScenarioResult {
	return []ScenarioResult{
		{
			ScenarioName: "fix-typo-in-readme",
			Category:     "code_generation",
			Difficulty:   "easy",
			AgentResults: map[string]AgentResult{
				"milk-tui": {
					RunResults: []RunResult{
						{
							Response:  "Fixed the typo: projcet -> project.",
							Tokens:    TokenUsage{InputTokens: 1200, OutputTokens: 340, CacheCreate: 0, CacheRead: 0},
							CostUSD:   0.001,
							Duration:  2100 * time.Millisecond,
							ToolCalls: []ToolCall{{Name: "read_file"}, {Name: "edit_file"}},
						},
					},
					Scores: []JudgeScore{
						{Criterion: "correctness", Score: 1.0, Reasoning: "Fixed the typo."},
						{Criterion: "efficiency", Score: 4.0, Reasoning: "Minimal tool calls."},
					},
					WeightedScore: 4.2,
					Cache:         CacheMetrics{CacheHitRate: 0, CacheCreateRate: 0, CachedTokens: 0},
				},
				"claude-code": {
					RunResults: []RunResult{
						{
							Response:  "I found and fixed the typo in README.md.",
							Tokens:    TokenUsage{InputTokens: 25477, OutputTokens: 18, CacheCreate: 25477, CacheRead: 0},
							CostUSD:   0.096,
							Duration:  2500 * time.Millisecond,
							ToolCalls: []ToolCall{{Name: "Read"}},
						},
					},
					Scores: []JudgeScore{
						{Criterion: "correctness", Score: 1.0, Reasoning: "Fixed the typo."},
						{Criterion: "efficiency", Score: 3.0, Reasoning: "More tool overhead."},
					},
					WeightedScore: 3.6,
					Cache:         CacheMetrics{CacheHitRate: 0, CacheCreateRate: 1.0, CachedTokens: 25477},
				},
			},
		},
		{
			ScenarioName: "iterative-refactoring",
			Category:     "refactoring",
			Difficulty:   "medium",
			AgentResults: map[string]AgentResult{
				"milk-tui": {
					RunResults: []RunResult{
						{
							Response: "Refactored to struct-based auth.",
							Tokens:   TokenUsage{InputTokens: 1200, OutputTokens: 340, CacheCreate: 1100, CacheRead: 0},
							CostUSD:  0.001, Duration: 2 * time.Second,
						},
						{
							Response: "Added input validation.",
							Tokens:   TokenUsage{InputTokens: 1300, OutputTokens: 280, CacheCreate: 0, CacheRead: 1100},
							CostUSD:  0.001, Duration: 1500 * time.Millisecond,
						},
						{
							Response: "Added password hashing.",
							Tokens:   TokenUsage{InputTokens: 1400, OutputTokens: 310, CacheCreate: 0, CacheRead: 1300},
							CostUSD:  0.001, Duration: 1800 * time.Millisecond,
						},
					},
					Scores: []JudgeScore{
						{Criterion: "correctness", Score: 1.0},
					},
					WeightedScore: 4.0,
					Cache:         CacheMetrics{CacheHitRate: 0.592, CacheCreateRate: 0.122, CachedTokens: 3500, CacheSavedCost: 0.002},
				},
				"claude-code": {
					RunResults: []RunResult{
						{
							Response: "Refactored to Auth struct.",
							Tokens:   TokenUsage{InputTokens: 25477, OutputTokens: 18, CacheCreate: 25477, CacheRead: 0},
							CostUSD:  0.096, Duration: 3 * time.Second,
						},
						{
							Response: "Added validation.",
							Tokens:   TokenUsage{InputTokens: 26000, OutputTokens: 22, CacheCreate: 0, CacheRead: 25477},
							CostUSD:  0.082, Duration: 2800 * time.Millisecond,
						},
						{
							Response: "Added bcrypt hashing.",
							Tokens:   TokenUsage{InputTokens: 26500, OutputTokens: 25, CacheCreate: 0, CacheRead: 26000},
							CostUSD:  0.085, Duration: 3200 * time.Millisecond,
						},
					},
					Scores: []JudgeScore{
						{Criterion: "correctness", Score: 1.0},
					},
					WeightedScore: 3.9,
					Cache:         CacheMetrics{CacheHitRate: 0.654, CacheCreateRate: 0.167, CachedTokens: 76954, CacheSavedCost: 0.050},
				},
			},
		},
	}
}

// ---------------------------------------------------------------------------
// GenerateReport tests
// ---------------------------------------------------------------------------

func TestGenerateReport_ContainsScenarioNames(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	for _, name := range []string{"fix-typo-in-readme", "iterative-refactoring"} {
		if !strings.Contains(report, name) {
			t.Errorf("report missing scenario name %q", name)
		}
	}
}

func TestGenerateReport_ContainsAgentNames(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	for _, name := range []string{"milk-tui", "claude-code"} {
		if !strings.Contains(report, name) {
			t.Errorf("report missing agent name %q", name)
		}
	}
}

func TestGenerateReport_ContainsWeightedScores(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	if !strings.Contains(report, "Weighted Score") {
		t.Error("report missing Weighted Score row")
	}
}

func TestGenerateReport_ContainsCacheHitRate(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	if !strings.Contains(report, "Cache hit rate") {
		t.Error("report missing Cache hit rate row")
	}
}

func TestGenerateReport_ContainsWinner(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	if !strings.Contains(report, "Overall winner") {
		t.Error("report missing Overall winner")
	}
}

func TestGenerateReport_ContainsAggregateSection(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	if !strings.Contains(report, "=== Aggregate Evaluation Report ===") {
		t.Error("report missing aggregate section header")
	}
}

func TestGenerateReport_ContainsCategoryBreakdown(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	if !strings.Contains(report, "code_generation") {
		t.Error("report missing code_generation category")
	}
	if !strings.Contains(report, "refactoring") {
		t.Error("report missing refactoring category")
	}
}

func TestGenerateReport_MultiTurnCacheBreakdown(t *testing.T) {
	report := GenerateReport(sampleScenarioResults())
	if !strings.Contains(report, "Per-turn cache breakdown") {
		t.Error("report missing multi-turn cache breakdown")
	}
}

func TestGenerateReport_SingleAgent(t *testing.T) {
	srs := []ScenarioResult{
		{
			ScenarioName: "solo",
			Category:     "test",
			Difficulty:   "easy",
			AgentResults: map[string]AgentResult{
				"only-agent": {
					RunResults:    []RunResult{{Response: "ok", Tokens: TokenUsage{InputTokens: 100}, Duration: time.Second}},
					Scores:        []JudgeScore{{Criterion: "correctness", Score: 1.0}},
					WeightedScore: 1.0,
				},
			},
		},
	}
	report := GenerateReport(srs)
	if !strings.Contains(report, "only-agent") {
		t.Error("report missing single agent name")
	}
	if !strings.Contains(report, "Overall winner: only-agent") {
		t.Error("report missing winner for single agent")
	}
}

func TestGenerateReport_EmptyScenarios(t *testing.T) {
	report := GenerateReport(nil)
	if !strings.Contains(report, "=== Agent Evaluation Report ===") {
		t.Error("report missing header")
	}
}

// ---------------------------------------------------------------------------
// GenerateCacheReport tests
// ---------------------------------------------------------------------------

func TestGenerateCacheReport_ContainsHeader(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "=== Cache Efficiency Report ===") {
		t.Error("cache report missing header")
	}
}

func TestGenerateCacheReport_ContainsMultiTurnScenario(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "iterative-refactoring") {
		t.Error("cache report missing multi-turn scenario name")
	}
}

func TestGenerateCacheReport_ContainsTurnCounts(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "3 turns") {
		t.Error("cache report missing turn count")
	}
}

func TestGenerateCacheReport_ContainsPerTurnBreakdown(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "Per-turn breakdown") {
		t.Error("cache report missing per-turn breakdown section")
	}
}

func TestGenerateCacheReport_ContainsAggregate(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "Aggregate") {
		t.Error("cache report missing Aggregate section")
	}
}

func TestGenerateCacheReport_ContainsOverallSummary(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "=== Overall Cache Summary ===") {
		t.Error("cache report missing overall summary")
	}
}

func TestGenerateCacheReport_ContainsCacheSavings(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	if !strings.Contains(report, "Cache savings") {
		t.Error("cache report missing cache savings row")
	}
}

func TestGenerateCacheReport_NoMultiTurn(t *testing.T) {
	srs := []ScenarioResult{
		{
			ScenarioName: "single-only",
			Category:     "test",
			Difficulty:   "easy",
			AgentResults: map[string]AgentResult{
				"agent-a": {
					RunResults:    []RunResult{{Response: "ok"}},
					WeightedScore: 1.0,
				},
			},
		},
	}
	report := GenerateCacheReport(srs)
	if !strings.Contains(report, "No multi-turn scenarios found") {
		t.Error("expected 'no multi-turn' message")
	}
}

func TestGenerateCacheReport_ContainsHitRates(t *testing.T) {
	report := GenerateCacheReport(sampleScenarioResults())
	// Should show percentage values.
	if !strings.Contains(report, "%") {
		t.Error("cache report missing percentage values")
	}
}

// ---------------------------------------------------------------------------
// GenerateJSON tests
// ---------------------------------------------------------------------------

func TestGenerateJSON_ValidJSON(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("GenerateJSON produced invalid JSON: %v", err)
	}
}

func TestGenerateJSON_ScenarioCount(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)
	if len(report.Scenarios) != 2 {
		t.Errorf("got %d scenarios, want 2", len(report.Scenarios))
	}
}

func TestGenerateJSON_AgentCount(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)
	if len(report.Aggregate.Agents) != 2 {
		t.Errorf("got %d aggregate agents, want 2", len(report.Aggregate.Agents))
	}
}

func TestGenerateJSON_ContainsTokenData(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	s := string(data)
	// Check that token fields are present.
	if !strings.Contains(s, "input_tokens") {
		t.Error("JSON missing input_tokens")
	}
	if !strings.Contains(s, "cache_read") {
		t.Error("JSON missing cache_read")
	}
}

func TestGenerateJSON_WinnerPopulated(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)

	if report.Aggregate.Winner == "" {
		t.Error("aggregate winner is empty")
	}
	for _, sr := range report.Scenarios {
		if sr.Winner == "" {
			t.Errorf("scenario %q winner is empty", sr.ScenarioName)
		}
	}
}

func TestGenerateJSON_WeightedScores(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)

	// Check the first scenario's agents have weighted scores.
	sr := report.Scenarios[0]
	for name, ar := range sr.Agents {
		if ar.WeightedScore == 0 {
			t.Errorf("agent %q in scenario %q has zero weighted score", name, sr.ScenarioName)
		}
	}
}

func TestGenerateJSON_DurationInSeconds(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)

	// First scenario, milk-tui: 2100ms = 2.1s.
	milk := report.Scenarios[0].Agents["milk-tui"]
	if len(milk.RunResults) == 0 {
		t.Fatal("no run results")
	}
	dur := milk.RunResults[0].Duration
	if dur < 2.0 || dur > 2.2 {
		t.Errorf("duration = %f, want ~2.1", dur)
	}
}

func TestGenerateJSON_RunResultCount(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)

	// Multi-turn scenario: milk-tui should have 3 run results.
	milk := report.Scenarios[1].Agents["milk-tui"]
	if len(milk.RunResults) != 3 {
		t.Errorf("got %d run results, want 3", len(milk.RunResults))
	}
}

func TestGenerateJSON_TotalTokens(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)

	// Single-turn scenario: milk-tui tokens = 1200 + 340 = 1540.
	milk := report.Scenarios[0].Agents["milk-tui"]
	if milk.TotalTokens != 1540 {
		t.Errorf("total_tokens = %d, want 1540", milk.TotalTokens)
	}
}

func TestGenerateJSON_ToolCallCount(t *testing.T) {
	data := GenerateJSON(sampleScenarioResults())
	var report JSONReport
	json.Unmarshal(data, &report)

	// Single-turn: milk-tui has 2 tool calls.
	milk := report.Scenarios[0].Agents["milk-tui"]
	if milk.ToolCalls != 2 {
		t.Errorf("tool_calls = %d, want 2", milk.ToolCalls)
	}
}

func TestGenerateJSON_EmptyScenarios(t *testing.T) {
	data := GenerateJSON(nil)
	var report JSONReport
	if err := json.Unmarshal(data, &report); err != nil {
		t.Fatalf("empty JSON invalid: %v", err)
	}
	if len(report.Scenarios) != 0 {
		t.Errorf("expected 0 scenarios, got %d", len(report.Scenarios))
	}
}

// ---------------------------------------------------------------------------
// Helper tests
// ---------------------------------------------------------------------------

func TestFormatInt(t *testing.T) {
	tests := []struct {
		input int64
		want  string
	}{
		{0, "0"},
		{1, "1"},
		{999, "999"},
		{1000, "1,000"},
		{25477, "25,477"},
		{1000000, "1,000,000"},
		{-1, "-1"},
		{-1000, "-1,000"},
	}
	for _, tt := range tests {
		got := formatInt(tt.input)
		if got != tt.want {
			t.Errorf("formatInt(%d) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		input time.Duration
		want  string
	}{
		{500 * time.Millisecond, "500ms"},
		{2100 * time.Millisecond, "2.1s"},
		{90 * time.Second, "1m30s"},
	}
	for _, tt := range tests {
		got := formatDuration(tt.input)
		if got != tt.want {
			t.Errorf("formatDuration(%v) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestFormatCompactFloat(t *testing.T) {
	tests := []struct {
		input float64
		want  string
	}{
		{0, "0.0"},
		{28.0, "28.0"},
		{2050.0, "2,050.0"},
		{1234567.8, "1,234,567.8"},
	}
	for _, tt := range tests {
		got := formatCompactFloat(tt.input)
		if got != tt.want {
			t.Errorf("formatCompactFloat(%f) = %q, want %q", tt.input, got, tt.want)
		}
	}
}

func TestPickWinner(t *testing.T) {
	sr := ScenarioResult{
		AgentResults: map[string]AgentResult{
			"low-score":  {WeightedScore: 2.0, RunResults: []RunResult{{CostUSD: 0.001}}},
			"high-score": {WeightedScore: 4.5, RunResults: []RunResult{{CostUSD: 0.1}}},
		},
	}
	if got := pickWinner(sr); got != "high-score" {
		t.Errorf("pickWinner() = %q, want high-score", got)
	}
}

func TestPickWinner_TieBreaksByCost(t *testing.T) {
	sr := ScenarioResult{
		AgentResults: map[string]AgentResult{
			"expensive": {WeightedScore: 4.0, RunResults: []RunResult{{CostUSD: 1.0}}},
			"cheap":     {WeightedScore: 4.0, RunResults: []RunResult{{CostUSD: 0.001}}},
		},
	}
	if got := pickWinner(sr); got != "cheap" {
		t.Errorf("pickWinner() = %q, want cheap", got)
	}
}

func TestCollectAgentNames(t *testing.T) {
	srs := []ScenarioResult{
		{AgentResults: map[string]AgentResult{"b": {}, "a": {}}},
		{AgentResults: map[string]AgentResult{"c": {}, "a": {}}},
	}
	names := collectAgentNames(srs)
	if len(names) != 3 {
		t.Fatalf("got %d names, want 3", len(names))
	}
	// Should be sorted.
	if names[0] != "a" || names[1] != "b" || names[2] != "c" {
		t.Errorf("unexpected order: %v", names)
	}
}

func TestColumnLabel_Alignment(t *testing.T) {
	// Right-aligned by default.
	got := columnLabel("abc", 6)
	if got != "   abc" {
		t.Errorf("right-align: got %q", got)
	}

	// Left-aligned.
	got = columnLabel("abc", 6, AlignLeft)
	if got != "abc   " {
		t.Errorf("left-align: got %q", got)
	}

	// Center-aligned.
	got = columnLabel("abc", 6, AlignCenter)
	if got != " abc  " {
		t.Errorf("center-align: got %q", got)
	}
}
