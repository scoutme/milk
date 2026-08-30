package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
