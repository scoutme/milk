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
// instead of quoting testing's opaque "directory not empty". Driven through
// the seam with a persistent permission error: the read-only-dir fixture
// would now be chmod'd into success by makeRemovable (by design), and no
// owner-created file can stay unremovable — a live writer instead races the
// loop to a premature nil (the absorb test above pins that side).
func TestDrainRemoveAll_TimesOutListingLeftovers(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "keeper")
	if err := os.Mkdir(sub, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "keeper.txt"), []byte("forever"), 0o600); err != nil {
		t.Fatal(err)
	}

	err := drainRemoveAllWith(dir, 300*time.Millisecond, func(string) error {
		return fs.ErrPermission // the doomed-loop condition, injected
	})
	if err == nil {
		t.Fatal("failing remover: drainRemoveAllWith = nil, want a timeout error")
	}
	if !strings.Contains(err.Error(), "outlived its test") {
		t.Errorf("error %q misses the outlived-its-test diagnosis", err)
	}
	if !strings.Contains(err.Error(), "keeper.txt") {
		t.Errorf("error %q does not name the leftover file, want keeper.txt", err)
	}
}

// TestDrainRemoveAll_ChmodsReadOnlyTree is the PR #411 CI incident as a
// fixture: a child `go build` downloaded the module cache inside the sandbox
// (read-only 0555 dirs, 0444 files), os.RemoveAll got EACCES forever, and
// TestMain FATALed after the 5s drain. rm -rf semantics must absorb it.
func TestDrainRemoveAll_ChmodsReadOnlyTree(t *testing.T) {
	dir := t.TempDir()
	mod := filepath.Join(dir, "go", "pkg", "mod", "x@v1.0.0")
	if err := os.MkdirAll(mod, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mod, "file.go"), []byte("package x"), 0o644); err != nil {
		t.Fatal(err)
	}
	// The module cache's real shape: dirs made non-writable after writing.
	if err := os.Chmod(mod, 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "go", "pkg", "mod"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "go", "pkg"), 0o555); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(filepath.Join(dir, "go"), 0o555); err != nil {
		t.Fatal(err)
	}

	if err := drainRemoveAll(dir, 2*time.Second); err != nil {
		t.Fatalf("read-only module-cache tree should drain with rm -rf semantics, got: %v", err)
	}
	if _, err := os.Stat(dir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("dir survived drain (stat err = %v), want removed", err)
	}
}

// TestForwardToolchainCaches_KeepsCachesOutOfSandbox: exactly CI's condition
// (no GOMODCACHE/GOCACHE in the env — PR #411 set no GOPATH either), before
// any HOME override — both caches resolve outside the sandbox home, and a
// second call with env already set is a no-op.
func TestForwardToolchainCaches_KeepsCachesOutOfSandbox(t *testing.T) {
	for _, k := range []string{"GOMODCACHE", "GOCACHE"} {
		if v, had := os.LookupEnv(k); had {
			k, v, had := k, v, had
			t.Cleanup(func() { _ = os.Setenv(k, v); _ = had })
		}
		_ = os.Unsetenv(k)
	}
	var home string
	t.Run("child", func(t *testing.T) {
		home = SandboxHome(t) // forwards BEFORE it overrides HOME
	})
	if home == "" {
		t.Fatal("no sandbox home")
	}
	gomod, gocache := os.Getenv("GOMODCACHE"), os.Getenv("GOCACHE")
	if gomod == "" || gocache == "" {
		t.Fatalf("caches not forwarded: GOMODCACHE=%q GOCACHE=%q", gomod, gocache)
	}
	for name, v := range map[string]string{"GOMODCACHE": gomod, "GOCACHE": gocache} {
		if strings.HasPrefix(v, home) {
			t.Errorf("%s = %q landed INSIDE the sandbox home %q", name, v, home)
		}
	}
	// Idempotence: env already set → early return, sentinel survives.
	_ = os.Setenv("GOMODCACHE", "/sentinel-keep")
	forwardToolchainCaches()
	if got := os.Getenv("GOMODCACHE"); got != "/sentinel-keep" {
		t.Errorf("second forward overwrote the env: %q", got)
	}
}

// TestDrainRemoveAll_MissingDirIsNil: draining an already-gone tree (second
// canary() call after the first drained) is a no-op success.
func TestDrainRemoveAll_MissingDirIsNil(t *testing.T) {
	if err := drainRemoveAll(filepath.Join(t.TempDir(), "never-created"), time.Second); err != nil {
		t.Errorf("missing dir = %v, want nil", err)
	}
}
