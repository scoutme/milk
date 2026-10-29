package eval

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// ---------------------------------------------------------------------------
// Result types used by the report generator
// ---------------------------------------------------------------------------

// ScenarioResult aggregates one scenario's outcomes across all tested agents.
// Repetition is the 1-based run index when the experiment/CLI asked for
// multiple repetitions of the same scenario (each repetition is its own
// ScenarioResult, on purpose: nth-run variance is one of the things an
// experiment measures); it is 0 for a single run.
type ScenarioResult struct {
	ScenarioName string                 `json:"scenario_name"`
	Category     string                 `json:"category"`
	Difficulty   string                 `json:"difficulty"`
	Repetition   int                    `json:"repetition,omitempty"`
	AgentResults map[string]AgentResult `json:"agent_results"`
}

// AgentResult holds all data for one agent on one scenario.
type AgentResult struct {
	RunResults    []RunResult  `json:"run_results"`
	Scores        []JudgeScore `json:"scores"`
	WeightedScore float64      `json:"weighted_score"`
	Cache         CacheMetrics `json:"cache"`
	// BudgetResult is the pass/fail outcome of the effective budget for this
	// scenario/agent run (nil when no budget applies).
	BudgetResult *BudgetResult `json:"budget,omitempty"`
	// JudgeError is non-empty when the judge failed to score this scenario/agent
	// at all (e.g. the judge model's own request was rejected by a content
	// filter, or its output was unparseable even after repair) — as opposed to
	// the judge genuinely scoring every criterion 0. When set, Scores may be
	// empty or a partial prefix and WeightedScore is always 0; every report
	// builder must treat this as "no data," not "a real score of 0," and
	// exclude it from aggregates.
	JudgeError string `json:"judge_error,omitempty"`
	// MissingCriteria lists rubric criteria the judge never returned a score
	// for, even after coverage re-asks (JSON "missing_criteria"). Non-empty
	// means WeightedScore was computed over a partial rubric — the missing
	// weights drop out of both numerator and denominator (see WeightedScore) —
	// so renderers must show those criteria as "—" (never a fabricated 0.0;
	// a genuine 0.0 lives in Scores and renders normally) and mark the weighted
	// score partial. JudgeError remains the stronger signal: judge failed outright.
	MissingCriteria []string `json:"missing_criteria,omitempty"`
}

// BudgetResult is one run's budget check: the budget that was applied and
// the resulting verdict, violations included verbatim for the report.
type BudgetResult struct {
	Budget     Budget   `json:"budget"`
	Passed     bool     `json:"passed"`
	Violations []string `json:"violations,omitempty"`
}

// ---------------------------------------------------------------------------
// Markdown report generator
// ---------------------------------------------------------------------------

// GenerateReport produces a full markdown comparison report covering per-scenario
// tables, multi-turn cache breakdowns, and an aggregate summary with a winner.
// Repetition entries (same scenario run several times) are labeled per run and
// summarized in a repetition-statistics section (mean ± sd), so nth-run
// variance is visible instead of averaged away.
func GenerateReport(scenarioResults []ScenarioResult) string {
	var b strings.Builder

	agentNames := collectAgentNames(scenarioResults)
	repTotals := repCounts(scenarioResults)

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

		title := sr.ScenarioName
		if n := repTotals[sr.ScenarioName]; n > 1 && sr.Repetition > 0 {
			title = fmt.Sprintf("%s (run %d/%d)", sr.ScenarioName, sr.Repetition, n)
		}
		b.WriteString(fmt.Sprintf("Scenario: %s\n", title))
		b.WriteString(fmt.Sprintf("Category: %s | Difficulty: %s\n\n", sr.Category, sr.Difficulty))

		// Score comparison table.
		b.WriteString(buildScoreTable(sr, agentNames))

		// Surface *why* any "ERR" cells above are ERR, not a real 0 — the
		// judge never scored this scenario/agent, so its WeightedScore is
		// excluded from every aggregate below rather than treated as data —
		// and flag partial rubrics: their weighted score (*) covers only the
		// criteria the judge returned after coverage re-asks.
		for _, name := range agentNames {
			ar, ok := sr.AgentResults[name]
			if !ok {
				continue
			}
			if ar.JudgeError != "" {
				b.WriteString(fmt.Sprintf("⚠ judging failed for %s: %s\n", name, ar.JudgeError))
			}
			if len(ar.MissingCriteria) > 0 {
				b.WriteString(fmt.Sprintf("⚠ partial rubric for %s: judge returned no score for %s — the weighted score (*) covers the remaining criteria only\n", name, strings.Join(ar.MissingCriteria, ", ")))
			}
		}

		// Token / cost / duration table.
		b.WriteString(buildMetricsTable(sr, agentNames))

		// The tool-call list behind the count: names in call order, per agent.
		b.WriteString(buildToolCallList(sr, agentNames))

		// Metric-budget verdicts (the automated pass/fail check).
		b.WriteString(buildBudgetVerdicts(sr, agentNames))

		// Multi-turn cache breakdown.
		if isMultiTurn {
			b.WriteString(buildMultiTurnCacheTable(sr, agentNames))
		}

		b.WriteString("\n")
	}

	// --- Repetition statistics (mean ± sd across runs) ---
	if maxRepCount(repTotals) > 1 {
		b.WriteString(buildRepetitionReport(scenarioResults, agentNames))
	}

	// --- Aggregate section ---
	b.WriteString(buildAggregateReport(scenarioResults, agentNames))

	return b.String()
}

