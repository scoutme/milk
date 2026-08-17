package eval

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Result types used by the report generator
// ---------------------------------------------------------------------------

// ScenarioResult aggregates one scenario's outcomes across all tested agents.
type ScenarioResult struct {
	ScenarioName string                 `json:"scenario_name"`
	Category     string                 `json:"category"`
	Difficulty   string                 `json:"difficulty"`
	AgentResults map[string]AgentResult `json:"agent_results"`
}

// AgentResult holds all data for one agent on one scenario.
type AgentResult struct {
	RunResults    []RunResult  `json:"run_results"`
	Scores        []JudgeScore `json:"scores"`
	WeightedScore float64      `json:"weighted_score"`
	Cache         CacheMetrics `json:"cache"`
}

// ---------------------------------------------------------------------------
// Markdown report generator
// ---------------------------------------------------------------------------

// GenerateReport produces a full markdown comparison report covering per-scenario
// tables, multi-turn cache breakdowns, and an aggregate summary with a winner.
func GenerateReport(scenarioResults []ScenarioResult) string {
	var b strings.Builder

	agentNames := collectAgentNames(scenarioResults)

	b.WriteString("=== Agent Evaluation Report ===\n\n")

	// --- Per-scenario sections ---
	for _, sr := range scenarioResults {
		isMultiTurn := false
		for _, ar := range sr.AgentResults {
			if len(ar.RunResults) > 1 {
				isMultiTurn = true
				break
			}
		}

		b.WriteString(fmt.Sprintf("Scenario: %s\n", sr.ScenarioName))
		b.WriteString(fmt.Sprintf("Category: %s | Difficulty: %s\n\n", sr.Category, sr.Difficulty))

		// Score comparison table.
		b.WriteString(buildScoreTable(sr, agentNames))

		// Token / cost / duration table.
		b.WriteString(buildMetricsTable(sr, agentNames))

		// Multi-turn cache breakdown.
		if isMultiTurn {
			b.WriteString(buildMultiTurnCacheTable(sr, agentNames))
		}

		b.WriteString("\n")
	}

	// --- Aggregate section ---
	b.WriteString(buildAggregateReport(scenarioResults, agentNames))

	return b.String()
}

// GenerateCacheReport produces a cache-focused markdown report for multi-turn
// scenarios, with per-turn breakdowns and aggregate cache statistics.
func GenerateCacheReport(scenarioResults []ScenarioResult) string {
	var b strings.Builder

	agentNames := collectAgentNames(scenarioResults)

	b.WriteString("=== Cache Efficiency Report ===\n\n")

	// Collect multi-turn scenarios.
	var multiTurn []ScenarioResult
	for _, sr := range scenarioResults {
		for _, ar := range sr.AgentResults {
			if len(ar.RunResults) > 1 {
				multiTurn = append(multiTurn, sr)
				break
			}
		}
	}

	if len(multiTurn) == 0 {
		b.WriteString("No multi-turn scenarios found.\n")
		return b.String()
	}

	// Per-scenario cache breakdown.
	for _, sr := range multiTurn {
		numTurns := 0
		for _, ar := range sr.AgentResults {
			if len(ar.RunResults) > numTurns {
				numTurns = len(ar.RunResults)
			}
		}

		b.WriteString(fmt.Sprintf("Scenario: %s (%d turns)\n\n", sr.ScenarioName, numTurns))
		b.WriteString("Per-turn breakdown:\n")
		b.WriteString(buildCachePerTurnTable(sr, agentNames))
		b.WriteString("\n")

		// Aggregate for this scenario.
		b.WriteString("Aggregate:\n")
		b.WriteString(buildCacheAggregateTable(sr, agentNames))
		b.WriteString("\n")
	}

	// Overall cache summary across all scenarios.
	b.WriteString(buildCacheOverallSummary(scenarioResults, agentNames))

	return b.String()
}

// ---------------------------------------------------------------------------
// Table builders
// ---------------------------------------------------------------------------

