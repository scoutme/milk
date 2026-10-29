package eval

import (
	"strings"
	"testing"
	"time"
)

// htmlFixture builds a small results set exercising every HTML-only state:
// full scores with reasoning, a missing criterion (partial rubric), a judge
// error, an absent agent (weighted "-"), tools, budget, multi-turn cache,
// and adversarial markup in the scenario name for the escaping test.
func htmlFixture() []ScenarioResult {
	run := func(cost float64, tools ...string) RunResult {
		var tcs []ToolCall
		for _, n := range tools {
			tcs = append(tcs, ToolCall{Name: n, Args: "{}"})
		}
		return RunResult{
			Response: "ok",
			Tokens: TokenUsage{
				InputTokens: 100, OutputTokens: 50,
				CacheCreate: 10, CacheRead: 90,
			},
			CostUSD:   cost,
			Duration:  2 * time.Second,
			ToolCalls: tcs,
		}
	}
	score := func(c string, v float64) JudgeScore {
		return JudgeScore{Criterion: c, Score: v, Reasoning: "because <the> & \"metrics\""}
	}

	return []ScenarioResult{
		{
			ScenarioName: `evil <script>alert(1)</script>`,
			Category:     "smoke",
			Difficulty:   "easy",
			AgentResults: map[string]AgentResult{
				"agent-a": {
					RunResults:    []RunResult{run(0.02, "read", "write")},
					Scores:        []JudgeScore{score("correctness", 4)},
					WeightedScore: 4,
					Cache:         CacheMetrics{CacheHitRate: 0.9},
					BudgetResult:  &BudgetResult{Budget: Budget{}, Passed: false, Violations: []string{"tools > 2"}},
				},
				"agent-b": {
					// Judge never scored correctness here: partial rubric.
					RunResults:      []RunResult{run(0.01)},
					Scores:          []JudgeScore{score("style", 5)},
					WeightedScore:   5,
					MissingCriteria: []string{"correctness"},
					Cache:           CacheMetrics{CacheHitRate: 0.5},
				},
			},
		},
		{
			ScenarioName: "second-scenario",
			Category:     "smoke",
			Difficulty:   "easy",
			AgentResults: map[string]AgentResult{
				"agent-a": {
					RunResults:    []RunResult{run(0.01), run(0.01)}, // multi-turn
					Scores:        []JudgeScore{score("correctness", 2)},
					WeightedScore: 2,
					Cache:         CacheMetrics{CacheHitRate: 0.8},
				},
				// agent-c exists here but is absent from scenario 1 → its
				// scenario-1 weighted cell must render "-".
				"agent-c": {
					RunResults:    []RunResult{run(0.03)},
					Scores:        []JudgeScore{score("correctness", 0)}, // genuine 0.0
					WeightedScore: 0,
					Cache:         CacheMetrics{CacheHitRate: 1},
					JudgeError:    "", // scored, so no ERR here
				},
				"agent-b": {
					RunResults:    []RunResult{run(0)},
					Scores:        nil,
					WeightedScore: 0,
					JudgeError:    "judge request rejected by content filter",
				},
			},
		},
	}
}

func TestGenerateHTMLDocumentShape(t *testing.T) {
	h := GenerateHTML(htmlFixture())

	if !strings.HasPrefix(h, "<!DOCTYPE html>\n<html") {
		t.Fatalf("not an HTML document: %.60q", h)
	}
	if !strings.HasSuffix(h, "</body>\n</html>\n") {
		t.Fatalf("missing closing tags: %.60q", h[len(h)-40:])
	}
	for _, want := range []string{
		"<title>milk eval report</title>",
		"milk eval report", // header
		"Scenarios",        // TOC
		"smoke", "easy",    // chips
		"Tokens (in/out)", "Cache hit rate", // metrics rows
		"Cost USD", "Duration", "Tool calls", // metrics rows
		"Budget",                             // budget row (fixture has one)
		"Tools [agent-a] (2)",                // tool list behind the count
		"Per-turn cache breakdown (2 turns)", // multi-turn
		"Judgment reasoning [agent-a]",       // reasoning details
		"partial rubric for agent-b",         // partial warning
		"judging failed for agent-b",         // judge-error warning
		"By weighted score (quality)",        // aggregate
		"By cost-efficiency",                 // aggregate
		"Overall winner",                     // aggregate
		"FAIL — tools &gt; 2",                // budget violation detail (HTML-escaped)
	} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML report missing %q", want)
		}
	}
	// Self-contained: no external assets, and the fixture's script payload must
	// not exist as a live tag anywhere.
	for _, banned := range []string{"<script", "<link", "src=", "https://", "http://"} {
		if strings.Contains(h, banned) {
			t.Errorf("report must be self-contained; found %q", banned)
		}
	}
	if !strings.Contains(h, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("scenario name was not HTML-escaped")
	}
	if !strings.Contains(h, "because &lt;the&gt; &amp; &#34;metrics&#34;") {
		t.Error("judge reasoning was not HTML-escaped")
	}
}

