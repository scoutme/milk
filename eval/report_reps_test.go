package eval

import (
	"strings"
	"testing"
	"time"
)

func TestGenerateReport_ToolCallListAndBudget(t *testing.T) {
	srs := []ScenarioResult{{
		ScenarioName: "fixed-request",
		Category:     "metrics",
		AgentResults: map[string]AgentResult{
			"milk-tui": {
				RunResults: []RunResult{{
					ToolCalls: []ToolCall{
						{Name: "read_file", Args: `{"path":"README.md"}`},
						{Name: "edit_file", Args: `{}`},
						{Name: "read_file", Args: `{}`},
					},
				}},
				BudgetResult: &BudgetResult{
					Budget:     Budget{MaxToolCalls: 2},
					Passed:     false,
					Violations: []string{"tool calls 3 > max_tool_calls 2"},
				},
			},
		},
	}}
	report := GenerateReport(srs)
	if !strings.Contains(report, "Tools [milk-tui] (3): read_file, edit_file, read_file") {
		t.Errorf("report missing the tool-call list:\n%s", report)
	}
	if !strings.Contains(report, "Budget [milk-tui]: FAIL — tool calls 3 > max_tool_calls 2") {
		t.Errorf("report missing the budget verdict:\n%s", report)
	}
	if !strings.Contains(report, "FAIL") {
		t.Errorf("metrics table should show the budget outcome:\n%s", report)
	}
}

func TestGenerateReport_RepetitionStats(t *testing.T) {
	mk := func(rep int, in int64, dur time.Duration, score float64) ScenarioResult {
		return ScenarioResult{
			ScenarioName: "fixed-request",
			Repetition:   rep,
			AgentResults: map[string]AgentResult{
				"milk-tui": {
					RunResults:    []RunResult{{Tokens: TokenUsage{InputTokens: in}, Duration: dur, ToolCalls: []ToolCall{{Name: "x"}}}},
					WeightedScore: score,
					BudgetResult:  &BudgetResult{Passed: rep != 2},
				},
			},
		}
	}
	srs := []ScenarioResult{
		mk(1, 1000, time.Second, 3),
		mk(2, 3000, 3*time.Second, 5),
	}
	report := GenerateReport(srs)

	for _, want := range []string{
		"Scenario: fixed-request (run 1/2)",
		"Scenario: fixed-request (run 2/2)",
		"=== Repetition Statistics ===",
		"Scenario: fixed-request (2 runs)",
		"1/2 pass",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
	// mean ± sd of tokens in: (1000+3000)/2=2000, sd=1414
	if !strings.Contains(report, "2,000 ± 1,414") {
		t.Errorf("report missing mean±sd of tokens in:\n%s", report)
	}
}

func TestGenerateReport_NoRepetitionSectionForSingleRuns(t *testing.T) {
	srs := []ScenarioResult{{
		ScenarioName: "one-off",
		AgentResults: map[string]AgentResult{
			"milk-tui": {RunResults: []RunResult{{}}},
		},
	}}
	if report := GenerateReport(srs); strings.Contains(report, "Repetition Statistics") {
		t.Errorf("single-run report must not show repetition stats:\n%s", report)
	}
}

func TestMeanSD(t *testing.T) {
	m, sd := meanSD([]float64{2, 4, 4, 4, 5, 5, 7, 9})
	if m != 5 {
		t.Errorf("mean = %v, want 5", m)
	}
	if sd < 2.13 || sd > 2.14 {
		t.Errorf("sd = %v, want ~2.138", sd)
	}
	m, sd = meanSD([]float64{7})
	if m != 7 || sd != 0 {
		t.Errorf("single sample = %v ± %v", m, sd)
	}
	m, sd = meanSD(nil)
	if m != 0 || sd != 0 {
		t.Errorf("empty = %v ± %v", m, sd)
	}
}