// buildScoreTable renders the per-scenario score comparison table.
func buildScoreTable(sr ScenarioResult, agentNames []string) string {
	// Collect all criterion names (preserving first-seen order).
	seen := map[string]bool{}
	var criteria []string
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		for _, s := range ar.Scores {
			if !seen[s.Criterion] {
				seen[s.Criterion] = true
				criteria = append(criteria, s.Criterion)
			}
		}
	}
	if len(criteria) == 0 {
		return ""
	}

	// Build score lookup: criterion -> agent -> score.
	type agentScores map[string]float64
	lookup := map[string]agentScores{}
	for _, c := range criteria {
		lookup[c] = agentScores{}
	}
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		for _, s := range ar.Scores {
			lookup[s.Criterion][name] = s.Score
		}
	}

	// Header.
	header := columnLabel("", 20)
	sep := strings.Repeat("─", 20+len(agentNames)*12)
	for _, name := range agentNames {
		header += columnLabel(name, 12)
	}

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Rows.
	for _, c := range criteria {
		row := columnLabel(c, 20)
		for _, name := range agentNames {
			score := lookup[c][name]
			row += columnLabel(fmt.Sprintf("%.1f", score), 12)
		}
		b.WriteString(row + "\n")
	}

	// Weighted score row.
	b.WriteString(sep + "\n")
	row := columnLabel("Weighted Score", 20)
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			row += columnLabel("-", 12)
			continue
		}
		row += columnLabel(fmt.Sprintf("%.1f", ar.WeightedScore), 12)
	}
	b.WriteString(row + "\n")
	b.WriteString("\n")

	return b.String()
}

// buildMetricsTable renders tokens, cache hit rate, cost, duration, and tool calls.
func buildMetricsTable(sr ScenarioResult, agentNames []string) string {
	header := columnLabel("", 20)
	sep := strings.Repeat("─", 20+len(agentNames)*12)
	for _, name := range agentNames {
		header += columnLabel(name, 12)
	}

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Aggregate token totals per agent.
	type totals struct {
		input, output, cacheCreate, cacheRead int64
		cost                                  float64
		duration                              time.Duration
		toolCalls                             int
	}
	agentTotals := map[string]totals{}
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		var t totals
		for _, r := range ar.RunResults {
			t.input += r.Tokens.InputTokens
			t.output += r.Tokens.OutputTokens
			t.cacheCreate += r.Tokens.CacheCreate
			t.cacheRead += r.Tokens.CacheRead
			t.cost += r.CostUSD
			t.duration += r.Duration
			t.toolCalls += len(r.ToolCalls)
		}
		agentTotals[name] = t
	}

	// Tokens (in/out).
	row := columnLabel("Tokens (in/out)", 20)
	for _, name := range agentNames {
		t := agentTotals[name]
		row += columnLabel(fmt.Sprintf("%s/%s", formatInt(t.input), formatInt(t.output)), 12)
	}
	b.WriteString(row + "\n")

	// Cache (create/read).
	row = columnLabel("Cache (create/read)", 20)
	for _, name := range agentNames {
		t := agentTotals[name]
		row += columnLabel(fmt.Sprintf("%s/%s", formatInt(t.cacheCreate), formatInt(t.cacheRead)), 12)
	}
	b.WriteString(row + "\n")

	// Cache hit rate.
	row = columnLabel("Cache hit rate", 20)
	for _, name := range agentNames {
		ar := sr.AgentResults[name]
		row += columnLabel(fmt.Sprintf("%.1f%%", ar.Cache.CacheHitRate*100), 12)
	}
	b.WriteString(row + "\n")

	// Cost.
	row = columnLabel("Cost USD", 20)
	for _, name := range agentNames {
		t := agentTotals[name]
		row += columnLabel(fmt.Sprintf("$%.3f", t.cost), 12)
	}
	b.WriteString(row + "\n")

	// Duration.
	row = columnLabel("Duration", 20)
	for _, name := range agentNames {
		t := agentTotals[name]
		row += columnLabel(formatDuration(t.duration), 12)
	}
	b.WriteString(row + "\n")

	// Tool calls.
	row = columnLabel("Tool calls", 20)
	for _, name := range agentNames {
		t := agentTotals[name]
		row += columnLabel(fmt.Sprintf("%d", t.toolCalls), 12)
	}
	b.WriteString(row + "\n")

	return b.String()
}

