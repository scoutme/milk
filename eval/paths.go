package eval

import (
	"os"
	"path/filepath"

	"github.com/scoutme/milk/internal/config"
)

// Eval artifact home: run outputs and machine-local experiment definitions
// live outside the source tree, under the milk state root (docs/internal/
// eval-artifacts.md). Honors --config-root / $MILK_CONFIG_ROOT via config.Dir,
// so a sandboxed state root also sandboxes eval artifacts.

// EvalHome returns the eval artifact home: <milk state root>/eval —
// ~/.milk/eval by default. Falls back to a throwaway directory under the
// system temp dir when no state root resolves at all.
func EvalHome() string {
	d, err := config.Dir()
	if err != nil || d == "" {
		return filepath.Join(os.TempDir(), "milk-eval")
	}
	return filepath.Join(d, "eval")
}

// DefaultResultsDir is the default output directory for eval runs
// (<EvalHome>/results). Each run writes its results.json (and any report)
// under a subdirectory of this.
func DefaultResultsDir() string {
	return filepath.Join(EvalHome(), "results")
}

// ExperimentsDir is the recommended home for machine-local experiment
// definitions (<EvalHome>/experiments) — kept out of the source tree.
func ExperimentsDir() string {
	return filepath.Join(EvalHome(), "experiments")
}
