package session

import (
	"os"
	"path/filepath"
	"testing"
	"time"
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

func TestLoad_TimeBasedNeedExpiry(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	s, _ := New("/proj/expiry", "test")
	s.CurrentNeed = "deploy the app"
	s.CurrentNeedSetAt = 1
	s.CurrentNeedUpdatedAt = time.Now().Add(-48 * time.Hour) // 2 days ago
	if err := Save(s); err != nil {
		t.Fatal(err)
	}

	// Default expiry is 24h — need should be cleared on load.
	loaded, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentNeed != "" {
		t.Errorf("expected CurrentNeed cleared after 48h, got %q", loaded.CurrentNeed)
	}
}

func TestLoad_TimeBasedNeedExpiry_Disabled(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	// Disable expiry (0 = never expire).
	old := NeedExpiryDuration
	NeedExpiryDuration = 0
	defer func() { NeedExpiryDuration = old }()

	s, _ := New("/proj/noexpiry", "test")
	s.CurrentNeed = "deploy the app"
	s.CurrentNeedSetAt = 1
	s.CurrentNeedUpdatedAt = time.Now().Add(-48 * time.Hour)
	if err := Save(s); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.CurrentNeed != "deploy the app" {
		t.Errorf("expected CurrentNeed preserved when expiry disabled, got %q", loaded.CurrentNeed)
	}
}

func TestLookup_ByIDPrefix(t *testing.T) {
	restore := overrideHome(t)
	defer restore()

	a, _ := New("/proj/one", "")
	b, _ := New("/proj/two", "")

	// Exact ID wins outright…
	got, err := Lookup(a.ID)
	if err != nil || got.ID != a.ID {
		t.Fatalf("Lookup(exact) = %v, %v; want %s", got, err, a.ID)
	}
	// …and an unambiguous prefix resolves.
	got, err = Lookup(a.ID[:8])
	if err != nil || got.ID != a.ID {
		t.Fatalf("Lookup(prefix) = %v, %v; want %s", got, err, a.ID)
	}

	// A prefix matching more than one session is an error, never a guess.
	s1 := &Session{ID: "abcd-1", CWD: "/proj/amb", History: []Turn{}, LastUsed: time.Now(), CreatedAt: time.Now()}
	s2 := &Session{ID: "abcd-2", CWD: "/proj/amb", History: []Turn{}, LastUsed: time.Now(), CreatedAt: time.Now()}
	if err := Save(s1); err != nil {
		t.Fatal(err)
	}
	if err := Save(s2); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup("abcd"); err == nil {
		t.Error("Lookup(ambiguous) = nil error, want ambiguity error")
	}
	// Exact IDs still resolve even when their prefixes are ambiguous.
	if got, err := Lookup("abcd-2"); err != nil || got.ID != "abcd-2" {
		t.Fatalf("Lookup(exact amid ambiguity) = %v, %v; want abcd-2", got, err)
	}

	if _, err := Lookup("no-such-session"); err == nil {
		t.Error("Lookup(miss) = nil error, want not-found")
	}
	if _, err := Lookup(""); err == nil {
		t.Error("Lookup(\"\") = nil error, want error")
	}
	_ = b
}