// buildMultiTurnCacheTable renders per-turn cache breakdown for a multi-turn scenario.
func buildMultiTurnCacheTable(sr ScenarioResult, agentNames []string) string {
	// Find max turns.
	maxTurns := 0
	for _, ar := range sr.AgentResults {
		if len(ar.RunResults) > maxTurns {
			maxTurns = len(ar.RunResults)
		}
	}
	if maxTurns < 2 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Per-turn cache breakdown (%d turns):\n\n", maxTurns))

	// Header: Turn + 5 columns per agent.
	header := columnLabel("Turn", 6)
	sep := strings.Repeat("─", 6+len(agentNames)*36)
	for _, name := range agentNames {
		header += columnLabel(name, 36, AlignCenter)
	}
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Sub-header.
	sub := columnLabel("", 6)
	for range agentNames {
		sub += columnLabel("InTok", 7) + columnLabel("OutTok", 7) +
			columnLabel("CacheR", 7) + columnLabel("CacheW", 7) +
			columnLabel("HitRate", 8)
	}
	b.WriteString(sub + "\n")

	// Per-turn rows.
	for turn := 0; turn < maxTurns; turn++ {
		row := columnLabel(fmt.Sprintf("%d", turn+1), 6)
		for _, name := range agentNames {
			ar, ok := sr.AgentResults[name]
			if !ok || turn >= len(ar.RunResults) {
				row += columnLabel("-", 7) + columnLabel("-", 7) +
					columnLabel("-", 7) + columnLabel("-", 7) +
					columnLabel("-", 8)
				continue
			}
			t := ar.RunResults[turn].Tokens
			m := ComputeCacheMetrics(t)
			row += columnLabel(formatInt(t.InputTokens), 7) +
				columnLabel(formatInt(t.OutputTokens), 7) +
				columnLabel(formatInt(t.CacheRead), 7) +
				columnLabel(formatInt(t.CacheCreate), 7) +
				columnLabel(fmt.Sprintf("%.1f%%", m.CacheHitRate*100), 8)
		}
		b.WriteString(row + "\n")
	}
	b.WriteString("\n")

	return b.String()
}

// buildCachePerTurnTable renders the per-turn cache table for the cache report.
func buildCachePerTurnTable(sr ScenarioResult, agentNames []string) string {
	maxTurns := 0
	for _, ar := range sr.AgentResults {
		if len(ar.RunResults) > maxTurns {
			maxTurns = len(ar.RunResults)
		}
	}

	// Header: Turn + 5 columns per agent.
	header := columnLabel("Turn", 6)
	sep := strings.Repeat("─", 6+len(agentNames)*36)
	for _, name := range agentNames {
		header += columnLabel(name, 36, AlignCenter)
	}

	sub := columnLabel("", 6)
	for range agentNames {
		sub += columnLabel("InTok", 7) + columnLabel("OutTok", 7) +
			columnLabel("CacheR", 7) + columnLabel("CacheW", 7) +
			columnLabel("HitRate", 8)
	}

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")
	b.WriteString(sub + "\n")

	for turn := 0; turn < maxTurns; turn++ {
		row := columnLabel(fmt.Sprintf("%d", turn+1), 6)
		for _, name := range agentNames {
			ar, ok := sr.AgentResults[name]
			if !ok || turn >= len(ar.RunResults) {
				row += columnLabel("-", 7) + columnLabel("-", 7) +
					columnLabel("-", 7) + columnLabel("-", 7) +
					columnLabel("-", 8)
				continue
			}
			t := ar.RunResults[turn].Tokens
			m := ComputeCacheMetrics(t)
			row += columnLabel(formatInt(t.InputTokens), 7) +
				columnLabel(formatInt(t.OutputTokens), 7) +
				columnLabel(formatInt(t.CacheRead), 7) +
				columnLabel(formatInt(t.CacheCreate), 7) +
				columnLabel(fmt.Sprintf("%.1f%%", m.CacheHitRate*100), 8)
		}
		b.WriteString(row + "\n")
	}

	return b.String()
}

