package main

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// tuiSessState builds the interactiveState the TUI handlers run against, with
// one already-bound current session.
func tuiSessState(t *testing.T, turns ...string) (*interactiveState, *session.Session) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	sess := tuiSaveSession(t, "/repo", "aaaa-1111", "alpha", turns)
	return &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}, sess
}

// tuiSaveSession persists a session with a fixed ID and named history turns.
func tuiSaveSession(t *testing.T, cwd, id, name string, turns []string) *session.Session {
	t.Helper()
	now := time.Now()
	var hist []session.Turn
	for _, content := range turns {
		hist = append(hist, session.Turn{Role: session.RoleUser, Content: content, Timestamp: now})
	}
	s := &session.Session{ID: id, Name: name, CWD: cwd, CreatedAt: now, LastUsed: now, State: session.StateRouting, History: hist}
	if err := session.Save(s); err != nil {
		t.Fatalf("Save(%s): %v", id, err)
	}
	return s
}

func TestExecSessionCmdSessionsListsAndMarksCurrent(t *testing.T) {
	st, cur := tuiSessState(t, "alpha-turn")
	tuiSaveSession(t, "/repo", "bbbb-2222", "beta", nil)

	out := execSessionCmd(cmdSessions, "", st)
	if !strings.Contains(out, "* aaaa-111") || !strings.Contains(out, "beta") {
		t.Errorf("/sessions missing current marker or entries: %q", out)
	}
	if st.sess.ID != cur.ID {
		t.Errorf("/sessions must not rebind: %s", st.sess.ID)
	}

	if out := execSessionCmd(cmdSessions, "bogus", st); !strings.Contains(out, "usage: /sessions") {
		t.Errorf("/sessions bogus = %q", out)
	}
	if out := execSessionCmd(cmdListLegacy, "", st); !strings.Contains(out, "deprecated") || !strings.Contains(out, "beta") {
		t.Errorf("/list alias = %q", out)
	}
}

func TestExecSessionCmdNewAndResumeSwapBinding(t *testing.T) {
	st, old := tuiSessState(t, "alpha-turn")
	beta := tuiSaveSession(t, "/repo", "bbbb-2222", "beta", []string{"beta-turn"})

	out := execSessionCmd(cmdNew, "gamma", st)
	if st.sess.ID == old.ID || st.sess.Name != "gamma" || !strings.Contains(out, "new session") {
		t.Fatalf("/new [name] = %q, sess = %+v", out, st.sess)
	}
	// The previous session stays resumable — /new deletes nothing.
	if _, err := session.Resolve("/repo", old.ID); err != nil {
		t.Fatalf("/new dropped the old session: %v", err)
	}

	out = execSessionCmd(cmdResume, "beta", st)
	if st.sess.ID != beta.ID || !strings.Contains(out, "resumed session") {
		t.Fatalf("/resume beta = %q, sess = %s", out, st.sess.ID)
	}
	if out := execSessionCmd(cmdResume, "beta", st); !strings.Contains(out, "already in session") || st.sess.ID != beta.ID {
		t.Fatalf("/resume current = %q", out)
	}

	// Bare /resume lists what is resumable instead of guessing.
	before := st.sess.ID
	out = execSessionCmd(cmdResume, "", st)
	if !strings.Contains(out, "usage: /resume") || !strings.Contains(out, "alpha") || st.sess.ID != before {
		t.Fatalf("/resume bare = %q", out)
	}
}

func TestExecSessionCmdDropScopes(t *testing.T) {
	st, cur := tuiSessState(t, "alpha-turn")
	other := tuiSaveSession(t, "/repo", "bbbb-2222", "beta", nil)

	out := execSessionCmd(cmdDrop, "beta", st)
	if st.sess.ID != cur.ID || strings.Contains(out, "new session") {
		t.Fatalf("/drop <other> must leave the conversation alone: %q", out)
	}
	if !strings.Contains(out, other.ID) {
		t.Fatalf("/drop output hides what it deleted: %q", out)
	}
	if _, err := session.Resolve("/repo", other.ID); !errors.Is(err, session.ErrNotFound) {
		t.Fatalf("dropped session still resolves: %v", err)
	}

	out = execSessionCmd(cmdDrop, "", st)
	if st.sess.ID == cur.ID || !strings.Contains(out, cur.ID) || !strings.Contains(out, "new session") {
		t.Fatalf("/drop bare = %q, sess = %s", out, st.sess.ID)
	}

	if out := execSessionCmd(cmdDrop, "ghost", st); !strings.Contains(out, "no session matches") {
		t.Fatalf("/drop ghost = %q", out)
	}
}

func TestHandleSlashInputResumeReseedsTranscript(t *testing.T) {
	st, _ := tuiSessState(t, "alpha-turn")
	beta := tuiSaveSession(t, "/repo", "bbbb-2222", "beta", []string{"beta-turn"})
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)
	m.appendTranscript("OLDVIEW-MARKER\n")

	updated, _ := m.handleSlashInput(cmdResume, "bbbb")
	m2 := updated.(model)

	if st.sess.ID != beta.ID {
		t.Fatalf("binding not swapped: %s", st.sess.ID)
	}
	got := m2.transcript.String()
	for _, want := range []string{"switched to session", "beta-turn"} {
		if !strings.Contains(got, want) {
			t.Errorf("transcript missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "OLDVIEW-MARKER") {
		t.Errorf("old conversation left on screen after /resume:\n%s", got)
	}
	if m2.transcriptNoThink.String() != got {
		t.Error("no-think variant out of sync after reseed")
	}
}

func TestSessionRefsCompleteForResumeAndDrop(t *testing.T) {
	st, _ := tuiSessState(t, "alpha-turn")
	tuiSaveSession(t, "/repo", "abcd-2222", "feature", nil)
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	for _, cmd := range []string{cmdResume, cmdDrop} {
		if got := namespaceForParam(cmd, "id|prefix|name"); got != nsSession {
			t.Errorf("namespaceForParam(%s) = %q, want %q", cmd, got, nsSession)
		}
	}
	got := strings.Join(m.paramLookup(nsSession), " ")
	if !strings.Contains(got, "abcd-222") || !strings.Contains(got, "feature") {
		t.Errorf("session refs = %q, want the short id and the name", got)
	}

	// Tab right after the command shows the signature (every milk command
	// does); typing a partial ref completes concrete session refs in value
	// mode.
	if res := buildTabMatches("/resume ", "/repo", m.paramLookup); res.replaceBase != cmdResume || len(res.matches) != 1 {
		t.Errorf("/resume + space completion = %+v, want the signature hint", res)
	}
	res := buildTabMatches("/resume abcd", "/repo", m.paramLookup)
	if !res.valueMode || len(res.matches) != 1 || res.matches[0] != "abcd-222" {
		t.Errorf("/resume abcd completion = %+v, want the matching short id", res)
	}
}
