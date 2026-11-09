package main

import (
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
)

// ctrlCKeyPress builds the key event bubbletea delivers for a plain Ctrl+C —
// the same shape ptyKeyToBytes maps to ^C (0x03).
func ctrlCKeyPress() tea.KeyPressMsg {
	return tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl}
}

// hungPaneCommand is a child that never reacts to ^C: sh drops SIGINT to
// ignored, and the foreground sleep inherits the ignore disposition across
// exec — the exact shape of the reported bug (a task/FTP deploy that swallows
// the interrupt) and the reason the pane needs its own escape hatch.
const hungPaneCommand = `trap '' INT; sleep 30`

// TestHandlePTYKey_DoubleCtrlCForceKillsHungPane pins the escape hatch end
// to end: the first Ctrl+C is still a plain ^C for the child (a terminal
// pane must behave like one), the second press inside the window tears the
// pane down synchronously — busy released, force-kill note in the
// transcript — so the user is back at milk's input without depending on the
// child ever exiting.
func TestHandlePTYKey_DoubleCtrlCForceKillsHungPane(t *testing.T) {
	m := newTestModelForPanels(t)
	next, _ := m.launchPTYPane(hungPaneCommand)
	nm := next.(model)
	state := nm.ptyPane
	if state == nil {
		t.Fatal("expected PTY pane launched")
	}
	t.Cleanup(func() { killPaneBounded(t, state) })

	// First Ctrl+C: forwarded only — the child ignores it, the pane stays.
	n1, _ := nm.handlePTYKey(ctrlCKeyPress())
	nm = n1.(model)
	if nm.ptyPane == nil {
		t.Fatal("first Ctrl+C must only forward ^C — the pane must stay up")
	}
	if nm.ptyPane.lastCtrlC.IsZero() {
		t.Error("expected lastCtrlC stamped by the first press")
	}
	if !nm.busy {
		t.Error("expected busy to stay true after a single forwarded Ctrl+C")
	}

	// Second Ctrl+C inside the window: force-kill, immediate return to milk.
	n2, _ := nm.handlePTYKey(ctrlCKeyPress())
	nm = n2.(model)
	if nm.ptyPane != nil {
		t.Fatal("expected the pane torn down synchronously by the second Ctrl+C")
	}
	if nm.busy {
		t.Error("expected busy released — the pane was the only reason it was true")
	}
	if got := nm.transcript.String(); !strings.Contains(got, "force-killed") {
		t.Errorf("expected a force-killed note in the transcript, got: %q", got)
	}
}

// TestHandlePTYKey_CtrlCOutsideWindowOnlyForwards guards the escalation
// window: presses more than ptyCtrlCKillWindow apart are independent ^C
// forwards (an impatient user hammering ^C once a second still gets normal
// terminal semantics), never a force-kill.
func TestHandlePTYKey_CtrlCOutsideWindowOnlyForwards(t *testing.T) {
	m := newTestModelForPanels(t)
	next, _ := m.launchPTYPane(hungPaneCommand)
	nm := next.(model)
	state := nm.ptyPane
	if state == nil {
		t.Fatal("expected PTY pane launched")
	}
	t.Cleanup(func() { killPaneBounded(t, state) })

	n1, _ := nm.handlePTYKey(ctrlCKeyPress())
	nm = n1.(model)
	// Age the stamp beyond the window before the second press.
	nm.ptyPane.lastCtrlC = time.Now().Add(-(ptyCtrlCKillWindow + time.Second))

	n2, _ := nm.handlePTYKey(ctrlCKeyPress())
	nm = n2.(model)
	if nm.ptyPane == nil {
		t.Fatal("a Ctrl+C outside the double-press window must only forward ^C")
	}
	if !nm.busy {
		t.Error("expected busy to stay true — no force-kill happened")
	}
}

// TestDirectBashDoneMsg_FromForceKilledPaneDoesNotCloseNewPane guards the
// identity check on the waiter's message: forceKillPTYPane tears state down
// eagerly, so the orphaned read goroutine may deliver its directBashDoneMsg
// later — possibly after a *new* pane launched. That late message must be
// ignored instead of closing the pane now on screen (and releasing the busy
// state it owns).
func TestDirectBashDoneMsg_FromForceKilledPaneDoesNotCloseNewPane(t *testing.T) {
	m := newTestModelForPanels(t)
	next, _ := m.launchPTYPane(hungPaneCommand)
	nm := next.(model)
	old := nm.ptyPane
	if old == nil {
		t.Fatal("expected first PTY pane launched")
	}
	t.Cleanup(func() { killPaneBounded(t, old) })

	// Force-kill the first pane (double Ctrl+C inside the window).
	n1, _ := nm.handlePTYKey(ctrlCKeyPress())
	n2, _ := n1.(model).handlePTYKey(ctrlCKeyPress())
	nm = n2.(model)
	if nm.ptyPane != nil {
		t.Fatal("expected first pane torn down by the force-kill")
	}

	// A second bang launches a fresh pane before the orphaned waiter wakes.
	next2, _ := nm.launchPTYPane("echo second-pane")
	nm = next2.(model)
	fresh := nm.ptyPane
	if fresh == nil {
		t.Fatal("expected second pane launched")
	}
	if fresh == old {
		t.Fatal("second pane must be a distinct state object")
	}
	t.Cleanup(func() { killPaneBounded(t, fresh) })

	// The late message from the force-killed pane's waiter arrives.
	n3, _ := nm.Update(directBashDoneMsg{err: nil, pane: old})
	got := n3.(model)
	if got.ptyPane != fresh {
		t.Error("late done msg from the force-killed pane must not close the new pane")
	}
	if !got.busy {
		t.Error("late done msg must not release the busy state owned by the new pane")
	}
}
