package eval

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// ---------------------------------------------------------------------------
// Pricing table
// ---------------------------------------------------------------------------

func TestLookupPricing_ExactAndQualified(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)

	for _, tc := range []struct {
		model string
		want  float64 // input price per 1M; 0 = not found
		found bool
	}{
		{"mimo-v2.6-flash", 0.14, true},
		{"xiaomi/mimo-v2.6-flash", 0.14, true},   // path-qualified
		{"mimo-v2.6-pro", 0.435, true},           // exact
		{"provider/mimo-v2.6-pro", 0.435, true},  // qualified
		{"mimo-v2.6-pro-ultraspeed", 4.35, true}, // own entry, not the pro's
		{"mimo-v2.6-pro-extra", 0, false},        // boundary: "-" is not "/"
		{"totally-unknown-model", 0, false},
		{"", 0, false},
	} {
		p, ok := LookupPricing(tc.model)
		if ok != tc.found {
			t.Errorf("LookupPricing(%q) found = %v, want %v", tc.model, ok, tc.found)
			continue
		}
		if ok && p.Input != tc.want {
			t.Errorf("LookupPricing(%q).Input = %v, want %v", tc.model, p.Input, tc.want)
		}
	}
}

func TestLookupPricing_LongestKeyWins(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)
	// Two keys qualify for "mimo/v2-pro" (prefix "mimo/" and suffix "/v2-pro"):
	// the longer key must win instead of an arbitrary map iteration hit.
	SetPricing(map[string]ModelPricing{"mimo": {Input: 9}, "v2-pro": {Input: 7}})
	p, ok := LookupPricing("mimo/v2-pro")
	if !ok || p.Input != 7 {
		t.Errorf("longest-key selection: got (%v,%v), want the v2-pro entry (7)", p.Input, ok)
	}
	if p, _ := LookupPricing("mimo"); p.Input != 9 {
		t.Errorf("exact single-key match = %v, want 9", p.Input)
	}
}

func TestComputeCost_KnownUnknownAndMath(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)

	// 1M input + 500k output + 2M cache-read on flash:
	// 0.14 + 0.5*0.28 + 2*0.0028 = 0.14 + 0.14 + 0.0056
	got := ComputeCost("mimo-v2.6-flash", TokenUsage{
		InputTokens:  1_000_000,
		OutputTokens: 500_000,
		CacheRead:    2_000_000,
		CacheCreate:  0,
	})
	if math.Abs(got-0.2856) > 1e-9 {
		t.Errorf("ComputeCost = %v, want 0.2856", got)
	}

	// Unknown model: 0, never a fabricated figure.
	if c := ComputeCost("no-such-model", TokenUsage{InputTokens: 1e9}); c != 0 {
		t.Errorf("unknown model cost = %v, want 0", c)
	}
}

func TestSetPricing_OverridesBuiltin(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)

	SetPricing(map[string]ModelPricing{"mimo-v2.6-flash": {Input: 2, Output: 4, CacheRead: 0.1}})
	got := ComputeCost("mimo-v2.6-flash", TokenUsage{InputTokens: 1_000_000})
	if math.Abs(got-2.0) > 1e-9 {
		t.Errorf("overridden cost = %v, want 2.0", got)
	}
	ResetPricing()
	got = ComputeCost("mimo-v2.6-flash", TokenUsage{InputTokens: 1_000_000})
	if math.Abs(got-0.14) > 1e-9 {
		t.Errorf("after ResetPricing cost = %v, want builtin 0.14", got)
	}
}

func TestExperimentPricingYAML(t *testing.T) {
	var exp Experiment
	src := `
name: pricing-test
agents: ["milk-tui"]
pricing:
  mimo-v2.6-pro: {input: 0.5, output: 1.0, cache_read: 0.004}
  my-local-model: {input: 0, output: 0}
`
	if err := yaml.Unmarshal([]byte(src), &exp); err != nil {
		t.Fatalf("yaml.Unmarshal: %v", err)
	}
	p, ok := exp.Pricing["mimo-v2.6-pro"]
	if !ok || p.Input != 0.5 || p.Output != 1.0 || p.CacheRead != 0.004 {
		t.Errorf("pricing block parsed as %+v (ok=%v)", p, ok)
	}
	if _, ok := exp.Pricing["my-local-model"]; !ok {
		t.Errorf("second pricing entry missing: %+v", exp.Pricing)
	}
}

// ---------------------------------------------------------------------------
// milk adapter: computed cost
// ---------------------------------------------------------------------------

const costInitFlash = `{"type":"system","subtype":"init","session_id":"s","seq":0,"agent":{"name":"mimo-local","provider":"local","model":"mimo-v2.6-flash"},"session_state":"idle","capabilities":["stream_v1"]}`

func TestParseMilkStream_CostFromModelBuckets(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)

	// Priced per-model bucket wins: 1M pro input = $0.435.
	stream := strings.Join([]string{
		costInitFlash,
		`{"type":"result","subtype":"success","session_id":"s","seq":1,"is_error":false,"num_turns":1,"duration_ms":10,"result":"ok","usage":{"input_tokens":1000000,"output_tokens":0},"model_usage":{"mimo-v2.6-pro":{"input_tokens":1000000,"output_tokens":0}}}`,
	}, "\n") + "\n"
	run, err := parseMilkStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if math.Abs(run.CostUSD-0.435) > 1e-9 {
		t.Errorf("bucket-priced cost = %v, want 0.435", run.CostUSD)
	}
}

