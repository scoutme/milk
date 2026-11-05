package testutil

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSandboxStateRoot_RedirectsAndCanaries pins the #404 test-hygiene
// contract: the state root is redirected away from the real one, a clean run
// passes the canary, and both a modified and a deleted real config are
// reported instead of passing silently.
func TestSandboxStateRoot_RedirectsAndCanaries(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("USERPROFILE", fakeHome)
	t.Setenv("MILK_CONFIG_ROOT", "")

	realCfg := filepath.Join(fakeHome, ".milk", "config.json")
	if err := os.MkdirAll(filepath.Dir(realCfg), 0o700); err != nil {
		t.Fatal(err)
	}
	good := []byte(`{"agents":[{"name":"a"}]}`)
	if err := os.WriteFile(realCfg, good, 0o600); err != nil {
		t.Fatal(err)
	}

	canary := SandboxStateRoot()

	if got := os.Getenv("HOME"); got == fakeHome || got == "" {
		t.Fatalf("HOME not redirected: %q", got)
	}
	if got := os.Getenv("MILK_CONFIG_ROOT"); got != "" {
		t.Fatalf("MILK_CONFIG_ROOT = %q, want cleared", got)
	}
	// The sandbox home must be rc-complete: a zsh child would otherwise open
	// newuser-install and swallow harness launch keystrokes (#404 forensics).
	if rc := filepath.Join(os.Getenv("HOME"), ".zshrc"); func() bool {
		_, err := os.Stat(rc)
		return err != nil
	}() {
		t.Errorf("sandbox HOME has no .zshrc: %s", rc)
	}

	// Clean run: the untouched real config passes.
	if err := canary(); err != nil {
		t.Fatalf("clean canary: %v", err)
	}

	// A test writing the real config must fail the canary loudly.
	wiped := []byte(`{}`)
	if err := os.WriteFile(realCfg, wiped, 0o600); err != nil {
		t.Fatal(err)
	}
	err := canary()
	if err == nil {
		t.Fatal("canary passed after the real config was modified")
	}
	if !strings.Contains(err.Error(), "CHANGED") {
		t.Errorf("canary error = %v, want a CHANGED report", err)
	}

	// Deletion is its own loud failure.
	if err := os.WriteFile(realCfg, good, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := canary(); err != nil { // back to good → pass again
		t.Fatalf("restored config canary: %v", err)
	}
	if err := os.Remove(realCfg); err != nil {
		t.Fatal(err)
	}
	err = canary()
	if err == nil || !strings.Contains(err.Error(), "DELETED") {
		t.Fatalf("canary after delete = %v, want a DELETED report", err)
	}
}

// TestEnsureShellRC pins the rc-completeness helper's contract: create when
// absent, never touch an existing file, tolerate an empty home.
func TestEnsureShellRC(t *testing.T) {
	home := t.TempDir()
	if err := EnsureShellRC(home); err != nil {
		t.Fatalf("EnsureShellRC: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatalf("reading created .zshrc: %v", err)
	}
	if !strings.Contains(string(data), "harness") {
		t.Errorf(".zshrc content = %q, want a harness stub", data)
	}

	// Existing file: untouched, byte for byte.
	sentinel := []byte("# my own rc\necho custom\n")
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), sentinel, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := EnsureShellRC(home); err != nil {
		t.Fatalf("EnsureShellRC over existing: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(home, ".zshrc"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(sentinel) {
		t.Errorf("existing .zshrc modified: %q", got)
	}

	if err := EnsureShellRC(""); err != nil {
		t.Errorf("empty home = %v, want nil", err)
	}
}
