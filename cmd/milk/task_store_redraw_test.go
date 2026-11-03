package main

import (
	"bytes"
	"io"
	"path/filepath"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/scoutme/milk/internal/tasks"
)

// redrawKick nudges the probe model into mutating the store from inside
// Update — the exact shape of `/task done` (handler → execTask → Complete).
type redrawKick struct{}

// redrawProbe mirrors the TUI's event loop around one store mutation.
type redrawProbe struct {
	store    *tasks.Store
	taskID   string
	observed bool
}

func (p *redrawProbe) Init() tea.Cmd { return func() tea.Msg { return redrawKick{} } }

func (p *redrawProbe) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg.(type) {
	case redrawKick:
		// Mutating the store FROM WITHIN Update: notify fires inline, so a
		// synchronous Program.Send here would block the event loop against
		// itself (the #347 regression, trace-proven via /task done).
		if err := p.store.Complete(p.taskID); err != nil {
			return p, func() tea.Msg { return redrawDone{err: err.Error()} }
		}
		return p, nil
	case taskStoreChangedMsg:
		p.observed = true
		return p, tea.Quit
	}
	return p, nil
}

func (p *redrawProbe) View() tea.View { return tea.NewView("") }

type redrawDone struct{ err string }

// TestWireTaskStoreRedraw_NoDeadlockInsideUpdate is the regression test for
// the frozen-TUI bug #347 wired in: the store's onChange ran inside Update
// and called Program.Send — an unbuffered channel send waiting for the very
// event loop that was waiting for it. The probe runs a real program, mutates
// a real store from inside Update (the /task done shape), and must observe
// the redraw message. If the wiring regresses to a synchronous Send, the
// loop deadlocks and the 5s deadline below fails the test.
func TestWireTaskStoreRedraw_NoDeadlockInsideUpdate(t *testing.T) {
	store, err := tasks.New(filepath.Join(t.TempDir(), "tasks"), "sess")
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.Create("probe task", nil)
	if err != nil {
		t.Fatal(err)
	}
	// Create already notified (no subscriber yet — the program below wires
	// exactly as runREPL and the session-swap path do).

	probe := &redrawProbe{store: store, taskID: task.ID}
	p := tea.NewProgram(probe,
		tea.WithoutRenderer(),
		tea.WithInput(bytes.NewReader(nil)),
		tea.WithOutput(io.Discard),
	)
	wireTaskStoreRedraw(p, store)

	done := make(chan error, 1)
	go func() {
		_, runErr := p.Run()
		done <- runErr
	}()

	select {
	case runErr := <-done:
		if runErr != nil {
			t.Fatalf("program run: %v", runErr)
		}
		if !probe.observed {
			t.Fatal("taskStoreChangedMsg never reached the loop — the redraw wiring is broken")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event loop deadlocked: onComplete called Program.Send synchronously from inside Update (unbuffered Send waits for the loop it blocks)")
	}
}

// TestWireTaskStoreRedraw_NilGuards: the helper is called on paths where the
// program or store may not exist yet (session swap before runREPL, tests) —
// nil must be a no-op, not a panic.
func TestWireTaskStoreRedraw_NilGuards(t *testing.T) {
	store, err := tasks.New(filepath.Join(t.TempDir(), "tasks"), "sess")
	if err != nil {
		t.Fatal(err)
	}
	wireTaskStoreRedraw(nil, store)
	wireTaskStoreRedraw(&tea.Program{}, nil)
	wireTaskStoreRedraw(nil, nil)
	// Wiring nil leaves no callback: mutating must not panic either.
	if _, err := store.Create("x", nil); err != nil {
		t.Fatal(err)
	}
}