// buildCacheAggregateTable renders the aggregate cache stats for one scenario.
func buildCacheAggregateTable(sr ScenarioResult, agentNames []string) string {
	header := columnLabel(24)
	sep := strings.Repeat("─", 24+len(agentNames)*12)
	for _, name := range agentNames {
		header += columnLabel(name, 12)
	}

	var b strings.Builder
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Total tokens.
	row := columnLabel("Total tokens", 24)
	for _, name := range agentNames {
		ar := sr.AgentResults[name]
		var total int64
		for _, r := range ar.RunResults {
			total += r.Tokens.InputTokens + r.Tokens.OutputTokens +
				r.Tokens.CacheCreate + r.Tokens.CacheRead
		}
		row += columnLabel(formatInt(total), 12)
	}
	b.WriteString(row + "\n")

	// Cache hit rate.
	row = columnLabel("Cache hit rate", 24)
	for _, name := range agentNames {
		ar := sr.AgentResults[name]
		row += columnLabel(fmt.Sprintf("%.1f%%", ar.Cache.CacheHitRate*100), 12)
	}
	b.WriteString(row + "\n")

	// Cache create.
	row = columnLabel("Cache create", 24)
	for _, name := range agentNames {
		ar := sr.AgentResults[name]
		var create int64
		for _, r := range ar.RunResults {
			create += r.Tokens.CacheCreate
		}
		row += columnLabel(formatInt(create), 12)
	}
	b.WriteString(row + "\n")

	// Cache read.
	row = columnLabel("Cache read", 24)
	for _, name := range agentNames {
		ar := sr.AgentResults[name]
		var read int64
		for _, r := range ar.RunResults {
			read += r.Tokens.CacheRead
		}
		row += columnLabel(formatInt(read), 12)
	}
	b.WriteString(row + "\n")

	// Total cost.
	row = columnLabel("Total cost", 24)
	for _, name := range agentNames {
		ar := sr.AgentResults[name]
		var cost float64
		for _, r := range ar.RunResults {
			cost += r.CostUSD
		}
		row += columnLabel(fmt.Sprintf("$%.3f", cost), 12)
	}
	b.WriteString(row + "\n")

	// Winner.
	winner := pickWinner(sr)
	b.WriteString(fmt.Sprintf("\nWinner: %s\n", winner))

	return b.String()
}

