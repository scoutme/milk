package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"

	"github.com/scoutme/milk/internal/config"
	milkscripts "github.com/scoutme/milk/scripts"
)

// smolagentScript holds the embedded milk-smolagent Python adapter script.
var smolagentScript = milkscripts.SmolagentScript

// ensureSmolagentScript writes the embedded milk-smolagent script to
// ~/.milk/scripts/milk-smolagent if it is absent or differs from the
// bundled version, then returns the absolute path to the deployed script.
func ensureSmolagentScript() (string, error) {
	dir, err := config.Dir()
	if err != nil {
		return "", fmt.Errorf("ensureSmolagentScript: %w", err)
	}
	scriptsDir := filepath.Join(dir, "scripts")
	dest := filepath.Join(scriptsDir, "milk-smolagent")

	if existing, err := os.ReadFile(dest); err == nil && bytes.Equal(existing, smolagentScript) {
		return dest, nil // already up-to-date
	}

	if err := os.MkdirAll(scriptsDir, 0o755); err != nil {
		return "", fmt.Errorf("ensureSmolagentScript: create dir: %w", err)
	}
	if err := os.WriteFile(dest, smolagentScript, 0o755); err != nil {
		return "", fmt.Errorf("ensureSmolagentScript: write: %w", err)
	}
	return dest, nil
}
