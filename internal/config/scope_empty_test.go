package config

// Tests for SaveEmptyConfigIn — the no-wizard bootstrap for a project-local
// override (ADR-0045 §4a): a minimal empty object at the chosen scope, never
// an overwrite.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveEmptyConfigIn_Local(t *testing.T) {
	dir := t.TempDir()
	path, err := SaveEmptyConfigIn("local", dir)
	if err != nil {
		t.Fatalf("SaveEmptyConfigIn(local) = %v", err)
	}
	want := filepath.Join(dir, LocalConfigDir, "config.json")
	if path != want {
		t.Errorf("path = %q, want %q", path, want)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the created config: %v", err)
	}
	if string(data) != "{}\n" {
		t.Errorf("content = %q, want the minimal empty object", data)
	}

	// A bootstrap creates; it never overwrites.
	if _, err := SaveEmptyConfigIn("local", dir); !errors.Is(err, ErrConfigFileExists) {
		t.Errorf("second call = %v, want ErrConfigFileExists", err)
	}
}

func TestSaveEmptyConfigIn_Global(t *testing.T) {
	prev := SetRoot(t.TempDir())
	defer SetRoot(prev)

	path, err := SaveEmptyConfigIn("global", "")
	if err != nil {
		t.Fatalf("SaveEmptyConfigIn(global) = %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join("config.json")) {
		t.Errorf("path = %q, want the state root's config.json", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading the created config: %v", err)
	}
	if string(data) != "{}\n" {
		t.Errorf("content = %q, want the minimal empty object", data)
	}
	if _, err := SaveEmptyConfigIn("global", ""); !errors.Is(err, ErrConfigFileExists) {
		t.Errorf("second call = %v, want ErrConfigFileExists", err)
	}
}
