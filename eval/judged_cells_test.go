package eval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Issue #479: an unjudged cell is missing data, never a real score of 0.
// These tests pin the four layers of the fix: the harness fails loudly on
// empty judge output, the results file carries an explicit `judged` marker,
// the aggregate reports coverage, and the winner line guards incomplete
// judging.

// judgeServerReturning answers every judge call with the given raw content.
func judgeServerReturning(content string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		payload, _ := json.Marshal(map[string]any{
			"choices": []map[string]any{{"message": map[string]any{"content": content}}},
		})
		_, _ = w.Write(payload)
	}))
}

// emptyJudgeScenario is a single-turn scenario with a rubric, so the judge
// has something to score.
func emptyJudgeScenario(rubric bool) Scenario {
	s := Scenario{
		Name:        "s-empty",
		Description: "fixture",
		Category:    "testing",
		Difficulty:  "easy",
		Turns:       []Turn{{Prompt: "do the thing"}},
	}
	if rubric {
		s.Turns[0].Rubric = []RubricCriterion{
			{Criterion: "correctness", Weight: 1, Scoring: "binary"},
		}
		s.Rubric = s.Turns[0].Rubric
	}
	return s
}

// TestRunOne_EmptyJudgeOutputFailsLoudly: a judge that answers every attempt
// with a parseable-but-empty array ("[]") produces no scores and no error on
// its own — the harness must convert that to a judge failure, so the cell
// lands as `judged: false` instead of an aggregate-ready 0.
func TestRunOne_EmptyJudgeOutputFailsLoudly(t *testing.T) {
	srv := judgeServerReturning("[]")
	defer srv.Close()

	h := &Harness{
		Adapters: []AgentAdapter{&stubAdapter{run: func(int) RunResult {
			return RunResult{Response: "done"}
		}}},
		Judge: &Judge{URL: srv.URL, HTTPClient: srv.Client()},
	}
	ar, err := h.runOne(context.Background(), emptyJudgeScenario(true), h.Adapters[0])
	if err != nil {
		t.Fatalf("runOne: %v", err)
	}
	if ar.JudgeError == "" {
		t.Fatal("empty judge output must set JudgeError (fail loudly), got none")
	}
	if ar.Judged {
		t.Error("Judged must be false when the judge produced no scores")
	}
	if ar.judged() {
		t.Error("judged() must be false for empty judge output")
	}
	if ar.WeightedScore != 0 {
		t.Errorf("WeightedScore = %v, want 0", ar.WeightedScore)
	}

	// The report must show ERR (not 0.0), coverage, and the guardrail note.
	report := GenerateReport([]ScenarioResult{{
		ScenarioName: "s-empty",
		Category:     "testing",
		AgentResults: map[string]AgentResult{"stub": ar},
	}})
	for _, want := range []string{"ERR", "Judged cells: stub 0/1", "⚠ incomplete judging"} {
		if !strings.Contains(report, want) {
			t.Errorf("report missing %q:\n%s", want, report)
		}
	}
}

// TestRunOne_NoRubricIsNotAJudgeFailure: silence from the judge on a scenario
// with nothing to score is not a judge failure — the cell stays a normal,
// unflagged result (no ERR, no guardrail).
func TestRunOne_NoRubricIsNotAJudgeFailure(t *testing.T) {
	srv := judgeServerReturning("[]")
	defer srv.Close()

	h := &Harness{
		Adapters: []AgentAdapter{&stubAdapter{run: func(int) RunResult {
			return RunResult{Response: "done"}
		}}},
		Judge: &Judge{URL: srv.URL, HTTPClient: srv.Client()},
	}
	ar, err := h.runOne(context.Background(), emptyJudgeScenario(false), h.Adapters[0])
	if err != nil {
		t.Fatalf("runOne: %v", err)
	}
	if ar.JudgeError != "" {
		t.Errorf("no-rubric scenario must not record a judge failure, got %q", ar.JudgeError)
	}
	if !ar.judged() {
		t.Error("no-rubric cell must count as judged (nothing was supposed to be scored)")
	}
}

