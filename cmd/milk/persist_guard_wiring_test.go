package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

// TestSaveWiring_GuardRefusalAndWizardForce pins #404's consent rule where
// cmd/milk meets the config package: after a failed load, the ordinary save
// every slash command uses is refused, while the init wizard's forced path
// writes and clears the guard. Isolated HOME: the guard is process-global
// state and must not leak into tests sharing the suite sandbox.
func TestSaveWiring_GuardRefusalAndWizardForce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	config.ResetLoadFailedState()
	t.Cleanup(config.ResetLoadFailedState)

	dir, err := config.Dir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	corrupt := filepath.Join(dir, "config.json")
	if err := os.WriteFile(corrupt, []byte(`{not json`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadFrom(corrupt); err == nil {
		t.Fatal("expected the corrupt fixture to fail its load")
	}
	if !config.LoadFailed() {
		t.Fatal("guard did not arm")
	}

	// The path behind /colorize, /theme, /agent … — refused after a failed load.
	if err := saveLocalOrGlobal(config.Config{Agent: "x"}, ""); !errors.Is(err, config.ErrPersistRefused) {
		t.Fatalf("saveLocalOrGlobal after failed load = %v, want ErrPersistRefused", err)
	}

	// The init wizard's commit — writes and clears the guard.
	if err := saveLocalOrGlobalForced(config.Config{Agent: "rebuilt"}, ""); err != nil {
		t.Fatalf("saveLocalOrGlobalForced: %v", err)
	}
	if config.LoadFailed() {
		t.Error("forced wizard save must clear the persist guard")
	}
	if err := saveLocalOrGlobal(config.Config{Agent: "normal"}, ""); err != nil {
		t.Errorf("ordinary save after recovery: %v", err)
	}
}
