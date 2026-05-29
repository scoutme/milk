package session

import (
	"os"
	"path/filepath"
	"testing"
)

// overrideHome redirects the milk data dir to a temp directory for the test.
func overrideHome(t *testing.T) func() {
	t.Helper()
	tmp := t.TempDir()
	orig := os.Getenv("HOME")
	os.Setenv("HOME", tmp)
	return func() { os.Setenv("HOME", orig) }
}

func TestNew_CreatesSessionAndIndex(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s, err := New("/proj/foo", "my-session")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Error("expected non-empty session ID")
	}
	if s.State != StateRouting {
		t.Errorf("expected initial state ROUTING, got %s", s.State)
	}

	// Session file must exist
	dir := filepath.Join(os.Getenv("HOME"), ".milk", "sessions")
	if _, err := os.Stat(filepath.Join(dir, s.ID+".json")); err != nil {
		t.Errorf("session file missing: %v", err)
	}
	// Index must exist
	if _, err := os.Stat(filepath.Join(dir, "index.json")); err != nil {
		t.Errorf("index file missing: %v", err)
	}
}

func TestLoadRoundtrip(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s, err := New("/proj/bar", "roundtrip")
	if err != nil {
		t.Fatal(err)
	}
	s.AddTurn(Turn{Role: RoleUser, Content: "hello"})
	s.EscalationSessionID = "claude-abc"
	if err := Save(s); err != nil {
		t.Fatal(err)
	}

	got, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.EscalationSessionID != "claude-abc" {
		t.Errorf("claude_session_id: want claude-abc got %s", got.EscalationSessionID)
	}
	if len(got.History) != 1 {
		t.Errorf("history length: want 1 got %d", len(got.History))
	}
}

func TestResume_ReturnsLatestForCWD(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s1, _ := New("/proj/cwd", "first")
	s2, _ := New("/proj/cwd", "second")

	resumed, err := Resume("/proj/cwd", "")
	if err != nil {
		t.Fatal(err)
	}
	// Most recently created session should be returned
	if resumed.ID != s2.ID {
		t.Errorf("expected most recent session %s, got %s (s1=%s)", s2.ID, resumed.ID, s1.ID)
	}
}

func TestResume_ByName(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	New("/proj/named", "alpha")
	New("/proj/named", "beta")

	resumed, err := Resume("/proj/named", "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Name != "alpha" {
		t.Errorf("expected session named alpha, got %s", resumed.Name)
	}
}

func TestResume_CreatesNewWhenNoneExist(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s, err := Resume("/proj/fresh", "")
	if err != nil {
		t.Fatal(err)
	}
	if s.ID == "" {
		t.Error("expected new session to be created")
	}
}

func TestDrop_RemovesFromIndexAndFile(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s, _ := New("/proj/drop", "")
	if err := Drop(s.ID, "/proj/drop"); err != nil {
		t.Fatal(err)
	}

	entries, err := List("/proj/drop")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["/proj/drop"]) != 0 {
		t.Errorf("expected empty list after drop, got %d entries", len(entries["/proj/drop"]))
	}

	dir := filepath.Join(os.Getenv("HOME"), ".milk", "sessions")
	if _, err := os.Stat(filepath.Join(dir, s.ID+".json")); !os.IsNotExist(err) {
		t.Error("session file should have been deleted")
	}
}

func TestList_FiltersAndRepairs(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	New("/proj/list", "s1")
	New("/proj/list", "s2")
	New("/proj/other", "s3")

	entries, err := List("/proj/list")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries["/proj/list"]) != 2 {
		t.Errorf("expected 2 sessions for /proj/list, got %d", len(entries["/proj/list"]))
	}
	if _, ok := entries["/proj/other"]; ok {
		t.Error("List with cwd filter should not return other directories")
	}
}

func TestRepairIndex_RemovesOrphans(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s, _ := New("/proj/repair", "orphan")

	// Delete the session file directly to create an orphan index entry
	home := os.Getenv("HOME")
	os.Remove(filepath.Join(home, ".milk", "sessions", s.ID+".json"))

	// Resume should trigger repair and create a fresh session
	fresh, err := Resume("/proj/repair", "")
	if err != nil {
		t.Fatal(err)
	}
	if fresh.ID == s.ID {
		t.Error("expected a new session after orphan repair, got the deleted one")
	}
}
