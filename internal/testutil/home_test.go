package testutil

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSandboxHome_SetsEnvAndDrains pins the helper's two halves: the env
// points at the returned dir while the test runs, and the dir is GONE when
// the test's cleanups have run (child subtest returns → its cleanup ran).
func TestSandboxHome_SetsEnvAndDrains(t *testing.T) {
	var dir string
	t.Run("child", func(t *testing.T) {
		dir = SandboxHome(t)
		if got := os.Getenv("HOME"); got != dir {
			t.Errorf("HOME = %q, want %q", got, dir)
		}
		if got := os.Getenv("USERPROFILE"); got != dir {
			t.Errorf("USERPROFILE = %q, want %q", got, dir)
		}
		if err := os.WriteFile(filepath.Join(dir, ".milk", "marker"), nil, 0o600); err != nil && !errors.Is(err, fs.ErrNotExist) {
			if mkerr := os.MkdirAll(filepath.Join(dir, ".milk"), 0o700); mkerr != nil {
				t.Fatalf("MkdirAll: %v", mkerr)
			}
			if err := os.WriteFile(filepath.Join(dir, ".milk", "marker"), []byte("x"), 0o600); err != nil {
				t.Fatalf("WriteFile: %v", err)
			}
		}
	})
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("sandbox home %s survived cleanup (stat err = %v), want removed", dir, err)
	}
}

// TestDrainRemoveAll_AbsorbsLateWriter is the flake regression pin: a writer
// still creating files when cleanup starts must NOT fail the removal — the
// drain waits it out, the tree ends gone.
func TestDrainRemoveAll_AbsorbsLateWriter(t *testing.T) {
	dir := t.TempDir() // direct dir: drainRemoveAll is what's under test
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; ; i++ {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.WriteFile(filepath.Join(dir, "late.bin"), []byte("late write"), 0o600)
			if i >= 4 { // keeps racing cleanup for ~200ms, then stops
				close(stop)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	if err := drainRemoveAll(dir, 3*time.Second); err != nil {
		t.Fatalf("late writer should have been drained, got: %v", err)
	}
	<-done
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dir survived drain (stat err = %v), want removed", err)
	}
}

// TestDrainRemoveAll_TimesOutListingLeftovers pins the loud-failure contract:
// when removal cannot succeed, drain waits its timeout and returns an error
// that NAMES the leftover file, so a future occurrence identifies its culprit
// instead of quoting testing's opaque "directory not empty". The failure is
// made deterministic (a read-only subdir cannot be unlinked from) — the
// concurrent-write race itself is pinned by the absorb test above.
func TestDrainRemoveAll_TimesOutListingLeftovers(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "keeper")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keeper.txt"), []byte("forever"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0o500); err != nil { // r+x: entries listed, not unlinked
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(sub, 0o700) }) // let TempDir's own cleanup proceed

	err := drainRemoveAll(dir, 300*time.Millisecond)
	if err == nil {
		t.Fatal("unremovable tree: drainRemoveAll = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "outlived its test") {
		t.Errorf("error %q misses the outlived-its-test diagnosis", err)
	}
	if !strings.Contains(err.Error(), "keeper.txt") {
		t.Errorf("error %q does not name the leftover file, want keeper.txt", err)
	}
}

// TestDrainRemoveAll_MissingDirIsNil: draining an already-gone tree (second
// canary() call after the first drained) is a no-op success.
func TestDrainRemoveAll_MissingDirIsNil(t *testing.T) {
	if err := drainRemoveAll(filepath.Join(t.TempDir(), "never-created"), time.Second); err != nil {
		t.Errorf("missing dir = %v, want nil", err)
	}
}
