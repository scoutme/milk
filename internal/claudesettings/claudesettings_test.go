package claudesettings

import (
	"os"
	"path/filepath"
	"testing"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	return &Store{path: path}, path
}

func TestAllowTool_RoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AllowTool("Write"); err != nil {
		t.Fatal(err)
	}
	tools, err := s.AllowedTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0] != "Write" {
		t.Errorf("want [Write], got %v", tools)
	}
}

func TestAllowTool_Idempotent(t *testing.T) {
	s, _ := newTestStore(t)
	s.AllowTool("Bash") //nolint:errcheck
	s.AllowTool("Bash") //nolint:errcheck
	tools, _ := s.AllowedTools()
	if len(tools) != 1 {
		t.Errorf("want 1 tool, got %d", len(tools))
	}
}

func TestIsToolAllowed(t *testing.T) {
	s, _ := newTestStore(t)
	s.AllowTool("Read") //nolint:errcheck

	ok, err := s.IsToolAllowed("Read")
	if err != nil || !ok {
		t.Errorf("want true, got %v %v", ok, err)
	}
	ok, err = s.IsToolAllowed("Write")
	if err != nil || ok {
		t.Errorf("want false, got %v %v", ok, err)
	}
}

func TestAllowDirectory_RoundTrip(t *testing.T) {
	s, _ := newTestStore(t)
	if err := s.AllowDirectory("/home/user/project"); err != nil {
		t.Fatal(err)
	}
	dirs, err := s.AllowedDirectories()
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0] != "/home/user/project" {
		t.Errorf("want [/home/user/project], got %v", dirs)
	}
	tools, _ := s.AllowedTools()
	// AllowDirectory writes Read and Bash glob entries, not tool-wildcard entries.
	// Those are not returned by AllowedTools (which only matches "Name(**)").
	for _, tool := range tools {
		if tool == "Read" || tool == "Bash" {
			t.Errorf("AllowedTools should not return dir-scoped glob entries, got %q", tool)
		}
	}
}

func TestAllowDirectory_Idempotent(t *testing.T) {
	s, _ := newTestStore(t)
	s.AllowDirectory("/tmp/foo") //nolint:errcheck
	s.AllowDirectory("/tmp/foo") //nolint:errcheck
	dirs, _ := s.AllowedDirectories()
	if len(dirs) != 1 {
		t.Errorf("want 1 dir, got %d", len(dirs))
	}
}

func TestStore_CreatesFileOnFirstWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "settings.json")
	s := &Store{path: path}
	if err := s.AllowTool("Write"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("settings file not created: %v", err)
	}
}

func TestStore_EmptyWhenFileAbsent(t *testing.T) {
	s, _ := newTestStore(t)
	tools, err := s.AllowedTools()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 0 {
		t.Errorf("want empty, got %v", tools)
	}
}
