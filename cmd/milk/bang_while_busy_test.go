package main

import (
	"os"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/scoutme/milk/internal/session"
	"github.com/vito/midterm"
)

// TestHandleBusyKey_BangExecutesImmediately guards issue #128's core fix:
// a "!"-prefixed command must run directly via launchPTYPane even while an
// agent turn is in progress, not get funneled into the "press Enter again to
// spawn a background agent" flow (which would hand the raw "!..." string to
// an LLM agent that has no special handling for milk's own bang syntax).
func TestHandleBusyKey_BangExecutesImmediately(t *testing.T) {
	m := newTestModelForPanels(t)
	m.busy = true
	m.ta.SetValue("!echo pwned-test-marker")

	next, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(model)
	t.Cleanup(func() {
		if nm.ptyPane != nil {
			_ = nm.ptyPane.cmd.Process.Kill()
			_, _ = nm.ptyPane.cmd.Process.Wait()
		}
	})

	if nm.ptyPane == nil {
		t.Fatal("expected launchPTYPane to run (ptyPane set), got nil")
	}
	if !nm.directBashConcurrentTurn {
		t.Error("expected directBashConcurrentTurn true: busy was already true when the bang ran")
	}
	if nm.busySpawnArmed {
		t.Error("bang command must not arm the background-agent-spawn flow")
	}
}

// TestHandleBusyKey_BangModeRePrepended guards the keystroke-consumed "!"
// path: typing "!" first enters bangMode (consuming the "!" itself, per
// handleKey), so by the time Enter is pressed the buffer holds only the
// command text. handleBusyKey must re-prepend "!" the same way handleEnter
// already does for the idle path, or stripBangPrefix never sees it.
func TestHandleBusyKey_BangModeRePrepended(t *testing.T) {
	m := newTestModelForPanels(t)
	m.busy = true
	m.bangMode = true
	m.ta.SetValue("echo pwned-test-marker")

	next, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(model)
	t.Cleanup(func() {
		if nm.ptyPane != nil {
			_ = nm.ptyPane.cmd.Process.Kill()
			_, _ = nm.ptyPane.cmd.Process.Wait()
		}
	})

	if nm.ptyPane == nil {
		t.Fatal("expected launchPTYPane to run once \"!\" was re-prepended")
	}
	if nm.bangMode {
		t.Error("expected bangMode cleared after being consumed")
	}
}

// TestHandleBusyKey_BangSuppressedWhenPasted guards issue #151's fix staying
// intact for the busy path: a tainted leading token must not execute even
// though it's a bang command.
func TestHandleBusyKey_BangSuppressedWhenPasted(t *testing.T) {
	m := newTestModelForPanels(t)
	m.busy = true
	m.leadingPasted = true
	m.ta.SetValue("!echo pwned-test-marker")

	next, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyEnter})
	nm := next.(model)

	if nm.ptyPane != nil {
		t.Error("expected no PTY launched: leading token was pasted, not typed")
	}
}

// TestDirectBashDoneMsg_ConcurrentTurn_PreservesAgentState guards issue
// #128's state-clobber fix: closing a PTY pane that ran alongside an
// already-in-progress agent turn must not clear that turn's busy/cancelTurn,
// and must queue its output for the next turn instead of only the transcript.
func TestDirectBashDoneMsg_ConcurrentTurn_PreservesAgentState(t *testing.T) {
	m := newTestModelForPanels(t)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	m.st.sess = sess

	cancelCalled := false
	m.busy = true
	m.cancelTurn = func() { cancelCalled = true }
	m.directBashConcurrentTurn = true

	ptm, ptmW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ptmW.Close() })
	vt := midterm.NewTerminal(24, 80)
	vt.Write([]byte("test output\n")) //nolint:errcheck
	m.ptyPane = &ptyPaneState{
		ptm:      ptm,
		vt:       vt,
		shellCmd: "echo test",
	}

	next, _ := m.Update(directBashDoneMsg{err: nil})
	nm := next.(model)

	if !nm.busy {
		t.Error("expected busy to stay true: an agent turn was already in progress")
	}
	if nm.cancelTurn == nil {
		t.Error("expected cancelTurn to survive: it belongs to the still-running agent turn")
	}
	if cancelCalled {
		t.Error("cancelTurn must not be invoked by PTY cleanup")
	}
	if nm.ptyPane != nil {
		t.Error("expected ptyPane cleared after directBashDoneMsg")
	}
	if nm.directBashConcurrentTurn {
		t.Error("expected directBashConcurrentTurn reset after being consumed")
	}
	if len(nm.st.sess.PendingBangOutput) != 1 {
		t.Fatalf("expected exactly one queued PendingBangOutput entry, got %d", len(nm.st.sess.PendingBangOutput))
	}
}

// TestDirectBashDoneMsg_NonConcurrent_ClearsBusy is the control case: a bang
// run while idle (the common case) must still clear busy/cancelTurn as
// before, and must not queue anything for the next turn (the idle path's own
// transcript is enough — there's no separately-running turn to inform).
func TestDirectBashDoneMsg_NonConcurrent_ClearsBusy(t *testing.T) {
	m := newTestModelForPanels(t)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	m.st.sess = sess
	m.busy = true
	m.cancelTurn = func() {}
	m.directBashConcurrentTurn = false

	ptm, ptmW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ptmW.Close() })
	m.ptyPane = &ptyPaneState{
		ptm:      ptm,
		vt:       midterm.NewTerminal(24, 80),
		shellCmd: "echo test",
	}

	next, _ := m.Update(directBashDoneMsg{err: nil})
	nm := next.(model)

	if nm.busy {
		t.Error("expected busy cleared: the PTY was the only reason it was true")
	}
	if nm.cancelTurn != nil {
		t.Error("expected cancelTurn cleared")
	}
	if len(nm.st.sess.PendingBangOutput) != 0 {
		t.Error("expected no queued output for the non-concurrent (idle) case")
	}
}