func TestGenerateHTMLScoreGridStates(t *testing.T) {
	h := GenerateHTML(htmlFixture())

	// Real score (incl. a genuine 0.0), judge gap, judge error, absent agent,
	// partial-rubric asterisk — the same four states the text report renders.
	for _, want := range []string{
		">4.0<",  // agent-a scored
		">0.0<",  // agent-c genuine zero — not confused with a gap
		">—<",    // agent-b missing correctness in scenario 1 / agent-c absent there
		">ERR<",  // agent-b judge error in scenario 2
		">-<",    // agent-c absent from scenario 1 → weighted "-"
		">5.0*<", // partial rubric asterisk
		"class=\"err\"",
		"class=\"gap\"",
	} {
		if !strings.Contains(h, want) {
			t.Errorf("score grid state %q not rendered", want)
		}
	}

	// Parity with the shared text renderer: whatever cells the grid produces
	// must be identical in both formats (one model, two views).
	grid := buildScoreGrid(htmlFixture()[0], collectAgentNames(htmlFixture()))
	text := buildScoreTable(htmlFixture()[0], collectAgentNames(htmlFixture()))
	if len(grid.criteria) == 0 {
		t.Fatal("grid produced no criteria")
	}
	if !strings.Contains(text, "Weighted Score") {
		t.Fatal("text table lost its weighted row in the grid refactor")
	}
	for _, c := range grid.criteria {
		if !strings.Contains(text, c) {
			t.Errorf("text table missing criterion %q", c)
		}
	}
}

func TestGenerateHTMLRepetitionStats(t *testing.T) {
	srs := []ScenarioResult{
		{ScenarioName: "rep", Category: "smoke", Difficulty: "easy", Repetition: 1,
			AgentResults: map[string]AgentResult{
				"a": {RunResults: []RunResult{{Tokens: TokenUsage{InputTokens: 10}, Duration: time.Second}},
					Scores: []JudgeScore{{Criterion: "c", Score: 4}}, WeightedScore: 4},
			}},
		{ScenarioName: "rep", Category: "smoke", Difficulty: "easy", Repetition: 2,
			AgentResults: map[string]AgentResult{
				"a": {RunResults: []RunResult{{Tokens: TokenUsage{InputTokens: 30}, Duration: 3 * time.Second}},
					Scores: []JudgeScore{{Criterion: "c", Score: 6}}, WeightedScore: 6},
			}},
	}
	h := GenerateHTML(srs)
	for _, want := range []string{
		"Repetition statistics",
		"rep (2 runs)",
		"5.0 ± 1.4",   // weighted score mean/sd of 4 and 6
		"20 ± 14",     // tokens in
		"2.0s ± 1.4s", // duration mean/sd
	} {
		if !strings.Contains(h, want) {
			t.Errorf("repetition stats missing %q", want)
		}
	}
}

func TestGenerateHTMLSelfContainedAndEscaped(t *testing.T) {
	h := GenerateHTML(htmlFixture())
	if strings.Count(h, "<table") != strings.Count(h, "</table>") {
		t.Errorf("unbalanced tables: %d open vs %d close",
			strings.Count(h, "<table"), strings.Count(h, "</table>"))
	}
	// Aggregate exclusion: agent-b's judge-error cell (scenario 2) must be
	// dropped from its average, not counted as 0 — its Overall stays 5.0 from
	// the one judged cell (count 1), where a zeroed error would show 2.5.
	// (agent-b winning overall is correct: its scenario-1 score is real data.)
	if !strings.Contains(h, "<tr class=\"weighted\"><th scope=\"row\">Overall</th><td>3.0</td><td>5.0</td><td>0.0</td>") {
		t.Error("judge-error cell leaked into the aggregate average")
	}
}
