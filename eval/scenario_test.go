package eval

import (
	"path/filepath"
	"runtime"
	"testing"
)

func scenariosDir() string {
	_, thisFile, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(thisFile), "scenarios")
}

func TestLoadScenarios(t *testing.T) {
	dir := scenariosDir()
	scenarios, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios(%q) error: %v", dir, err)
	}
	if len(scenarios) == 0 {
		t.Fatal("LoadScenarios returned 0 scenarios")
	}

	// Check that _base.yaml was skipped.
	for _, s := range scenarios {
		if s.Name == "_base" {
			t.Error("_base.yaml should be skipped")
		}
	}

	// Check expected categories.
	categories := map[string]bool{}
	for _, s := range scenarios {
		categories[s.Category] = true
	}
	for _, want := range []string{"code_generation", "debugging", "refactoring", "smoke"} {
		if !categories[want] {
			t.Errorf("missing expected category %q", want)
		}
	}

	// Check expected scenario names.
	names := map[string]bool{}
	for _, s := range scenarios {
		names[s.Name] = true
	}
	for _, want := range []string{"fix-typo", "add-function", "write-test", "fix-nil-pointer", "fix-off-by-one", "iterative-refactor", "add-validation", "say-pong"} {
		if !names[want] {
			t.Errorf("missing expected scenario %q", want)
		}
	}

	// Total should be 8 scenarios (3 code_generation + 2 debugging + 2 refactoring + 1 smoke).
	if len(scenarios) != 8 {
		t.Errorf("got %d scenarios, want 8", len(scenarios))
	}

	t.Logf("Loaded %d scenarios across %d categories", len(scenarios), len(categories))
	for _, s := range scenarios {
		turns := len(s.Turns)
		if turns == 0 {
			turns = 1 // single-turn via prompt field
		}
		t.Logf("  %s [%s] %s — %d turn(s), difficulty=%s", s.Name, s.Category, s.Description[:min(50, len(s.Description))], turns, s.Difficulty)
	}
}

func TestLoadScenarios_MultiTurnParsed(t *testing.T) {
	dir := scenariosDir()
	scenarios, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios error: %v", err)
	}

	for _, s := range scenarios {
		if s.Name == "iterative-refactor" {
			if len(s.Turns) != 3 {
				t.Errorf("iterative-refactor: got %d turns, want 3", len(s.Turns))
			}
			if !s.MultiTurn {
				t.Error("iterative-refactor: MultiTurn should be true")
			}
			if s.CacheExpect == nil {
				t.Error("iterative-refactor: CacheExpect should be set")
			}
			return
		}
	}
	t.Error("iterative-refactor scenario not found")
}

func TestLoadScenarios_RubricDefaults(t *testing.T) {
	dir := scenariosDir()
	scenarios, err := LoadScenarios(dir)
	if err != nil {
		t.Fatalf("LoadScenarios error: %v", err)
	}

	for _, s := range scenarios {
		if s.Name == "fix-typo" {
			// _base.yaml sets default_scoring: scale_1_5, default_weight: 1
			// fix-typo rubric: correctness has scoring: binary (explicit), weight: 3 (explicit)
			//                 efficiency has scoring: scale_1_5 (from base default), weight: 1 (from base default? no, weight is set to 1 explicitly)
			for _, r := range s.Rubric {
				if r.Scoring == "" {
					t.Errorf("fix-typo: criterion %q has empty scoring after base defaults", r.Criterion)
				}
				if r.Weight == 0 {
					t.Errorf("fix-typo: criterion %q has zero weight after base defaults", r.Criterion)
				}
			}
			return
		}
	}
	t.Error("fix-typo scenario not found")
}

func TestLoadScenarios_BadDir(t *testing.T) {
	_, err := LoadScenarios("/nonexistent/dir")
	if err == nil {
		t.Error("expected error for nonexistent dir")
	}
}