func TestParseMilkStream_CostFallsBackToInitModel(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)

	// No model_usage: price the init-reported primary model over the total.
	stream := strings.Join([]string{
		costInitFlash,
		`{"type":"result","subtype":"success","session_id":"s","seq":1,"is_error":false,"num_turns":1,"duration_ms":10,"result":"ok","usage":{"input_tokens":1000000,"output_tokens":0,"cache_read":0}}`,
	}, "\n") + "\n"
	run, err := parseMilkStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if math.Abs(run.CostUSD-0.14) > 1e-9 {
		t.Errorf("model-fallback cost = %v, want 0.14", run.CostUSD)
	}

	// Experiment override changes the number.
	SetPricing(map[string]ModelPricing{"mimo-v2.6-flash": {Input: 2}})
	run, err = parseMilkStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if math.Abs(run.CostUSD-2.0) > 1e-9 {
		t.Errorf("overridden cost = %v, want 2.0", run.CostUSD)
	}
}

func TestParseMilkStream_CostUnknownModelStaysZero(t *testing.T) {
	ResetPricing()
	t.Cleanup(ResetPricing)

	stream := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"s","seq":0,"agent":{"name":"local","provider":"local","model":"some-untuned-model"},"session_state":"idle","capabilities":["stream_v1"]}`,
		`{"type":"result","subtype":"success","session_id":"s","seq":1,"is_error":false,"num_turns":1,"duration_ms":10,"result":"ok","usage":{"input_tokens":1000000,"output_tokens":0}}`,
	}, "\n") + "\n"
	run, err := parseMilkStream(strings.NewReader(stream))
	if err != nil {
		t.Fatalf("parseMilkStream: %v", err)
	}
	if run.CostUSD != 0 {
		t.Errorf("unknown model cost = %v, want 0 (unpriced, never fabricated)", run.CostUSD)
	}
}

// ---------------------------------------------------------------------------
// claude-code adapter: authoritative cost + model
// ---------------------------------------------------------------------------

func TestClaudeJSONLLine_TotalCostUSDAndModel(t *testing.T) {
	var l claudeJSONLLine
	if err := json.Unmarshal([]byte(`{"type":"result","subtype":"success","total_cost_usd":0.42}`), &l); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if l.TotalCostUSD != 0.42 {
		t.Errorf("TotalCostUSD = %v, want 0.42", l.TotalCostUSD)
	}

	var m claudeJSONLLine
	if err := json.Unmarshal([]byte(`{"type":"assistant","message":{"id":"m1","role":"assistant","model":"claude-sonnet-4-5-20250929","content":[]}}`), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.Message.Model != "claude-sonnet-4-5-20250929" {
		t.Errorf("Message.Model = %q, want claude-sonnet-4-5-20250929", m.Message.Model)
	}
}

func TestTurnCostUSD_CumulativeDelta(t *testing.T) {
	turn1 := []claudeJSONLLine{{Type: "result", TotalCostUSD: 0.15}}
	turn2 := []claudeJSONLLine{{Type: "result", TotalCostUSD: 0.30}}

	cost, cum := turnCostUSD(turn1, 0)
	if cost != 0.15 || cum != 0.15 {
		t.Errorf("turn1 = (%v,%v), want (0.15,0.15)", cost, cum)
	}
	cost, cum = turnCostUSD(turn2, cum)
	if cost != 0.15 || cum != 0.30 {
		t.Errorf("turn2 = (%v,%v), want delta (0.15,0.30)", cost, cum)
	}
	// Session reset: total below the running sum is taken at face value.
	cost, cum = turnCostUSD([]claudeJSONLLine{{Type: "result", TotalCostUSD: 0.05}}, cum)
	if cost != 0.05 || cum != 0.05 {
		t.Errorf("reset turn = (%v,%v), want (0.05,0.05)", cost, cum)
	}
	// No result line: nothing counted, cumulative unchanged.
	cost, cum = turnCostUSD([]claudeJSONLLine{{Type: "assistant"}}, cum)
	if cost != 0 || cum != 0.05 {
		t.Errorf("no result line = (%v,%v), want (0,0.05)", cost, cum)
	}
}

func TestTurnModel_LastAssistantWithModel(t *testing.T) {
	lines := []claudeJSONLLine{
		{Type: "result"},
		{Type: "assistant", Message: claudeMessage{Model: "claude-sonnet-4-5-20250929"}},
		{Type: "assistant", Message: claudeMessage{Model: "claude-opus-4-1"}},
		{Type: "assistant", Message: claudeMessage{}}, // no model: skipped
	}
	if got := turnModel(lines); got != "claude-opus-4-1" {
		t.Errorf("turnModel = %q, want claude-opus-4-1 (last assistant that carries one)", got)
	}
	if got := turnModel([]claudeJSONLLine{{Type: "result"}}); got != "" {
		t.Errorf("turnModel with no assistant = %q, want empty", got)
	}
}