// TestAgentResultJudged pins the predicate behind every aggregate: explicit
// failure loses, explicit true wins, legacy cells infer from their data.
func TestAgentResultJudged(t *testing.T) {
	scored := []JudgeScore{{Criterion: "c", Score: 0.0}} // genuine all-zero judgment
	tests := []struct {
		name string
		ar   AgentResult
		want bool
	}{
		{"judge error", AgentResult{JudgeError: "refused"}, false},
		{"explicit false wins over stale error-free state", AgentResult{Judged: false}, false},
		{"explicit true", AgentResult{Judged: true}, true},
		{"explicit true cannot resurrect a judge error", AgentResult{Judged: true, JudgeError: "refused"}, false},
		{"legacy judged cell (scores present)", AgentResult{Scores: scored, WeightedScore: 0}, true},
		{"legacy cell with nonzero weighted score", AgentResult{WeightedScore: 2.5}, true},
		{"legacy unjudged cell (error set)", AgentResult{JudgeError: "parse", Scores: scored}, false},
		{"no scores, no error, no weighted score — never scored", AgentResult{}, false},
	}
	for _, tt := range tests {
		if got := tt.ar.judged(); got != tt.want {
			t.Errorf("%s: judged() = %v, want %v", tt.name, got, tt.want)
		}
	}
}