// repCounts counts result entries per scenario name (1 = a single run).
func repCounts(srs []ScenarioResult) map[string]int {
	counts := map[string]int{}
	for _, sr := range srs {
		counts[sr.ScenarioName]++
	}
	return counts
}

func maxRepCount(counts map[string]int) int {
	max := 0
	for _, n := range counts {
		if n > max {
			max = n
		}
	}
	return max
}

// buildToolCallList renders each agent's tool calls in call order (the list
// behind the "Tool calls" count). Long lists are truncated — full args and
// results live in results.json.
func buildToolCallList(sr ScenarioResult, agentNames []string) string {
	var b strings.Builder
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		var names []string
		for _, r := range ar.RunResults {
			for _, tc := range r.ToolCalls {
				names = append(names, tc.Name)
			}
		}
		if len(names) == 0 {
			continue
		}
		list := strings.Join(names, ", ")
		const maxLen = 160
		if len(list) > maxLen {
			list = list[:maxLen] + "…"
		}
		b.WriteString(fmt.Sprintf("Tools [%s] (%d): %s\n", name, len(names), list))
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// buildBudgetVerdicts renders one line per agent whose run was checked
// against a metric budget: PASS, or FAIL with every violation spelled out.
func buildBudgetVerdicts(sr ScenarioResult, agentNames []string) string {
	var b strings.Builder
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok || ar.BudgetResult == nil {
			continue
		}
		if ar.BudgetResult.Passed {
			b.WriteString(fmt.Sprintf("Budget [%s]: PASS\n", name))
		} else {
			b.WriteString(fmt.Sprintf("Budget [%s]: FAIL — %s\n", name, strings.Join(ar.BudgetResult.Violations, "; ")))
		}
	}
	if b.Len() > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// buildRepetitionReport summarizes same-scenario repetitions as mean ± sd
// per metric — the nth-run variance an experiment with repetitions > 1 is
// meant to expose. Score is the judge's weighted score; budget is how many
// runs passed the metric budget.
func buildRepetitionReport(scenarioResults []ScenarioResult, agentNames []string) string {
	repTotals := repCounts(scenarioResults)
	if maxRepCount(repTotals) <= 1 {
		return ""
	}

	var b strings.Builder
	b.WriteString("=== Repetition Statistics ===\n\n")

	// Group entries by scenario name (first-seen order).
	var order []string
	byName := map[string][]ScenarioResult{}
	for _, sr := range scenarioResults {
		if _, seen := byName[sr.ScenarioName]; !seen {
			order = append(order, sr.ScenarioName)
		}
		byName[sr.ScenarioName] = append(byName[sr.ScenarioName], sr)
	}

	for _, name := range order {
		group := byName[name]
		if len(group) <= 1 {
			continue
		}
		b.WriteString(fmt.Sprintf("Scenario: %s (%d runs)\n", name, len(group)))

		header := columnLabel("", 18)
		sep := strings.Repeat("─", 18+len(agentNames)*16)
		for _, an := range agentNames {
			header += columnLabel(an, 16)
		}
		b.WriteString(header + "\n" + sep + "\n")

		type sample struct {
			scores  []float64
			inputs  []float64
			outputs []float64
			tools   []float64
			secs    []float64
			budgetN int
			budgetD int
		}
		samples := map[string]*sample{}
		for _, an := range agentNames {
			samples[an] = &sample{}
		}
		for _, sr := range group {
			for _, an := range agentNames {
				ar, ok := sr.AgentResults[an]
				if !ok {
					continue
				}
				s := samples[an]
				if ar.JudgeError == "" {
					s.scores = append(s.scores, ar.WeightedScore)
				}
				var in, out int64
				var tools int
				var secs float64
				for _, r := range ar.RunResults {
					in += r.Tokens.InputTokens
					out += r.Tokens.OutputTokens
					tools += len(r.ToolCalls)
					secs += r.Duration.Seconds()
				}
				s.inputs = append(s.inputs, float64(in))
				s.outputs = append(s.outputs, float64(out))
				s.tools = append(s.tools, float64(tools))
				s.secs = append(s.secs, secs)
				if ar.BudgetResult != nil {
					s.budgetD++
					if ar.BudgetResult.Passed {
						s.budgetN++
					}
				}
			}
		}

		meanSDRow := func(label string, vals func(*sample) []float64, format func(float64) string) {
			row := columnLabel(label, 18)
			for _, an := range agentNames {
				m, sd := meanSD(vals(samples[an]))
				row += columnLabel(fmt.Sprintf("%s ± %s", format(m), format(sd)), 16)
			}
			b.WriteString(row + "\n")
		}
		meanSDRow("Weighted score", func(s *sample) []float64 { return s.scores },
			func(v float64) string { return fmt.Sprintf("%.1f", v) })
		meanSDRow("Tokens in", func(s *sample) []float64 { return s.inputs },
			func(v float64) string { return formatInt(int64(v)) })
		meanSDRow("Tokens out", func(s *sample) []float64 { return s.outputs },
			func(v float64) string { return formatInt(int64(v)) })
		meanSDRow("Tool calls", func(s *sample) []float64 { return s.tools },
			func(v float64) string { return fmt.Sprintf("%.1f", v) })
		meanSDRow("Duration", func(s *sample) []float64 { return s.secs },
			func(v float64) string { return formatDuration(time.Duration(v * float64(time.Second))) })

		row := columnLabel("Budget", 18)
		for _, an := range agentNames {
			s := samples[an]
			if s.budgetD == 0 {
				row += columnLabel("—", 16)
			} else {
				row += columnLabel(fmt.Sprintf("%d/%d pass", s.budgetN, s.budgetD), 16)
			}
		}
		b.WriteString(row + "\n\n")
	}

	return b.String()
}

// meanSD returns the mean and (sample) standard deviation of vals.
// A single sample has sd 0.
func meanSD(vals []float64) (mean, sd float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	for _, v := range vals {
		mean += v
	}
	mean /= float64(len(vals))
	if len(vals) < 2 {
		return mean, 0
	}
	var sumsq float64
	for _, v := range vals {
		d := v - mean
		sumsq += d * d
	}
	return mean, math.Sqrt(sumsq / float64(len(vals)-1))
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

// scoreGrid is the shared model behind the text and HTML score tables — one
// implementation of the subtle bits (criterion union incl. MissingCriteria
// rows, the scored-presence set so 0.0 ≠ absent, ERR vs "—", the partial-rubric
// asterisk) so the two renderers cannot drift apart. Cells carry their display
// text already: "%.1f" scores, "ERR" (judge never scored), "—" (criterion
// never returned for that agent), "-" (agent absent), weighted adds "*" when
// computed over a partial rubric.
type scoreGrid struct {
	criteria []string
	cells    map[string]map[string]string // lower(criterion) -> agent -> text
	weighted map[string]string            // agent -> text
}

// buildScoreGrid computes the score table model for one scenario.
func buildScoreGrid(sr ScenarioResult, agentNames []string) scoreGrid {
	g := scoreGrid{
		cells:    map[string]map[string]string{},
		weighted: map[string]string{},
	}

	// Criterion rows: criteria some agent was actually scored on, plus criteria
	// the judge skipped for an agent (MissingCriteria) — a dropped criterion
	// still gets its row instead of silently vanishing.
	seen := map[string]bool{}
	addCriterion := func(c string) {
		key := strings.ToLower(c)
		if !seen[key] {
			seen[key] = true
			g.criteria = append(g.criteria, c)
		}
	}
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		for _, s := range ar.Scores {
			addCriterion(s.Criterion)
		}
	}
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		for _, c := range ar.MissingCriteria {
			addCriterion(c)
		}
	}

	// Score lookup + explicit "was scored" presence. 0.0 is a legitimate score
	// (binary criteria reach it), so absence cannot be inferred from the zero
	// value — without this, a criterion the judge never returned for an agent
	// rendered as a fake 0.0 in its cell.
	lookup := map[string]map[string]float64{}
	scored := map[string]map[string]bool{}
	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		if !ok {
			continue
		}
		for _, s := range ar.Scores {
			key := strings.ToLower(s.Criterion)
			if lookup[key] == nil {
				lookup[key] = map[string]float64{}
				scored[key] = map[string]bool{}
			}
			lookup[key][name] = s.Score
			scored[key][name] = true
		}
	}

	for _, c := range g.criteria {
		key := strings.ToLower(c)
		row := map[string]string{}
		for _, name := range agentNames {
			switch {
			case sr.AgentResults[name].JudgeError != "":
				// map zero value: JudgeError=="" when the agent has no entry —
				// handled by the scored check below.
				if _, ok := sr.AgentResults[name]; ok {
					row[name] = "ERR"
					continue
				}
				row[name] = "—"
			case !scored[key][name]:
				// Judge never returned this criterion for this agent: an
				// explicit gap, not a zero value indistinguishable from a
				// genuine 0.0 — exactly the misread the missing-criteria
				// episode produced.
				row[name] = "—"
			default:
				row[name] = fmt.Sprintf("%.1f", lookup[key][name])
			}
		}
		g.cells[key] = row
	}

	for _, name := range agentNames {
		ar, ok := sr.AgentResults[name]
		switch {
		case !ok:
			g.weighted[name] = "-"
		case ar.JudgeError != "":
			// Not a real 0 — the judge never scored this scenario/agent at all.
			g.weighted[name] = "ERR"
		default:
			w := fmt.Sprintf("%.1f", ar.WeightedScore)
			if len(ar.MissingCriteria) > 0 {
				// Asterisk: computed over the criteria the judge actually
				// returned (the missing weights dropped out of both sides).
				w += "*"
			}
			g.weighted[name] = w
		}
	}
	return g
}

