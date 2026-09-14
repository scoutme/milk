package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// withSandboxedConfigDirs isolates both the global config dir (~/.milk via
// HOME) and the local config dir (.milk in cwd) for a test, restoring both
// on cleanup. Mirrors the pattern used by internal/config's own tests
// (t.Setenv("HOME", ...) + os.Chdir into a temp dir).
func withSandboxedConfigDirs(t *testing.T) (home, cwd string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)

	cwd = t.TempDir()
	orig, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	if err := os.Chdir(cwd); err != nil {
		t.Fatalf("Chdir: %v", err)
	}
	t.Cleanup(func() { os.Chdir(orig) })
	return home, cwd
}

func TestPreferredSaveScope_NoLocalConfig_ReturnsGlobal(t *testing.T) {
	withSandboxedConfigDirs(t)

	if got := preferredSaveScope(); got != "global" {
		t.Errorf("expected global scope with no local config, got %q", got)
	}
}

func TestPreferredSaveScope_WithLocalConfig_ReturnsLocal(t *testing.T) {
	_, cwd := withSandboxedConfigDirs(t)

	localDir := filepath.Join(cwd, config.LocalConfigDir)
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if got := preferredSaveScope(); got != "local" {
		t.Errorf("expected local scope with a local config present, got %q", got)
	}
}

func TestSaveLocalOrGlobal_NoLocalConfig_WritesGlobal(t *testing.T) {
	home, cwd := withSandboxedConfigDirs(t)

	if err := saveLocalOrGlobal(config.Config{Agent: "test-agent"}); err != nil {
		t.Fatalf("saveLocalOrGlobal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".milk", "config.json")); err != nil {
		t.Errorf("expected global config.json to be written, got: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".milk", "config.json")); err == nil {
		t.Error("did not expect a local config.json to be created")
	}
}

func TestSaveLocalOrGlobal_WithLocalConfig_WritesLocal(t *testing.T) {
	home, cwd := withSandboxedConfigDirs(t)

	localDir := filepath.Join(cwd, config.LocalConfigDir)
	if err := os.MkdirAll(localDir, 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(filepath.Join(localDir, "config.json"), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := saveLocalOrGlobal(config.Config{Agent: "test-agent"}); err != nil {
		t.Fatalf("saveLocalOrGlobal: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, ".milk", "config.json"))
	if err != nil {
		t.Fatalf("expected local config.json to be written: %v", err)
	}
	if !strings.Contains(string(data), `"test-agent"`) {
		t.Errorf("expected local config.json to contain the saved agent name, got %q", data)
	}
	if _, err := os.Stat(filepath.Join(home, ".milk", "config.json")); err == nil {
		t.Error("did not expect the global config.json to be written")
	}
}

// TestSaveConfigForScope_GlobalScope_DoesNotCreateLocalConfig is a regression
// test for a bug live-testing caught: every CLI config-mutating command
// called ensureLocalConfig() unconditionally after resolving scope, so even
// an explicit --global save created a stray .milk/config.json in cwd — which
// then silently flipped every later command's scope default to "local".
// saveConfigForScope must only create the local file when scope == "local".
func TestSaveConfigForScope_GlobalScope_DoesNotCreateLocalConfig(t *testing.T) {
	home, cwd := withSandboxedConfigDirs(t)

	if err := saveConfigForScope(config.Config{Agent: "global-agent"}, "global"); err != nil {
		t.Fatalf("saveConfigForScope: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cwd, ".milk")); err == nil {
		t.Error("a --global save must not create .milk/ in the current directory")
	}
	data, err := os.ReadFile(filepath.Join(home, ".milk", "config.json"))
	if err != nil {
		t.Fatalf("expected global config.json to be written: %v", err)
	}
	if !strings.Contains(string(data), `"global-agent"`) {
		t.Errorf("expected global config.json to contain the saved agent name, got %q", data)
	}
}

func TestSaveConfigForScope_LocalScope_CreatesLocalConfig(t *testing.T) {
	_, cwd := withSandboxedConfigDirs(t)

	if err := saveConfigForScope(config.Config{Agent: "local-agent"}, "local"); err != nil {
		t.Fatalf("saveConfigForScope: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(cwd, ".milk", "config.json"))
	if err != nil {
		t.Fatalf("expected local config.json to be written: %v", err)
	}
	if !strings.Contains(string(data), `"local-agent"`) {
		t.Errorf("expected local config.json to contain the saved agent name, got %q", data)
	}
}
