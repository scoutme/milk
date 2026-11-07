package testutil

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Tunables as vars so tests can shrink them without sleeping on defaults.
var (
	// drainTimeout is how long DrainRemoveAll tolerates late writers before
	// failing loudly. Observed lates land within milliseconds-to-seconds of
	// test end; 5s is generous headroom for loaded runners.
	drainTimeout = 5 * time.Second
	// drainInterval is the retry pause between RemoveAll attempts.
	drainInterval = 50 * time.Millisecond
)

// SandboxHome gives ONE test its own HOME (and USERPROFILE, mirroring
// SandboxStateRoot's process-wide redirect so Windows resolutions follow the
// same dir) and registers a drain-aware removal — replacing the bare
// `t.Setenv("HOME", t.TempDir())` pattern, whose TempDir cleanup fails
// whenever a late writer is mid-flight (the CI flake family: a turn or
// background-job completion handler writing state after its test returned
// — TestACPStreaming_BackgroundJobOutputV2AndV1, TestHandleBusyKey_
// EnterDoesNotSpawn, one of them twice). Returns the dir for tests that use
// it as a path. Env restores at test end via t.Setenv's own cleanup;
// ordering with other cleanups is LIFO (registered here runs before
// cleanups registered earlier — writers that run in those still get drained).
// Known limit: a writer bound to env at write time can still land under
// ANOTHER test's HOME; the drain absorbs it into the victim's cleanup —
// bounding writers to their spawn-time root is the larger refactor (ADR-0101).
//
// MILK_CONFIG_ROOT is deliberately NOT touched: SandboxStateRoot already
// leaves it empty (config falls through to HOME), and a test that sets it
// explicitly keeps its value regardless of call order.
func SandboxHome(t *testing.T) string {
	t.Helper()
	tmp, err := os.MkdirTemp("", "milk-test-home-")
	if err != nil {
		t.Fatalf("testutil: temp home: %v", err)
	}
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Cleanup(func() {
		if err := drainRemoveAll(tmp, drainTimeout); err != nil {
			t.Errorf("test home cleanup: %v", err)
		}
	})
	return tmp
}

// drainRemoveAll removes dir, retrying while late writers race the unlink,
// until timeout. On success it reports nil even if a straggler recreates the
// tree afterwards (that leaves a temp dir, never a test failure — no
// competing cleanup exists for sandbox homes we created ourselves). On
// timeout it returns an error naming the leftovers so the next report
// identifies its culprit instead of quoting testing's bare "directory not
// empty".
func drainRemoveAll(dir string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		err := os.RemoveAll(dir)
		if err == nil {
			return nil
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s still receiving writes after %v (a writer outlived its test): %w; leftovers: %s",
				dir, timeout, err, describeTree(dir, 8))
		}
		time.Sleep(drainInterval)
	}
}

// describeTree lists up to max relative paths under dir for diagnostics.
func describeTree(dir string, max int) string {
	var shown []string
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil // a vanishing tree is fine; the error carries it
		}
		if len(shown) >= max {
			return filepath.SkipAll
		}
		if rel, rerr := filepath.Rel(dir, path); rerr == nil {
			shown = append(shown, rel)
		}
		return nil
	})
	if len(shown) == 0 {
		return "(tree vanished)"
	}
	return strings.Join(shown, ", ")
}
