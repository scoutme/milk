package eval

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestBudgetYAML_DurationString(t *testing.T) {
	var b Budget
	if err := yaml.Unmarshal([]byte("max_duration: 2m30s\nmax_tool_calls: 12\n"), &b); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if b.MaxDurationMS != 150000 {
		t.Errorf("MaxDurationMS = %d, want 150000", b.MaxDurationMS)
	}
	if b.MaxToolCalls != 12 {
		t.Errorf("MaxToolCalls = %d, want 12", b.MaxToolCalls)
	}

	if err := yaml.Unmarshal([]byte("max_duration: banana\n"), &b); err == nil {
		t.Error("invalid duration string should fail to parse")
	}
}

func TestMergeBudget(t *testing.T) {
	base := &Budget{MaxInputTokens: 100, MaxToolCalls: 5, MaxDurationMS: 60000}
	override := &Budget{MaxToolCalls: 9, MinToolCalls: 1}
	merged := MergeBudget(base, override)
	if merged.MaxInputTokens != 100 {
		t.Errorf("base threshold lost: %+v", merged)
	}
	if merged.MaxToolCalls != 9 {
		t.Errorf("override not applied: %+v", merged)
	}
	if merged.MinToolCalls != 1 {
		t.Errorf("override not applied: %+v", merged)
	}
	if merged.MaxDurationMS != 60000 {
		t.Errorf("base threshold lost: %+v", merged)
	}

	if m := MergeBudget(nil, nil); m != nil {
		t.Errorf("empty merge should be nil, got %+v", m)
	}
	if m := MergeBudget(nil, override); m.MaxToolCalls != 9 {
		t.Errorf("override-only merge: %+v", m)
	}
}

func TestBudgetEvaluate_Pass(t *testing.T) {
	b := &Budget{MaxInputTokens: 100, MaxToolCalls: 3, MaxDurationMS: 5000, MinToolCalls: 1}
	results := []RunResult{
		{Tokens: TokenUsage{InputTokens: 40}, Duration: 2 * time.Second, ToolCalls: []ToolCall{{Name: "a"}}},
		{Tokens: TokenUsage{InputTokens: 50}, Duration: 2 * time.Second, ToolCalls: []ToolCall{{Name: "b"}}},
	}
	v := b.Evaluate(results)
	if !v.Passed {
		t.Errorf("want pass, got violations %v", v.Violations)
	}
}

func TestBudgetEvaluate_Violations(t *testing.T) {
	b := &Budget{MaxInputTokens: 10, MaxToolCalls: 1, MinToolCalls: 5, MaxDurationMS: 1000}
	results := []RunResult{
		{
			Tokens:   TokenUsage{InputTokens: 42, OutputTokens: 7},
			Duration: 3 * time.Second,
			ToolCalls: []ToolCall{
				{Name: "a"}, {Name: "b"}, {Name: "c"},
			},
			NumTurns: 4,
		},
	}
	v := b.Evaluate(results)
	if v.Passed {
		t.Fatal("want fail")
	}
	wantSubs := []string{"input tokens 42", "tool calls 3 > max_tool_calls 1", "tool calls 3 < min_tool_calls 5", "duration 3s"}
	for _, w := range wantSubs {
		found := false
		for _, got := range v.Violations {
			if strings.Contains(got, w) {
				found = true
			}
		}
		if !found {
			t.Errorf("violations %v missing %q", v.Violations, w)
		}
	}
}

func TestBudgetEvaluate_EmptyPasses(t *testing.T) {
	var nilBudget *Budget
	if v := nilBudget.Evaluate([]RunResult{{}}); !v.Passed {
		t.Error("nil budget must pass")
	}
	if v := (&Budget{}).Evaluate([]RunResult{{}}); !v.Passed {
		t.Error("zero budget must pass")
	}
}

func TestBudgetEvaluate_MaxTotalAndTurns(t *testing.T) {
	b := &Budget{MaxTotalTokens: 100, MaxNumTurns: 2}
	results := []RunResult{
		{Tokens: TokenUsage{InputTokens: 60, OutputTokens: 50}, NumTurns: 3},
	}
	v := b.Evaluate(results)
	if v.Passed {
		t.Fatal("want fail")
	}
	joined := strings.Join(v.Violations, "; ")
	if !strings.Contains(joined, "total tokens 110") || !strings.Contains(joined, "model turns 3 > max_num_turns 2") {
		t.Errorf("violations = %v", v.Violations)
	}

	// No timing source (NumTurns 0) never trips the turns budget.
	v = (&Budget{MaxNumTurns: 1}).Evaluate([]RunResult{{}})
	if !v.Passed {
		t.Errorf("absent num_turns must not trip the budget: %v", v.Violations)
	}
}
