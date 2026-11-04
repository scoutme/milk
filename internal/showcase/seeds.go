package showcase

import (
	"fmt"
	"os"
	"os/exec"
)

// RunSeeds executes the manifest's Seed commands for one pass: each command
// runs via `sh -c` with cwd = the run workdir and env WORKDIR + STATE_ROOT
// (the pass's config root), in order, failing loudly on the first error with
// its combined output — a plot's starting state is the recorder's contract,
// so a seed that didn't apply is a broken pass, not a degraded one.
//
// Seeds run before every pass (verify, calibrate, and the recorded replay)
// because every pass needs identical starting state: calibration mutates the
// workdir (the model under test edits files), and the record pass must replay
// against the pre-edit state the calibration measured. No seeds = no-op, and
// the workdir is not created.
func RunSeeds(m *Manifest, workdir, stateRoot string) error {
	if len(m.Seed) == 0 {
		return nil
	}
	if err := os.MkdirAll(workdir, 0o755); err != nil {
		return fmt.Errorf("seeds: preparing workdir %s: %w", workdir, err)
	}
	for i, cmd := range m.Seed {
		c := exec.Command("sh", "-c", cmd)
		c.Dir = workdir
		c.Env = append(os.Environ(), "WORKDIR="+workdir, "STATE_ROOT="+stateRoot)
		out, err := c.CombinedOutput()
		if err != nil {
			return fmt.Errorf("seed %d (%s): %w\n%s", i, cmd, err, out)
		}
	}
	return nil
}
