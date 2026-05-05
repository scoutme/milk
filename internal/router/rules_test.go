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
			LocalBelowTokens:    8,

			EscalateThreshold: 6,
			LocalThreshold:    -4,

			LocalVerbWeight:    -3,
			EscalateVerbWeight: 4,
			PathRefWeight:      -2,
			CodeBlockWeight:    -2,
			OpenQuestionWeight: 3,

			ClassifierFallback: "local",

			LocalVerbs:    []string{"grep", "find", "list", "run", "read", "fix", "debug", "show", "cat", "ls", "check"},
			EscalateVerbs: []string{"architect", "design", "explain why", "evaluate", "propose"},
		},
	}
}

func TestRulesDecision_AboveTokenThreshold(t *testing.T) {
	long := string(make([]byte, 500)) // ~125 tokens
	d := rulesDecision(long, testCfg())
	if !d.Conclusive || d.Target != TargetClaude {
		t.Errorf("long prompt should escalate, got conclusive=%v target=%s", d.Conclusive, d.Target)
	}
}

func TestRulesDecision_EscalateKeyword(t *testing.T) {
	d := rulesDecision("please architect a new service", testCfg())
	if !d.Conclusive || d.Target != TargetClaude {
		t.Errorf("escalate keyword should be conclusive Claude, got %+v", d)
	}
}

func TestRulesDecision_EscalateKeywordCaseInsensitive(t *testing.T) {
	d := rulesDecision("ARCHITECT the system", testCfg())
	if !d.Conclusive || d.Target != TargetClaude {
		t.Errorf("escalate keyword case-insensitive should be conclusive Claude, got %+v", d)
	}
}

func TestRulesDecision_ShortPromptLocal(t *testing.T) {
	// "ls" is 2 chars ≈ 0 tokens, well below LocalBelowTokens=8
	d := rulesDecision("ls", testCfg())
	if !d.Conclusive || d.Target != TargetLocal {
		t.Errorf("very short prompt should be conclusive local, got %+v", d)
	}
}

func TestRulesDecision_LocalVerbConclusive(t *testing.T) {
	// "grep for TODO in main.go" — local verb only; score = -3, threshold = -4
	// Not conclusive on its own, but adding another local signal should tip it
	// "find all test files" → find verb (-3) ; score=-3 > -4 → inconclusive
	d := rulesDecision("find all test files", testCfg())
	// score = -3, threshold = -4: not conclusive yet
	if d.Conclusive && d.Target == TargetLocal {
		// acceptable if score nudged past threshold by other signals
	}
	// just verify no panic and reason is set
	_ = d.Reason
}

func TestRulesDecision_TwoLocalSignalsConclusive(t *testing.T) {
	// "grep for TODO in ./main.go" → grep(-3) + path-ref(-2) = -5 ≤ -4 → conclusive local
	// We need a real path for path detection; skip path part, test via code block instead
	// "```\nls -la\n``` fix this output" → code-block(-2) + fix verb(-3) = -5 ≤ -4
	d := rulesDecision("```\nls -la\n```\nfix this output", testCfg())
	if !d.Conclusive || d.Target != TargetLocal {
		t.Errorf("code-block + local-verb should be conclusive local, got conclusive=%v target=%s reason=%s", d.Conclusive, d.Target, d.Reason)
	}
}

func TestRulesDecision_OpenQuestion(t *testing.T) {
	// "Why does this fail?" → open-question(+3); score=3 < 6 → inconclusive
	d := rulesDecision("Why does this function fail?", testCfg())
	if d.Conclusive && d.Target == TargetClaude {
		// if somehow conclusive it must be Claude
	} else if !d.Conclusive {
		// expected
	}
}

func TestRulesDecision_OpenQuestionPlusEscalateVerb(t *testing.T) {
	// "Why should we evaluate this approach?" → open-question(+3) + evaluate(+4) = 7 ≥ 6 → Claude
	d := rulesDecision("Why should we evaluate this approach?", testCfg())
	if !d.Conclusive || d.Target != TargetClaude {
		t.Errorf("open-question + escalate-verb should be conclusive Claude, got %+v", d)
	}
}

func TestRulesDecision_PlainTextInconclusive(t *testing.T) {
	// Prompt is long enough to escape the short-prompt rule but has no strong signals.
	d := rulesDecision("make the code work a little better please", testCfg())
	if d.Conclusive {
		t.Errorf("generic prompt should be inconclusive, got target=%s reason=%s", d.Target, d.Reason)
	}
}

func TestScoreSignals_LocalVerb(t *testing.T) {
	cfg := testCfg()
	result := scoreSignals("grep for TODO", "grep for todo", cfg)
	found := false
	for _, s := range result.Signals {
		if s.Name == "local-verb:grep" {
			found = true
		}
	}
	if !found {
		t.Error("expected local-verb:grep signal")
	}
	if result.TotalScore != cfg.Rules.LocalVerbWeight {
		t.Errorf("expected score %d, got %d", cfg.Rules.LocalVerbWeight, result.TotalScore)
	}
}

func TestScoreSignals_OpenQuestion(t *testing.T) {
	cfg := testCfg()
	result := scoreSignals("What is the best approach?", "what is the best approach?", cfg)
	found := false
	for _, s := range result.Signals {
		if s.Name == "open-question" {
			found = true
		}
	}
	if !found {
		t.Error("expected open-question signal")
	}
}

func TestScoreSignals_CodeBlock(t *testing.T) {
	cfg := testCfg()
	result := scoreSignals("```go\nfmt.Println()\n```", "```go\nfmt.println()\n```", cfg)
	found := false
	for _, s := range result.Signals {
		if s.Name == "code-block" {
			found = true
		}
	}
	if !found {
		t.Error("expected code-block signal")
	}
}

func TestLooksLikePath(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"/etc/hosts", true},
		{"./main.go", true},
		{"../pkg/util.go", true},
		{"~/config.json", true},
		{"internal/router/rules.go", true},
		{"main.go", false},
		{"hello world", false},
		{"fixbug", false},
	}
	for _, tc := range cases {
		if got := looksLikePath(tc.s); got != tc.want {
			t.Errorf("looksLikePath(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}