// buildScoreTable renders the per-scenario score comparison table.
func buildScoreTable(sr ScenarioResult, agentNames []string) string {
	g := buildScoreGrid(sr, agentNames)
	if len(g.criteria) == 0 {
		return ""
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
	for _, c := range g.criteria {
		row := columnLabel(c, 20)
		for _, name := range agentNames {
			row += columnLabel(g.cells[strings.ToLower(c)][name], 12)
		}
		b.WriteString(row + "\n")
	}

	// Weighted score row.
	b.WriteString(sep + "\n")
	row := columnLabel("Weighted Score", 20)
	for _, name := range agentNames {
		row += columnLabel(g.weighted[name], 12)
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
		input, output, cacheCreate, cacheRead             int64
		subInput, subOutput, subCacheCreate, subCacheRead int64
		wfInput, wfOutput, wfCacheCreate, wfCacheRead     int64
		cost                                              float64
		duration                                          time.Duration
		toolCalls                                         int
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
			t.subInput += r.Tokens.SubagentInputTokens
			t.subOutput += r.Tokens.SubagentOutputTokens
			t.subCacheCreate += r.Tokens.SubagentCacheCreate
			t.subCacheRead += r.Tokens.SubagentCacheRead
			t.wfInput += r.Tokens.WorkflowInputTokens
			t.wfOutput += r.Tokens.WorkflowOutputTokens
			t.wfCacheCreate += r.Tokens.WorkflowCacheCreate
			t.wfCacheRead += r.Tokens.WorkflowCacheRead
			t.cost += r.CostUSD
			t.duration += r.Duration
			t.toolCalls += len(r.ToolCalls)
		}
		agentTotals[name] = t
	}

	// hasSubagentOrWorkflow reports whether any agent has non-zero subagent or workflow tokens.
	hasSubagentOrWorkflow := func() bool {
		for _, t := range agentTotals {
			if t.subInput != 0 || t.subOutput != 0 || t.wfInput != 0 || t.wfOutput != 0 {
				return true
			}
		}
		return false
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

	// Subagent tokens — only shown when any agent has non-zero subagent usage.
	if hasSubagentOrWorkflow() {
		row = columnLabel("Subagent (in/out)", 20)
		for _, name := range agentNames {
			t := agentTotals[name]
			if t.subInput == 0 && t.subOutput == 0 {
				row += columnLabel("—", 12)
			} else {
				row += columnLabel(fmt.Sprintf("%s/%s", formatInt(t.subInput), formatInt(t.subOutput)), 12)
			}
		}
		b.WriteString(row + "\n")

		// Workflow tokens.
		row = columnLabel("Workflow (in/out)", 20)
		for _, name := range agentNames {
			t := agentTotals[name]
			if t.wfInput == 0 && t.wfOutput == 0 {
				row += columnLabel("—", 12)
			} else {
				row += columnLabel(fmt.Sprintf("%s/%s", formatInt(t.wfInput), formatInt(t.wfOutput)), 12)
			}
		}
		b.WriteString(row + "\n")
	}

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

	// Budget verdict (metric thresholds) — only when a budget applied.
	hasBudget := false
	for _, name := range agentNames {
		if ar, ok := sr.AgentResults[name]; ok && ar.BudgetResult != nil {
			hasBudget = true
		}
	}
	if hasBudget {
		row = columnLabel("Budget", 20)
		for _, name := range agentNames {
			ar, ok := sr.AgentResults[name]
			switch {
			case !ok || ar.BudgetResult == nil:
				row += columnLabel("—", 12)
			case ar.BudgetResult.Passed:
				row += columnLabel("PASS", 12)
			default:
				row += columnLabel("FAIL", 12)
			}
		}
		b.WriteString(row + "\n")
	}

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
				r.Tokens.CacheCreate + r.Tokens.CacheRead +
				r.Tokens.SubagentInputTokens + r.Tokens.SubagentOutputTokens +
				r.Tokens.SubagentCacheCreate + r.Tokens.SubagentCacheRead +
				r.Tokens.WorkflowInputTokens + r.Tokens.WorkflowOutputTokens +
				r.Tokens.WorkflowCacheCreate + r.Tokens.WorkflowCacheRead
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
			if !ok || ar.JudgeError != "" {
				continue // unjudged — not a real 0, exclude so it can't drag the average down
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
			if ar, ok := sr.AgentResults[name]; ok && ar.JudgeError == "" {
				// Cost is scoped to judged-only scenarios here too, so the ratio's
				// numerator and denominator stay over the same subset — mixing an
				// unjudged scenario's real cost into the denominator while
				// excluding its (nonexistent) quality from the numerator would
				// understate quality/dollar for reasons unrelated to quality.
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
				if ar, ok := sr.AgentResults[name]; ok && ar.JudgeError == "" {
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
	Repetition   int                        `json:"repetition,omitempty"`
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
	// Budget is the run's metric-budget verdict (null when no budget applied).
	Budget *BudgetResult `json:"budget,omitempty"`
	// JudgeError mirrors AgentResult.JudgeError — non-empty means WeightedScore
	// is not real data (the judge never scored this scenario/agent), so
	// consumers of the JSON report must exclude it from aggregates rather than
	// treat 0 as a genuine score.
	JudgeError string `json:"judge_error,omitempty"`
	// MissingCriteria mirrors AgentResult.MissingCriteria — criteria the judge
	// never returned a score for. Non-empty means WeightedScore was computed
	// over a partial rubric (missing weights dropped from numerator and
	// denominator); consumers should surface the gap rather than read the score
	// as full-rubric coverage.
	MissingCriteria []string `json:"missing_criteria,omitempty"`
}

// JSONRunResult is a serializable per-turn result.
type JSONRunResult struct {
	Response              string     `json:"response"`
	Tokens                TokenUsage `json:"tokens"`
	CostUSD               float64    `json:"cost_usd"`
	Duration              float64    `json:"duration_sec"`
	ToolCalls             []ToolCall `json:"tool_calls,omitempty"`
	Model                 string     `json:"model,omitempty"`
	NumTurns              int64      `json:"num_turns,omitempty"`
	StopReason            string     `json:"stop_reason,omitempty"`
	RouteHistory          []string   `json:"route_history,omitempty"`
	TTFTMS                int64      `json:"ttft_ms,omitempty"`
	OutputTokensPerSecond float64    `json:"output_tokens_per_second,omitempty"`
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

	// Per-agent accumulators for the aggregate. qualityCost is scoped to
	// judged-only scenarios (unlike totalCost, which is real spend across all
	// scenarios) so quality-per-dollar's numerator and denominator stay over
	// the same subset — mirroring the markdown report's Quality/dollar fix.
	type accum struct {
		weightedSum float64
		count       int
		totalCost   float64
		qualityCost float64
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
			Repetition:   sr.Repetition,
			Agents:       make(map[string]JSONAgentResult, len(sr.AgentResults)),
		}

		for name, ar := range sr.AgentResults {
			jar := JSONAgentResult{
				Scores:          ar.Scores,
				MissingCriteria: ar.MissingCriteria,
				WeightedScore:   ar.WeightedScore,
				Cache:           ar.Cache,
				Budget:          ar.BudgetResult,
				JudgeError:      ar.JudgeError,
			}

			jar.RunResults = make([]JSONRunResult, 0, len(ar.RunResults))
			for _, r := range ar.RunResults {
				jar.RunResults = append(jar.RunResults, JSONRunResult{
					Response:              r.Response,
					Tokens:                r.Tokens,
					CostUSD:               r.CostUSD,
					Duration:              r.Duration.Seconds(),
					ToolCalls:             r.ToolCalls,
					Model:                 r.Model,
					NumTurns:              r.NumTurns,
					StopReason:            r.StopReason,
					RouteHistory:          r.RouteHistory,
					TTFTMS:                r.TTFTMS,
					OutputTokensPerSecond: r.OutputTokensPerSecond,
				})
				jar.TotalTokens += r.Tokens.InputTokens + r.Tokens.OutputTokens +
					r.Tokens.CacheCreate + r.Tokens.CacheRead
				jar.TotalCostUSD += r.CostUSD
				jar.DurationSec += r.Duration.Seconds()
				jar.ToolCalls += len(r.ToolCalls)
			}

			jsr.Agents[name] = jar

			// Accumulate for aggregate. Cache/cost/token totals reflect real
			// usage regardless of judging outcome, so they accumulate
			// unconditionally; only the judgment-derived weightedSum/count
			// (and qualityCost, its cost-denominator counterpart) are skipped
			// when the judge never scored this scenario/agent — see
			// AgentResult.JudgeError.
			acc := accums[name]
			if ar.JudgeError == "" {
				acc.weightedSum += ar.WeightedScore
				acc.count++
				for _, r := range ar.RunResults {
					acc.qualityCost += r.CostUSD
				}
			}
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
			if acc.qualityCost > 0 {
				qpd = acc.weightedSum / acc.qualityCost
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
// pickWinner returns the agent name with the best WeightedScore (cost as
// tie-break), skipping any agent the judge failed to score at all — an
// unjudged scenario has no real WeightedScore to compare. Returns "" if every
// agent's judging failed.
func pickWinner(sr ScenarioResult) string {
	best := ""
	bestScore := -1.0
	bestCost := 1e18

	for name, ar := range sr.AgentResults {
		if ar.JudgeError != "" {
			continue
		}
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
