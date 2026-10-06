package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

func TestHandleSlashInputNewRefreshesSessionScopedState(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldSess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: oldSess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)
	m.sessionHistory = []string{"old prompt", "/new"}

	updated, _ := m.handleSlashInput("/new", "")
	m2 := updated.(model)

	if st.sess.ID == oldSess.ID {
		t.Fatal("expected /new to replace active session")
	}
	oldHistoryPath, err := sessionHistoryPath(oldSess.ID)
	if err != nil {
		t.Fatal(err)
	}
	oldHistory, err := os.ReadFile(oldHistoryPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(oldHistory) != "old prompt\n/new\n" {
		t.Fatalf("expected old session history to be flushed before switching, got %q", oldHistory)
	}
	if len(m2.sessionHistory) != 0 {
		t.Fatalf("expected new session history to be loaded fresh, got %#v", m2.sessionHistory)
	}
	if m2.taskStore == nil {
		t.Fatal("expected task store to be rebuilt for new session")
	}
	created, err := m2.taskStore.Create("new session task", nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.SessionID != st.sess.ID {
		t.Fatalf("task store still points at old session: got %q want %q", created.SessionID, st.sess.ID)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("HOME"), ".milk", "tasks", st.sess.ID+".json")); err != nil {
		t.Fatalf("expected new session task file: %v", err)
	}
}

// TestHandleSlashInputClearAliasesNew covers #164: /clear must behave
// exactly like /new — recognized as a leading command token (not inert
// text) and replacing the active session with a fresh one.
func TestHandleSlashInputClearAliasesNew(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldSess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: oldSess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	// extractSlashCommand must recognize /clear as a leading command token.
	cmd, rest, found := extractSlashCommand("/clear leftover prompt")
	if !found || cmd != "/clear" || rest != "leftover prompt" {
		t.Fatalf("expected /clear to be extracted, got cmd=%q rest=%q found=%v", cmd, rest, found)
	}

	updated, _ := m.handleSlashInput("/clear", "")
	if updated == nil {
		t.Fatal("expected a model back from /clear")
	}
	if st.sess.ID == oldSess.ID {
		t.Fatal("expected /clear to replace active session (alias of /new)")
	}

	// The alias must also show up in tab-completion's variant hints.
	vars, ok := cmdVariants["/clear"]
	if !ok || len(vars) == 0 {
		t.Fatalf("expected /clear variants derived from help text, got %#v", vars)
	}
}

// TestHandleSlashInputNewRepointsBackgroundJobStateFile covers the session-
// swap persistence bug: the background Manager's job state file is wired once
// with the startup session's ID (repl.go's Manager construction), so without a
// re-point in refreshSessionScopedState every job spawned after /new, /clear
// or /drop kept persisting under the *previous* session's ~/.milk/jobs file
// and the new session's triage record never appeared.
func TestHandleSlashInputNewRepointsBackgroundJobStateFile(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	oldSess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	jobsDir := filepath.Join(os.Getenv("HOME"), ".milk", "jobs")
	mgr := local.NewManager(context.Background(), 2)
	mgr.SetStateFile(filepath.Join(jobsDir, oldSess.ID+".json")) // repl.go's startup wiring
	st := &interactiveState{sess: oldSess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	updated, _ := m.handleSlashInput("/new", "")
	if updated == nil {
		t.Fatal("expected a model back from /new")
	}
	newSessID := st.sess.ID
	if newSessID == oldSess.ID {
		t.Fatal("expected /new to replace active session")
	}

	// A job spawned after the swap must persist under the NEW session's file.
	done := make(chan *local.Job, 1)
	mgr.SetOnDone(func(j *local.Job) { done <- j })
	mgr.Spawn("post-swap", "t", "primary", "m",
		func(context.Context, string, io.Writer) (string, session.TokenUsage, error) {
			return "ok", session.TokenUsage{}, nil
		})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("post-swap job never finished")
	}

	newRecs, err := local.LoadJobs(filepath.Join(jobsDir, newSessID+".json"))
	if err != nil {
		t.Fatalf("LoadJobs(new session file): %v", err)
	}
	if len(newRecs) != 1 || newRecs[0].Label != "post-swap" || newRecs[0].Status != local.JobCompleted {
		t.Fatalf("new session's job file = %+v, want exactly one completed post-swap job", newRecs)
	}
	oldRecs, err := local.LoadJobs(filepath.Join(jobsDir, oldSess.ID+".json"))
	if err != nil {
		t.Fatalf("LoadJobs(old session file): %v", err)
	}
	if len(oldRecs) != 0 {
		t.Fatalf("old session's job file still holds post-swap records: %+v", oldRecs)
	}
}
