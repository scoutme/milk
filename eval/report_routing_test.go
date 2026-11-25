package eval

import (
	"strings"
	"testing"
	"time"
)

// tieScenarioResults returns two single-turn scenarios where both agents
// score identically (exact per-scenario tie) but one is cheaper — the
// milk-vs-pi report shape where "Winner: pi" was really a cost tie-break.
func tieScenarioResults() []ScenarioResult {
	mk := func(name string, scoreA, scoreB, costA, costB float64) ScenarioResult {
		return ScenarioResult{
			ScenarioName: name,
			Category:     "code_generation",
			Difficulty:   "easy",
			AgentResults: map[string]AgentResult{
				"milk": {
					RunResults:    []RunResult{{CostUSD: costA, Duration: time.Second}},
					WeightedScore: scoreA,
				},
				"pi": {
					RunResults:    []RunResult{{CostUSD: costB, Duration: time.Second}},
					WeightedScore: scoreB,
				},
			},
		}
	}
	return []ScenarioResult{
		mk("s1", 2.5, 2.5, 0.01, 0.004),
		mk("s2", 2.55, 2.5, 0.01, 0.004),
	}
}

// TestReport_TieIsNotAWinner: equal top scores must render as a cost
// tie-break, never as a quality win — the "8–2" headline of the milk-vs-pi
// report was seven such tie-breaks.
func TestReport_TieIsNotAWinner(t *testing.T) {
	out := GenerateReport(tieScenarioResults())

	if !strings.Contains(out, "Winner: pi (score tie — cost tie-break)") {
		t.Errorf("scenario tie not labeled as cost tie-break (the cheaper agent wins it):\n%s", out)
	}
	if strings.Contains(out, "Winner: pi\n") {
		t.Errorf("scenario tie rendered as a plain winner:\n%s", out)
	}
}

// TestReport_OverallTieShowsHiddenMeans: when the judged means round to the
// same value at the report's one-decimal display precision, the overall
// line must say tie and show the two-decimal means — a winner hidden by the
// report's own rounding is not a winner.
func TestReport_OverallTieShowsHiddenMeans(t *testing.T) {
	out := GenerateReport(tieScenarioResults())

	if !strings.Contains(out, "Overall winner: milk (tie: ") {
		t.Errorf("overall tie not rendered:\n%s", out)
	}
	if !strings.Contains(out, "— gap below display precision)") {
		t.Errorf("overall tie must name the honest tie-break:\n%s", out)
	}
	if strings.Contains(out, "(2x cheaper)") {
		t.Errorf("cost note must not masquerade as a quality win:\n%s", out)
	}
}

// TestReport_OverallRealWinShowsMeans: a clear gap renders as a winner with
// both means visible.
func TestReport_OverallRealWinShowsMeans(t *testing.T) {
	srs := []ScenarioResult{
		{
			ScenarioName: "s1",
			Category:     "smoke",
			AgentResults: map[string]AgentResult{
				"milk": {RunResults: []RunResult{{CostUSD: 0.01}}, WeightedScore: 5.0},
				"pi":   {RunResults: []RunResult{{CostUSD: 0.004}}, WeightedScore: 1.0},
			},
		},
	}
	out := GenerateReport(srs)

	if !strings.Contains(out, "Overall winner: milk (5.00 vs 1.00)") {
		t.Errorf("overall winner must show both means:\n%s", out)
	}
	if strings.Contains(out, "(tie:") {
		t.Errorf("clear gap must not render as tie:\n%s", out)
	}
}

// TestReport_RoutingLine: route target + reason + billing model per turn —
// the "why" behind route_history, without which routing cost reads as pure
// overhead.
func TestReport_RoutingLine(t *testing.T) {
	srs := []ScenarioResult{
		{
			ScenarioName: "hard-design",
			Category:     "routing",
			AgentResults: map[string]AgentResult{
				"milk": {
					RunResults: []RunResult{
						{
							RouteHistory: []string{"escalation"},
							RouteReasons: []string{"model classifier"},
							ModelsUsed:   []string{"mimo-v2.6-pro"},
							CostUSD:      0.01,
						},
						{
							RouteHistory: []string{"primary"},
							RouteReasons: []string{"short prompt (local)"},
							ModelsUsed:   []string{"mimo-v2.6-flash"},
							CostUSD:      0.001,
						},
					},
					WeightedScore: 4.0,
				},
			},
		},
	}
	out := GenerateReport(srs)

	want := "Routing [milk]: T1 escalation/mimo-v2.6-pro (model classifier) → T2 primary/mimo-v2.6-flash (short prompt (local))"
	if !strings.Contains(out, want) {
		t.Errorf("routing line missing; want %q\n%s", want, out)
	}
}

// TestRouteHopLabels_NoData renders nothing: older results.json files (no
// route/model data) must not grow empty annotation rows.
func TestRouteHopLabels_NoData(t *testing.T) {
	ar := AgentResult{RunResults: []RunResult{{Response: "hi"}}}
	if hops := routeHopLabels(ar); len(hops) != 0 {
		t.Errorf("routeHopLabels = %v, want empty", hops)
	}
}

// TestHTML_TieBadgeAndRouting: the HTML report carries the same honest
// labels and routing attribution as the text report.
func TestHTML_TieBadgeAndRouting(t *testing.T) {
	html := GenerateHTML(tieScenarioResults())
	if !strings.Contains(html, "(score tie — cost tie-break)") {
		t.Error("HTML badge missing the cost tie-break label")
	}
	if !strings.Contains(html, "Overall winner: milk (tie: ") {
		t.Error("HTML overall line missing the tie label")
	}
}
