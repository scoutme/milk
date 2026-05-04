package router

import (
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func testCfg() config.Config {
	return config.Config{
		Rules: config.Rules{
			EscalateAboveTokens: 100,
			EscalateKeywords:    []string{"architect", "refactor entire"},
		},
	}
}

func TestRulesDecision_BelowThreshold(t *testing.T) {
	d := rulesDecision("fix the bug", testCfg())
	if d.Conclusive {
		t.Errorf("short prompt should not be conclusive, got target=%s reason=%s", d.Target, d.Reason)
	}
}

func TestRulesDecision_AboveThreshold(t *testing.T) {
	// ~100 tokens ≈ 400 chars
	long := string(make([]byte, 500))
	d := rulesDecision(string(long), testCfg())
	if !d.Conclusive || d.Target != TargetClaude {
		t.Errorf("long prompt should escalate, got conclusive=%v target=%s", d.Conclusive, d.Target)
	}
}

func TestRulesDecision_KeywordMatch(t *testing.T) {
	cases := []struct {
		prompt string
		want   Target
	}{
		{"please architect a new service", TargetClaude},
		{"refactor entire codebase", TargetClaude},
		{"ARCHITECT the system", TargetClaude}, // case-insensitive
		{"grep for todos", ""},                 // no match → not conclusive
	}
	for _, tc := range cases {
		d := rulesDecision(tc.prompt, testCfg())
		if tc.want == "" {
			if d.Conclusive {
				t.Errorf("prompt %q should not be conclusive", tc.prompt)
			}
		} else {
			if !d.Conclusive || d.Target != tc.want {
				t.Errorf("prompt %q: want conclusive target=%s, got conclusive=%v target=%s", tc.prompt, tc.want, d.Conclusive, d.Target)
			}
		}
	}
}
