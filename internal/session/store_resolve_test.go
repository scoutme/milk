package session

import (
	"errors"
	"testing"
	"time"
)

// saveWithID persists a session with a caller-chosen ID (New mints UUIDs;
// reference resolution tests need stable, prefix-guessable IDs).
func saveWithID(t *testing.T, cwd, id, name string, lastUsed time.Time) *Session {
	t.Helper()
	s := &Session{ID: id, Name: name, CWD: cwd, CreatedAt: lastUsed, LastUsed: lastUsed, State: StateRouting, History: []Turn{}}
	if err := Save(s); err != nil {
		t.Fatalf("Save(%s): %v", id, err)
	}
	return s
}

func wantResolved(t *testing.T, got *Session, wantID string) {
	t.Helper()
	if got == nil {
		t.Fatalf("resolved nil, want session %s", wantID)
	}
	if got.ID != wantID {
		t.Fatalf("resolved %s, want %s", got.ID, wantID)
	}
}

func TestResolveExactIDWins(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	now := time.Now()
	saveWithID(t, "/repo", "abc-1111", "other", now)
	saveWithID(t, "/repo", "abc-2222", "abc-1111", now.Add(-time.Minute)) // name shadows the other ID

	got, err := Resolve("/repo", "abc-1111")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantResolved(t, got, "abc-1111") // exact ID beats the same-named session
}

func TestResolveUniquePrefix(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	saveWithID(t, "/repo", "abcd-1111", "", time.Now())

	got, err := Resolve("/repo", "abcd")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantResolved(t, got, "abcd-1111")
}

func TestResolveAmbiguousPrefix(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	now := time.Now()
	saveWithID(t, "/repo", "abcd-1111", "", now)
	saveWithID(t, "/repo", "abcd-2222", "", now)

	_, err := Resolve("/repo", "abcd")
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
	}
	if amb.Matches != 2 {
		t.Errorf("ambiguous matches = %d, want 2", amb.Matches)
	}
}

func TestResolveNameIsCWDScoped(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	now := time.Now()
	saveWithID(t, "/a", "aaaa-1111", "feature", now)
	saveWithID(t, "/b", "bbbb-2222", "feature", now)

	got, err := Resolve("/a", "feature")
	if err != nil {
		t.Fatalf("Resolve(/a): %v", err)
	}
	wantResolved(t, got, "aaaa-1111")

	if _, err := Resolve("/c", "feature"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve(/c) err = %v, want ErrNotFound", err)
	}
}

func TestResolveIDPrefixBeatsName(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	now := time.Now()
	saveWithID(t, "/repo", "cafe-9999", "", now)
	saveWithID(t, "/repo", "other-1", "cafe", now.Add(time.Minute)) // newer, same-cwd name match

	got, err := Resolve("/repo", "cafe")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantResolved(t, got, "cafe-9999") // ID resolution is exhausted first
}

func TestResolveAmbiguousName(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	now := time.Now()
	saveWithID(t, "/repo", "one-1", "dup", now)
	saveWithID(t, "/repo", "two-2", "dup", now)

	_, err := Resolve("/repo", "dup")
	var amb *AmbiguousError
	if !errors.As(err, &amb) {
		t.Fatalf("err = %v (%T), want *AmbiguousError", err, err)
	}
	if amb.Matches != 2 {
		t.Errorf("ambiguous matches = %d, want 2", amb.Matches)
	}
}

func TestResolveNotFound(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	saveWithID(t, "/repo", "abcd-1111", "named", time.Now())

	for _, ref := range []string{"nope", "xyz", "Named"} {
		if _, err := Resolve("/repo", ref); !errors.Is(err, ErrNotFound) {
			t.Errorf("Resolve(%q) err = %v, want ErrNotFound", ref, err)
		}
	}
}

func TestResolveEmptyRefIsMostRecent(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	now := time.Now()
	saveWithID(t, "/repo", "old-1111", "", now.Add(-time.Hour))
	saveWithID(t, "/repo", "new-2222", "", now)
	saveWithID(t, "/other", "zzz-3333", "", now.Add(time.Hour)) // other cwd never wins

	got, err := Resolve("/repo", "")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	wantResolved(t, got, "new-2222")

	if _, err := Resolve("/empty", ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve on empty cwd err = %v, want ErrNotFound", err)
	}
}

func TestResolveNeverCreates(t *testing.T) {
	restore := overrideHome(t)
	defer restore()
	saveWithID(t, "/repo", "abcd-1111", "", time.Now())

	if _, err := Resolve("/repo", "ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve err = %v, want ErrNotFound", err)
	}
	idx, err := List("/repo")
	if err != nil {
		t.Fatal(err)
	}
	if len(idx["/repo"]) != 1 {
		t.Fatalf("resolution created a session: %v", idx)
	}
}