// buildAggregateReport renders the cross-scenario aggregate comparison.
func buildAggregateReport(scenarioResults []ScenarioResult, agentNames []string) string {
	var b strings.Builder

	// Count unique categories.
	categories := map[string]bool{}
	for _, sr := range scenarioResults {
		categories[sr.Category] = true
	}

	b.WriteString("=== Aggregate Evaluation Report ===\n\n")
	b.WriteString(fmt.Sprintf("Scenarios: %d | Categories: %d | Agents: %d\n\n",
		len(scenarioResults), len(categories), len(agentNames)))

	// --- Per-category weighted score averages ---
	b.WriteString("By weighted score (quality):\n")

	header := columnLabel("", 20)
	sep := strings.Repeat("─", 20+len(agentNames)*12)
	for _, name := range agentNames {
		header += columnLabel(name, 12)
	}
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Sort categories for deterministic output.
	sortedCats := sortedKeys(categories)

	type agentAccum struct {
		sum   float64
		count int
	}
	catAccums := map[string]map[string]*agentAccum{}

	for _, cat := range sortedCats {
		catAccums[cat] = map[string]*agentAccum{}
		for _, name := range agentNames {
			catAccums[cat][name] = &agentAccum{}
		}
	}

	for _, sr := range scenarioResults {
		for _, name := range agentNames {
			ar, ok := sr.AgentResults[name]
			if !ok {
				continue
			}
			acc := catAccums[sr.Category][name]
			acc.sum += ar.WeightedScore
			acc.count++
		}
	}

	// Per-category rows.
	overallAccums := map[string]*agentAccum{}
	for _, name := range agentNames {
		overallAccums[name] = &agentAccum{}
	}

	for _, cat := range sortedCats {
		row := columnLabel(cat, 20)
		for _, name := range agentNames {
			acc := catAccums[cat][name]
			avg := 0.0
			if acc.count > 0 {
				avg = acc.sum / float64(acc.count)
			}
			row += columnLabel(fmt.Sprintf("%.1f", avg), 12)
			overallAccums[name].sum += acc.sum
			overallAccums[name].count += acc.count
		}
		b.WriteString(row + "\n")
	}

	// Overall row.
	b.WriteString(sep + "\n")
	row := columnLabel("Overall", 20)
	for _, name := range agentNames {
		acc := overallAccums[name]
		avg := 0.0
		if acc.count > 0 {
			avg = acc.sum / float64(acc.count)
		}
		row += columnLabel(fmt.Sprintf("%.1f", avg), 12)
	}
	b.WriteString(row + "\n\n")

	// --- Cost-efficiency section ---
	b.WriteString("By cost-efficiency (quality per dollar):\n")
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Avg cost / scenario.
	row = columnLabel("Avg cost/scenario", 20)
	for _, name := range agentNames {
		var totalCost float64
		n := 0
		for _, sr := range scenarioResults {
			if ar, ok := sr.AgentResults[name]; ok {
				for _, r := range ar.RunResults {
					totalCost += r.CostUSD
				}
				n++
			}
		}
		avgCost := 0.0
		if n > 0 {
			avgCost = totalCost / float64(n)
		}
		row += columnLabel(fmt.Sprintf("$%.3f", avgCost), 12)
	}
	b.WriteString(row + "\n")

	// Quality / dollar.
	row = columnLabel("Quality/dollar", 20)
	for _, name := range agentNames {
		var totalCost, totalQuality float64
		for _, sr := range scenarioResults {
			if ar, ok := sr.AgentResults[name]; ok {
				for _, r := range ar.RunResults {
					totalCost += r.CostUSD
				}
				totalQuality += ar.WeightedScore
			}
		}
		qpd := 0.0
		if totalCost > 0 {
			qpd = totalQuality / totalCost
		}
		row += columnLabel(formatCompactFloat(qpd), 12)
	}
	b.WriteString(row + "\n")

	// Cache hit rate.
	row = columnLabel("Cache hit rate", 20)
	for _, name := range agentNames {
		var totalRead, totalCreate, totalInput int64
		for _, sr := range scenarioResults {
			if ar, ok := sr.AgentResults[name]; ok {
				for _, r := range ar.RunResults {
					totalRead += r.Tokens.CacheRead
					totalCreate += r.Tokens.CacheCreate
					totalInput += r.Tokens.InputTokens
				}
			}
		}
		total := float64(totalRead + totalCreate + totalInput)
		hitRate := 0.0
		if total > 0 {
			hitRate = float64(totalRead) / total
		}
		row += columnLabel(fmt.Sprintf("%.1f%%", hitRate*100), 12)
	}
	b.WriteString(row + "\n")

	// Overall winner.
	b.WriteString("\n")

	if len(agentNames) > 0 {
		bestAgent := agentNames[0]
		bestScore := -1.0
		bestCost := 1e18
		for _, name := range agentNames {
			var score float64
			var cost float64
			count := 0
			for _, sr := range scenarioResults {
				if ar, ok := sr.AgentResults[name]; ok {
					score += ar.WeightedScore
					for _, r := range ar.RunResults {
						cost += r.CostUSD
					}
					count++
				}
			}
			avgScore := 0.0
			if count > 0 {
				avgScore = score / float64(count)
			}
			if avgScore > bestScore || (avgScore == bestScore && cost < bestCost) {
				bestScore = avgScore
				bestCost = cost
				bestAgent = name
			}
		}

		// Find runner-up cost for the comparison note.
		runnerUpCost := 0.0
		for _, name := range agentNames {
			if name == bestAgent {
				continue
			}
			var cost float64
			for _, sr := range scenarioResults {
				if ar, ok := sr.AgentResults[name]; ok {
					for _, r := range ar.RunResults {
						cost += r.CostUSD
					}
				}
			}
			if runnerUpCost == 0 || cost < runnerUpCost {
				runnerUpCost = cost
			}
		}

		note := ""
		if bestCost > 0 && runnerUpCost > 0 && runnerUpCost > bestCost {
			ratio := runnerUpCost / bestCost
			note = fmt.Sprintf(" (%.0fx cheaper)", ratio)
		}

		b.WriteString(fmt.Sprintf("Overall winner: %s%s\n", bestAgent, note))
	}

	return b.String()
}

