package eval

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// TestDefaultResultsDirFollowsStateRoot pins the external artifact home:
// run outputs default under the milk state root (~/.milk/eval/results), never
// inside the source tree — and they follow --config-root/$MILK_CONFIG_ROOT so
// a sandboxed state root also sandboxes eval artifacts.
func TestDefaultResultsDirFollowsStateRoot(t *testing.T) {
	root := t.TempDir()
	prev := config.SetRoot(root)
	t.Cleanup(func() { config.SetRoot(prev) })

	if want := filepath.Join(root, "eval", "results"); DefaultResultsDir() != want {
		t.Errorf("DefaultResultsDir() = %q, want %q", DefaultResultsDir(), want)
	}
	if want := filepath.Join(root, "eval", "experiments"); ExperimentsDir() != want {
		t.Errorf("ExperimentsDir() = %q, want %q", ExperimentsDir(), want)
	}
	if home := EvalHome(); filepath.Base(home) != "eval" {
		t.Errorf("EvalHome() = %q, want .../eval", home)
	}
}

// TestLoadExperiment_TildeExpansion pins ~/ expansion in the experiment's
// path fields, so machine-local experiment files can say
// "results: ~/.milk/eval/results/<name>".
func TestLoadExperiment_TildeExpansion(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory")
	}
	path := writeExperiment(t, "name: x\nagents: [milk-tui]\nresults: ~/eval-out\nscenarios: ~/eval-scen\n")
	exp, err := LoadExperiment(path)
	if err != nil {
		t.Fatalf("LoadExperiment: %v", err)
	}
	if want := filepath.Join(home, "eval-out"); exp.Results != want {
		t.Errorf("Results = %q, want %q", exp.Results, want)
	}
	if want := filepath.Join(home, "eval-scen"); exp.Scenarios != want {
		t.Errorf("Scenarios = %q, want %q", exp.Scenarios, want)
	}
}