// TestWriteResults_WritesExplicitJudged: every results.json milk writes must
// carry the `judged` marker, normalized through judged() — including files
// re-scored from legacy input that never had the field.
func TestWriteResults_WritesExplicitJudged(t *testing.T) {
	dir := t.TempDir()
	in := []ScenarioResult{{
		ScenarioName: "s1",
		AgentResults: map[string]AgentResult{
			"scored":     {Scores: []JudgeScore{{Criterion: "c", Score: 4}}, WeightedScore: 4},
			"unscored":   {JudgeError: "refused"},
			"empty-judg": {Scores: []JudgeScore{{Criterion: "c", Score: 0}}, WeightedScore: 0},
		},
	}}
	if err := writeResults(dir, in); err != nil {
		t.Fatalf("writeResults: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "results.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded []struct {
		AgentResults map[string]struct {
			Judged bool `json:"judged"`
		} `json:"agent_results"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("parse written results: %v", err)
	}
	got := decoded[0].AgentResults
	if !got["scored"].Judged {
		t.Error("scored cell must be written judged:true")
	}
	if !got["empty-judg"].Judged {
		t.Error("genuine all-zero judgment must be written judged:true (scores exist)")
	}
	if got["unscored"].Judged {
		t.Error("judge-failed cell must be written judged:false")
	}
}

// fixtureWithUnjudgedCell: two agents over two scenarios; agent b's s2 cell
// never got judged. Means must cover judged cells only, coverage must say so,
// and the winner line must carry the guardrail.
func fixtureWithUnjudgedCell() []ScenarioResult {
	judged := func(ws float64) AgentResult {
		return AgentResult{
			RunResults:    []RunResult{{Response: "ok", CostUSD: 0.01}},
			Scores:        []JudgeScore{{Criterion: "c", Score: ws}},
			WeightedScore: ws,
		}
	}
	return []ScenarioResult{
		{
			ScenarioName: "s1", Category: "testing", Difficulty: "easy",
			AgentResults: map[string]AgentResult{"a": judged(4), "b": judged(2)},
		},
		{
			ScenarioName: "s2", Category: "testing", Difficulty: "easy",
			AgentResults: map[string]AgentResult{
				"a": judged(3),
				"b": {RunResults: []RunResult{{Response: "", CostUSD: 0.01}}, JudgeError: "refused"},
			},
		},
	}
}

func TestReport_CoverageAndWinnerGuardrail(t *testing.T) {
	srs := fixtureWithUnjudgedCell()
	report := GenerateReport(srs)

	if !strings.Contains(report, "Judged cells: a 2/2 | b 1/2") {
		t.Errorf("aggregate missing per-arm judged coverage:\n%s", report)
	}
	if !strings.Contains(report, "⚠ incomplete judging") ||
		!strings.Contains(report, "b 1/2 judged (missing: s2)") {
		t.Errorf("winner guardrail missing or incomplete:\n%s", report)
	}
	// b's mean is over its one judged cell (2.0), not dragged toward 0.
	agg := report[strings.Index(report, "=== Aggregate"):]
	if !strings.Contains(agg, "2.0") {
		t.Errorf("b's aggregate should average its judged cell 2.0:\n%s", agg)
	}
	if !strings.Contains(report, "Overall winner: a") {
		t.Errorf("expected winner line for a:\n%s", report)
	}
}

func TestReport_CompleteJudgingHasNoGuardrailNoise(t *testing.T) {
	srs := fixtureWithUnjudgedCell()
	srs[1].AgentResults["b"] = srs[1].AgentResults["a"] // b judged now
	report := GenerateReport(srs)
	if strings.Contains(report, "⚠ incomplete judging") {
		t.Errorf("complete judging must not emit the guardrail:\n%s", report)
	}
	if !strings.Contains(report, "Judged cells: a 2/2 | b 2/2") {
		t.Errorf("coverage line should still state 2/2:\n%s", report)
	}
}

func TestReport_AllUnjudgedEmitsNoWinner(t *testing.T) {
	srs := []ScenarioResult{{
		ScenarioName: "s1", Category: "testing",
		AgentResults: map[string]AgentResult{
			"a": {JudgeError: "refused"},
			"b": {JudgeError: "refused"},
		},
	}}
	report := GenerateReport(srs)
	if !strings.Contains(report, "Overall winner: none") {
		t.Errorf("all-unjudged run must say there is no winner:\n%s", report)
	}
	if strings.Contains(report, "Overall winner:  (") {
		t.Errorf("empty-prefixed winner line must never be emitted:\n%s", report)
	}
}

func TestHTML_CoverageAndWinnerGuardrail(t *testing.T) {
	html := GenerateHTML(fixtureWithUnjudgedCell())
	if !strings.Contains(html, "Judged cells: a 2/2 | b 1/2") {
		t.Error("HTML aggregate missing judged coverage")
	}
	if !strings.Contains(html, "⚠ incomplete judging") {
		t.Error("HTML winner missing the incomplete-judging guardrail")
	}
}

func TestJSONReport_JudgedCoverage(t *testing.T) {
	data := GenerateJSON(fixtureWithUnjudgedCell())
	var parsed struct {
		Scenarios []struct {
			Agents map[string]struct {
				Judged bool `json:"judged"`
			} `json:"agents"`
		} `json:"scenarios"`
		Aggregate struct {
			Agents map[string]struct {
				JudgedScenarios int `json:"judged_scenarios"`
				TotalScenarios  int `json:"total_scenarios"`
			} `json:"agents"`
		} `json:"aggregate"`
	}
	if err := json.Unmarshal(data, &parsed); err != nil {
		t.Fatalf("parse JSON report: %v", err)
	}
	if !parsed.Scenarios[0].Agents["a"].Judged {
		t.Error("JSON report judged cell must carry judged:true")
	}
	if parsed.Scenarios[1].Agents["b"].Judged {
		t.Error("JSON report unjudged cell must carry judged:false")
	}
	a := parsed.Aggregate.Agents["a"]
	b := parsed.Aggregate.Agents["b"]
	if a.JudgedScenarios != 2 || a.TotalScenarios != 2 {
		t.Errorf("a coverage = %d/%d, want 2/2", a.JudgedScenarios, a.TotalScenarios)
	}
	if b.JudgedScenarios != 1 || b.TotalScenarios != 2 {
		t.Errorf("b coverage = %d/%d, want 1/2", b.JudgedScenarios, b.TotalScenarios)
	}
}
