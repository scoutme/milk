package testutil

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
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
	// Before the HOME override: the Go toolchain's caches must stay on their
	// real paths — a child `go build` deriving them from the sandboxed HOME
	// downloads a READ-ONLY module cache into the sandbox, which RemoveAll
	// can never unlink (see forwardToolchainCaches).
	forwardToolchainCaches()
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

// forwardToolchainCaches pins GOMODCACHE and GOCACHE to what the Go
// toolchain resolves from the CURRENT (pre-sandbox) environment, before any
// HOME override. Without it a test that spawns `go build` derives its caches
// from the sandboxed HOME (CI sets no GOPATH — the PR #411 failure): the
// module cache lands inside the sandbox, Go writes it READ-ONLY (0555 dirs),
// os.RemoveAll can never unlink it, the drain retries a doomed removal until
// timeout and TestMain FATALs — reproduced locally with `env -u GOPATH`.
// Both caches are content-addressed and safe to share with the real
// toolchain; CI's setup-go warm cache becomes usable instead of
// re-downloaded. Idempotent (env already set = no-op); a missing `go`
// binary is a no-op — tests needing it gate on availability.
func forwardToolchainCaches() {
	if os.Getenv("GOMODCACHE") != "" && os.Getenv("GOCACHE") != "" {
		return
	}
	out, err := exec.Command("go", "env", "GOMODCACHE", "GOCACHE").Output()
	if err != nil {
		return
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 2 {
		return
	}
	if v := strings.TrimSpace(lines[0]); v != "" && os.Getenv("GOMODCACHE") == "" {
		_ = os.Setenv("GOMODCACHE", v)
	}
	if v := strings.TrimSpace(lines[1]); v != "" && os.Getenv("GOCACHE") == "" {
		_ = os.Setenv("GOCACHE", v)
	}
}

// drainRemoveAll removes dir, retrying while late writers race the unlink,
// until timeout. On success it reports nil even if a straggler recreates the
// tree afterwards (that leaves a temp dir, never a test failure — no
// competing cleanup exists for sandbox homes we created ourselves). On
// timeout it returns an error naming the leftovers so the next report
// identifies its culprit instead of quoting testing's bare "directory not
// empty". Permission-blocked trees (read-only module-cache dirs) get rm -rf
// semantics via makeRemovable instead of timing out on a doomed loop.
func drainRemoveAll(dir string, timeout time.Duration) error {
	return drainRemoveAllWith(dir, timeout, os.RemoveAll)
}

// drainRemoveAllWith is the testable seam: same loop, injectable remover —
// the timeout contract is pinned against a persistent permission error
// instead of a chmod-defeatable fixture (makeRemovable would undo read-only
// dirs, and the real world has no owner-unremovable file a test can make).
func drainRemoveAllWith(dir string, timeout time.Duration, remove func(string) error) error {
	deadline := time.Now().Add(timeout)
	for {
		err := remove(dir)
		if err == nil {
			return nil
		}
		if errors.Is(err, fs.ErrPermission) {
			// Read-only content (the go module cache's 0555 dirs): make the
			// tree owner-removable and retry once — this is the CI incident's
			// exact shape, and it must succeed, not time out.
			makeRemovable(dir)
			if err = remove(dir); err == nil {
				return nil
			}
		}
		if !time.Now().Before(deadline) {
			return fmt.Errorf("%s still receiving writes after %v (a writer outlived its test): %w; leftovers: %s",
				dir, timeout, err, describeTree(dir, 8))
		}
		time.Sleep(drainInterval)
	}
}

// makeRemovable grants the owner write bits throughout the tree (dirs +0700,
// files +0200), mirroring rm -rf semantics for content created read-only.
// Best-effort: a tree that still cannot be removed keeps draining and
// reports its leftovers as before.
func makeRemovable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d == nil {
			return nil
		}
		// Type() + the needed bits: lstat-shaped, no per-entry Info() stat,
		// and the exact final mode does not matter — the tree is leaving.
		if d.IsDir() {
			_ = os.Chmod(p, d.Type()|0o700)
		} else {
			_ = os.Chmod(p, d.Type()|0o200)
		}
		return nil
	})
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