// buildCacheOverallSummary renders the overall cache summary across all scenarios.
func buildCacheOverallSummary(scenarioResults []ScenarioResult, agentNames []string) string {
	var b strings.Builder
	b.WriteString("=== Overall Cache Summary ===\n\n")

	header := columnLabel(24)
	sep := strings.Repeat("─", 24+len(agentNames)*12)
	for _, name := range agentNames {
		header += columnLabel(name, 12)
	}
	b.WriteString(header + "\n")
	b.WriteString(sep + "\n")

	// Avg cache hit rate.
	row := columnLabel("Avg cache hit rate", 24)
	for _, name := range agentNames {
		var totalRead, totalCreate, totalInput int64
		for _, sr := range scenarioResults {
			if ar, ok := sr.AgentResults[name]; ok {
				for _, r := range ar.RunResults {
					totalRead += r.Tokens.CacheRead
					totalCreate += r.Tokens.CacheCreate
					totalInput += r.Tokens.InputTokens
				}
			}
		}
		total := float64(totalRead + totalCreate + totalInput)
		hitRate := 0.0
		if total > 0 {
			hitRate = float64(totalRead) / total
		}
		row += columnLabel(fmt.Sprintf("%.1f%%", hitRate*100), 12)
	}
	b.WriteString(row + "\n")

	// Cache savings (aggregate).
	row = columnLabel("Cache savings", 24)
	for _, name := range agentNames {
		var totalSaved, totalCost float64
		for _, sr := range scenarioResults {
			if ar, ok := sr.AgentResults[name]; ok {
				totalSaved += ar.Cache.CacheSavedCost
				for _, r := range ar.RunResults {
					totalCost += r.CostUSD
				}
			}
		}
		savingsPct := 0.0
		if totalCost+totalSaved > 0 {
			savingsPct = totalSaved / (totalCost + totalSaved) * 100
		}
		row += columnLabel(fmt.Sprintf("%.1f%%", savingsPct), 12)
	}
	b.WriteString(row + "\n")

	// Total cost.
	row = columnLabel("Total cost", 24)
	for _, name := range agentNames {
		var cost float64
		for _, sr := range scenarioResults {
			if ar, ok := sr.AgentResults[name]; ok {
				for _, r := range ar.RunResults {
					cost += r.CostUSD
				}
			}
		}
		row += columnLabel(fmt.Sprintf("$%.3f", cost), 12)
	}
	b.WriteString(row + "\n")

	return b.String()
}

// ---------------------------------------------------------------------------
// JSON report generator
// ---------------------------------------------------------------------------

// JSONReport is the top-level JSON output structure.
type JSONReport struct {
	Scenarios []JSONScenarioResult `json:"scenarios"`
	Aggregate JSONAggregate        `json:"aggregate"`
}

// JSONScenarioResult is one scenario's data in the JSON report.
type JSONScenarioResult struct {
	ScenarioName string                     `json:"scenario_name"`
	Category     string                     `json:"category"`
	Difficulty   string                     `json:"difficulty"`
	Agents       map[string]JSONAgentResult `json:"agents"`
	Winner       string                     `json:"winner"`
}

// JSONAgentResult is one agent's data for one scenario in the JSON report.
type JSONAgentResult struct {
	RunResults    []JSONRunResult `json:"run_results"`
	Scores        []JudgeScore    `json:"scores"`
	WeightedScore float64         `json:"weighted_score"`
	Cache         CacheMetrics    `json:"cache"`
	TotalTokens   int64           `json:"total_tokens"`
	TotalCostUSD  float64         `json:"total_cost_usd"`
	DurationSec   float64         `json:"duration_sec"`
	ToolCalls     int             `json:"tool_calls"`
}

// JSONRunResult is a serializable per-turn result.
type JSONRunResult struct {
	Response  string     `json:"response"`
	Tokens    TokenUsage `json:"tokens"`
	CostUSD   float64    `json:"cost_usd"`
	Duration  float64    `json:"duration_sec"`
	ToolCalls []ToolCall `json:"tool_calls,omitempty"`
}

