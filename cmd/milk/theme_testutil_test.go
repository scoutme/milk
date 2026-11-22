package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/scoutme/milk/internal/theme"
)

// Test helpers for the theme registry: write a drop-in theme file and point
// the package registry at sandbox dirs (restored after the test).

func writeTestTheme(t *testing.T, dir, file, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func loadTestRegistry(t *testing.T, userDir, projectDir string) {
	t.Helper()
	old := themeRegistry
	t.Cleanup(func() { themeRegistry = old })
	themeRegistry = theme.New()
	themeRegistry.Load(userDir, projectDir)
}
