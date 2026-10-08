package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
)

// saveTestSession persists a session with a caller-chosen ID (session.New mints
// UUIDs; ref-resolution tests need stable, prefix-guessable IDs).
func saveTestSession(t *testing.T, cwd, id, name string, turns int, lastUsed time.Time) *session.Session {
	t.Helper()
	hist := make([]session.Turn, 0, turns)
	for i := 0; i < turns; i++ {
		hist = append(hist, session.Turn{Role: session.RoleUser, Content: "hi", Timestamp: lastUsed})
	}
	s := &session.Session{ID: id, Name: name, CWD: cwd, CreatedAt: lastUsed, LastUsed: lastUsed, State: session.StateRouting, History: hist}
	if err := session.Save(s); err != nil {
		t.Fatalf("Save(%s): %v", id, err)
	}
	return s
}

func TestOpenSessionNewNames(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	sess, err := openSession("/repo", true, "feature-x")
	if err != nil {
		t.Fatalf("openSession(--new): %v", err)
	}
	if sess.Name != "feature-x" {
		t.Fatalf("created name = %q, want feature-x", sess.Name)
	}
	// --new with no name: an unnamed session.
	sess2, err := openSession("/repo", true, "")
	if err != nil {
		t.Fatalf("openSession(--new unnamed): %v", err)
	}
	if sess2.Name != "" || sess2.ID == sess.ID {
		t.Fatalf("second session = %s (%q), want fresh unnamed", sess2.ID, sess2.Name)
	}
}

func TestOpenSessionRefTargetsStored(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saveTestSession(t, "/repo", "abcd-1111", "older", 2, time.Now().Add(-time.Hour))
	want := saveTestSession(t, "/repo", "abef-2222", "newer", 0, time.Now())

	for _, ref := range []string{"abef-2222", "abef", "newer"} {
		got, err := openSession("/repo", false, ref)
		if err != nil {
			t.Fatalf("openSession(%q): %v", ref, err)
		}
		if got.ID != want.ID {
			t.Errorf("openSession(%q) = %s, want %s", ref, got.ID, want.ID)
		}
	}
}

func TestOpenSessionUnknownRefIsAnError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saveTestSession(t, "/repo", "abcd-1111", "", 0, time.Now())

	_, err := openSession("/repo", false, "ghost")
	if err == nil {
		t.Fatal("unknown ref must not silently create/resume another session")
	}
	if !strings.Contains(err.Error(), sessionRefCreateHintCLI) {
		t.Errorf("err = %q, want the --new create hint", err)
	}
}

func TestOpenSessionBareResumesMostRecent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saveTestSession(t, "/repo", "old-1111", "", 0, time.Now().Add(-time.Hour))
	want := saveTestSession(t, "/repo", "new-2222", "", 0, time.Now())

	got, err := openSession("/repo", false, "")
	if err != nil {
		t.Fatalf("openSession: %v", err)
	}
	if got.ID != want.ID {
		t.Fatalf("resumed %s, want %s", got.ID, want.ID)
	}

	// A cwd with no sessions still gets one — the REPL's default behavior.
	fresh, err := openSession("/empty", false, "")
	if err != nil {
		t.Fatalf("openSession on empty dir: %v", err)
	}
	if fresh.CWD != "/empty" || len(fresh.History) != 0 {
		t.Fatalf("fresh = %+v, want empty session for /empty", fresh)
	}
}

func TestOpSessionNewResumeDrop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	created := opSessionNew("/repo", "alpha")
	if created.err != nil {
		t.Fatalf("opSessionNew: %v", created.err)
	}
	first := created.bind
	if first == nil || first.Name != "alpha" {
		t.Fatalf("bind = %+v, want named session", created.bind)
	}

	// Resume of the current binding is a report, not a bind.
	same := opSessionResume(first, "/repo", first.ID)
	if same.err != nil || same.bind != nil {
		t.Fatalf("re-resume = %+v, want report-only", same)
	}
	if !strings.Contains(same.out, "already in session") {
		t.Errorf("re-resume out = %q", same.out)
	}

	// Drop a non-current session: deleted only, conversation untouched.
	other := opSessionNew("/repo", "beta").bind
	res := opSessionDrop(first, "/repo", "beta")
	if res.err != nil {
		t.Fatalf("opSessionDrop(beta): %v", res.err)
	}
	if res.bind != nil {
		t.Fatal("/drop <other> must not rebind the host")
	}
	if _, err := session.Resolve("/repo", other.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("dropped session still resolves: %v", err)
	}

	// Drop the current binding: lands on a fresh session and says what died.
	res = opSessionDrop(first, "/repo", "")
	if res.err != nil {
		t.Fatalf("opSessionDrop(current): %v", res.err)
	}
	if res.bind == nil || res.bind.ID == first.ID {
		t.Fatalf("dropping current must bind a fresh session, got %+v", res.bind)
	}
	if !strings.Contains(res.out, first.ID) {
		t.Errorf("drop output hides what it deleted: %q", res.out)
	}
	if _, err := session.Resolve("/repo", first.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("dropped current session still resolves: %v", err)
	}
}

func TestSessionListTextMarksCurrent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saveTestSession(t, "/repo", "abcd-1111", "solo", 3, time.Now())
	saveTestSession(t, "/other", "zzzz-9999", "", 0, time.Now())

	out, err := sessionListText("/repo", "abcd-1111", false)
	if err != nil {
		t.Fatalf("sessionListText: %v", err)
	}
	if strings.Contains(out, "/other") {
		t.Errorf("plain listing leaked other cwds: %q", out)
	}
	if !strings.Contains(out, "* abcd-111") || !strings.Contains(out, "solo") || !strings.Contains(out, "3") {
		t.Errorf("listing missing marker/name/turns: %q", out)
	}

	all, err := sessionListText("/repo", "abcd-1111", true)
	if err != nil {
		t.Fatalf("sessionListText(all): %v", err)
	}
	if !strings.Contains(all, "/other") {
		t.Errorf("--all listing missing other cwds: %q", all)
	}
}

func TestRenderOpResultHints(t *testing.T) {
	out := renderOpResult(sessionOpResult{err: session.ErrNotFound}, "ghost")
	if !strings.Contains(out, "no session matches") || !strings.Contains(out, sessionRefCreateHintHost) {
		t.Errorf("miss rendering = %q", out)
	}
	amb := renderOpResult(sessionOpResult{err: &session.AmbiguousError{Ref: "ab", Matches: 3}}, "ab")
	if !strings.Contains(amb, "ambiguous") || strings.Contains(amb, sessionRefCreateHintHost) {
		t.Errorf("ambiguity rendering = %q", amb)
	}
}

func TestSessionListTextEmptyCWDGroup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	saveTestSession(t, "/other", "zzzz-9999", "", 0, time.Now())

	// session.List returns the queried cwd as an empty group — the listing
	// must still say "no sessions found", not render nothing.
	out, err := sessionListText("/nothing", "", false)
	if err != nil {
		t.Fatalf("sessionListText: %v", err)
	}
	if out != "no sessions found" {
		t.Errorf("empty listing = %q", out)
	}
}