// JSONAggregate holds cross-scenario summary statistics.
type JSONAggregate struct {
	TotalScenarios int                         `json:"total_scenarios"`
	Agents         map[string]JSONAgentSummary `json:"agents"`
	Winner         string                      `json:"winner"`
}

// JSONAgentSummary is one agent's cross-scenario aggregates.
type JSONAgentSummary struct {
	AvgWeightedScore float64 `json:"avg_weighted_score"`
	TotalCostUSD     float64 `json:"total_cost_usd"`
	AvgCacheHitRate  float64 `json:"avg_cache_hit_rate"`
	QualityPerDollar float64 `json:"quality_per_dollar"`
}

// GenerateJSON produces a structured JSON report.
func GenerateJSON(scenarioResults []ScenarioResult) []byte {
	agentNames := collectAgentNames(scenarioResults)

	report := JSONReport{
		Scenarios: make([]JSONScenarioResult, 0, len(scenarioResults)),
	}

	// Per-agent accumulators for the aggregate.
	type accum struct {
		weightedSum float64
		count       int
		totalCost   float64
		totalRead   int64
		totalCreate int64
		totalInput  int64
	}
	accums := map[string]*accum{}
	for _, name := range agentNames {
		accums[name] = &accum{}
	}

	for _, sr := range scenarioResults {
		jsr := JSONScenarioResult{
			ScenarioName: sr.ScenarioName,
			Category:     sr.Category,
			Difficulty:   sr.Difficulty,
			Agents:       make(map[string]JSONAgentResult, len(sr.AgentResults)),
		}

		for name, ar := range sr.AgentResults {
			jar := JSONAgentResult{
				Scores:        ar.Scores,
				WeightedScore: ar.WeightedScore,
				Cache:         ar.Cache,
			}

			jar.RunResults = make([]JSONRunResult, 0, len(ar.RunResults))
			for _, r := range ar.RunResults {
				jar.RunResults = append(jar.RunResults, JSONRunResult{
					Response:  r.Response,
					Tokens:    r.Tokens,
					CostUSD:   r.CostUSD,
					Duration:  r.Duration.Seconds(),
					ToolCalls: r.ToolCalls,
				})
				jar.TotalTokens += r.Tokens.InputTokens + r.Tokens.OutputTokens +
					r.Tokens.CacheCreate + r.Tokens.CacheRead
				jar.TotalCostUSD += r.CostUSD
				jar.DurationSec += r.Duration.Seconds()
				jar.ToolCalls += len(r.ToolCalls)
			}

			jsr.Agents[name] = jar

			// Accumulate for aggregate.
			acc := accums[name]
			acc.weightedSum += ar.WeightedScore
			acc.count++
			for _, r := range ar.RunResults {
				acc.totalCost += r.CostUSD
				acc.totalRead += r.Tokens.CacheRead
				acc.totalCreate += r.Tokens.CacheCreate
				acc.totalInput += r.Tokens.InputTokens
			}
		}

		jsr.Winner = pickWinner(sr)
		report.Scenarios = append(report.Scenarios, jsr)
	}

	// Build aggregate.
	report.Aggregate = JSONAggregate{
		TotalScenarios: len(scenarioResults),
		Agents:         make(map[string]JSONAgentSummary, len(agentNames)),
	}

	if len(agentNames) > 0 {
		bestAgent := agentNames[0]
		bestAvgScore := -1.0
		bestCost := 1e18

		for _, name := range agentNames {
			acc := accums[name]
			avgScore := 0.0
			if acc.count > 0 {
				avgScore = acc.weightedSum / float64(acc.count)
			}
			qpd := 0.0
			if acc.totalCost > 0 {
				qpd = (acc.weightedSum) / acc.totalCost
			}
			total := float64(acc.totalRead + acc.totalCreate + acc.totalInput)
			hitRate := 0.0
			if total > 0 {
				hitRate = float64(acc.totalRead) / total
			}

			report.Aggregate.Agents[name] = JSONAgentSummary{
				AvgWeightedScore: avgScore,
				TotalCostUSD:     acc.totalCost,
				AvgCacheHitRate:  hitRate,
				QualityPerDollar: qpd,
			}

			if avgScore > bestAvgScore || (avgScore == bestAvgScore && acc.totalCost < bestCost) {
				bestAvgScore = avgScore
				bestCost = acc.totalCost
				bestAgent = name
			}
		}
		report.Aggregate.Winner = bestAgent
	}

	data, _ := json.MarshalIndent(report, "", "  ")
	return data
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// collectAgentNames returns a sorted, deduplicated list of agent names across
// all scenario results.
func collectAgentNames(srs []ScenarioResult) []string {
	seen := map[string]bool{}
	for _, sr := range srs {
		for name := range sr.AgentResults {
			seen[name] = true
		}
	}
	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// pickWinner returns the agent name with the highest weighted score.
// Ties are broken by lower total cost.
func pickWinner(sr ScenarioResult) string {
	best := ""
	bestScore := -1.0
	bestCost := 1e18

	for name, ar := range sr.AgentResults {
		var cost float64
		for _, r := range ar.RunResults {
			cost += r.CostUSD
		}
		if ar.WeightedScore > bestScore || (ar.WeightedScore == bestScore && cost < bestCost) {
			bestScore = ar.WeightedScore
			bestCost = cost
			best = name
		}
	}
	return best
}

// alignMode controls text alignment within a column.
type alignMode int

const (
	AlignLeft alignMode = iota
	AlignRight
	AlignCenter
)

// columnLabel formats a value into a fixed-width column.
// Single-argument call uses AlignRight with default width 20.
func columnLabel(parts ...interface{}) string {
	var val string
	var width int
	var align alignMode

	switch len(parts) {
	case 1:
		val = fmt.Sprint(parts[0])
		width = 20
		align = AlignRight
	case 2:
		switch v := parts[1].(type) {
		case int:
			val = fmt.Sprint(parts[0])
			width = v
			align = AlignRight
		case alignMode:
			val = fmt.Sprint(parts[0])
			width = 20
			align = v
		default:
			val = fmt.Sprint(parts[0])
			width = 20
			align = AlignRight
		}
	case 3:
		val = fmt.Sprint(parts[0])
		if w, ok := parts[1].(int); ok {
			width = w
		}
		if a, ok := parts[2].(alignMode); ok {
			align = a
		}
	default:
		return ""
	}

	if len(val) >= width {
		return val
	}
	pad := width - len(val)
	switch align {
	case AlignLeft:
		return val + strings.Repeat(" ", pad)
	case AlignCenter:
		left := pad / 2
		right := pad - left
		return strings.Repeat(" ", left) + val + strings.Repeat(" ", right)
	default: // AlignRight
		return strings.Repeat(" ", pad) + val
	}
}

// formatInt formats an int64 with comma separators (e.g. 25477 -> "25,477").
func formatInt(n int64) string {
	if n == 0 {
		return "0"
	}
	negative := n < 0
	if negative {
		n = -n
	}
	s := fmt.Sprintf("%d", n)
	// Insert commas from the right.
	var result []byte
	for i, c := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, c)
	}
	if negative {
		return "-" + string(result)
	}
	return string(result)
}

// formatDuration formats a duration as a human-readable string (e.g. "2.1s", "1m30s").
func formatDuration(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%.0fms", float64(d.Milliseconds()))
	}
	if d < time.Minute {
		return fmt.Sprintf("%.1fs", d.Seconds())
	}
	mins := int(d.Minutes())
	secs := int(d.Seconds()) % 60
	return fmt.Sprintf("%dm%ds", mins, secs)
}

// formatCompactFloat formats a float with comma-separated thousands and one decimal.
func formatCompactFloat(f float64) string {
	if f == 0 {
		return "0.0"
	}
	// Format with 1 decimal place.
	s := fmt.Sprintf("%.1f", f)
	parts := strings.Split(s, ".")
	intPart := parts[0]

	negative := false
	if intPart[0] == '-' {
		negative = true
		intPart = intPart[1:]
	}

	// Add commas.
	var result []byte
	for i, c := range []byte(intPart) {
		if i > 0 && (len(intPart)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, c)
	}

	out := string(result)
	if negative {
		out = "-" + out
	}
	if len(parts) > 1 {
		out += "." + parts[1]
	}
	return out
}

// sortedKeys returns the keys of the given map in sorted order.
func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
