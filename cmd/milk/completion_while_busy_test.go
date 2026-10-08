package main

// Regression tests for completion keys while an agent turn is running
// (busy state): Tab/Shift+Tab cycle completions and Enter accepts the
// active completion or arrow-hint selection — only Enter-as-submission
// stays trapped. The helpers are shared with the idle path
// (completion.go: acceptActiveCompletion / handleTabKey), so these tests
// pin both states against drifting apart.

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// busyCompletionModel returns a busy model with "/con" typed, ready for a
// Tab keystroke to start a completion cycle.
func busyCompletionModel(t *testing.T) model {
	t.Helper()
	m := newTestModelForPanels(t)
	m.busy = true
	m.ta.SetValue("/con")
	// Completion works on the token before the cursor; mimic typed input
	// (SetValue alone leaves the cursor at position 0).
	m.ta.SetCursor(len([]rune("/con")))
	return m
}

// TestBusyTab_StartsCompletion: Tab must not be swallowed while busy —
// it starts a completion cycle and inserts the token, exactly like idle.
func TestBusyTab_StartsCompletion(t *testing.T) {
	m := busyCompletionModel(t)

	next, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyTab})
	nm := next.(model)

	if len(nm.tabMatches) == 0 {
		t.Fatal("Tab while busy started no completion cycle (tabMatches empty)")
	}
	if got := nm.ta.Value(); !strings.HasPrefix(got, "/config") {
		t.Errorf("completion token not inserted, buffer = %q", got)
	}
	if nm.busyHint != "" {
		t.Errorf("Tab must not set a busy hint, got %q", nm.busyHint)
	}
}

// TestBusyShiftTab_CyclesCompletion: Shift+Tab reaches the same
// handleTabKey(-1) path while busy instead of being ignored.
func TestBusyShiftTab_CyclesCompletion(t *testing.T) {
	m := busyCompletionModel(t)

	next, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyTab})
	nm := next.(model)
	if len(nm.tabMatches) == 0 {
		t.Fatal("Tab while busy started no completion cycle")
	}
	before := nm.ta.Value()

	next, _ = nm.handleBusyKey(tea.KeyMsg{Type: tea.KeyShiftTab})
	ns := next.(model)

	if len(ns.tabMatches) == 0 {
		t.Fatal("Shift+Tab while busy dropped the completion cycle")
	}
	// From index 0, backwards wraps to the last entry — either the buffer
	// changes (wrap) or the cycle state stays intact (single entry).
	if ns.ta.Value() == before && len(ns.tabMatches) == 1 && ns.tabCmdIdx != 0 {
		t.Errorf("Shift+Tab reset the cycle: idx = %d", ns.tabCmdIdx)
	}
	if ns.busyHint != "" {
		t.Errorf("Shift+Tab must not set a busy hint, got %q", ns.busyHint)
	}
}

// TestBusyEnter_AcceptsCompletion: Enter with an active tab completion
// accepts it (clears the cycle, no busy hint) instead of being trapped
// as "agent is working — Ctrl+Enter to spawn…".
func TestBusyEnter_AcceptsCompletion(t *testing.T) {
	m := busyCompletionModel(t)

	next, _ := m.handleBusyKey(tea.KeyMsg{Type: tea.KeyTab})
	nm := next.(model)
	if len(nm.tabMatches) == 0 {
		t.Fatal("Tab while busy started no completion cycle")
	}

	next, _ = nm.handleBusyKey(teaKeyEnter())
	out := next.(model)

	if len(out.tabMatches) != 0 {
		t.Error("Enter did not accept the completion (tabMatches still set)")
	}
	if out.busyHint != "" {
		t.Errorf("Enter-as-accept must not raise the busy trap, got %q", out.busyHint)
	}
	if got := out.ta.Value(); !strings.HasPrefix(got, "/config") {
		t.Errorf("buffer after accept = %q, want the completed sig", got)
	}
}

// TestBusyEnter_CommitsArrowHint: Enter with an arrow-selected hint
// (hintIdx set, no tab cycle) commits the hint into the buffer — the
// same commitHintSelection path the idle Enter uses.
func TestBusyEnter_CommitsArrowHint(t *testing.T) {
	m := newTestModelForPanels(t)
	m.busy = true
	m.ta.SetValue("/con")
	m.tabHints = []string{"/config  show the configuration"}
	m.tabHintsBase = []string{"/config  show the configuration"}
	m.hintIdx = 0

	next, _ := m.handleBusyKey(teaKeyEnter())
	out := next.(model)

	if got := out.ta.Value(); got != "/config" {
		t.Errorf("hint not committed: buffer = %q, want %q", got, "/config")
	}
	if out.hintIdx != -1 {
		t.Errorf("hintIdx = %d after commit, want -1", out.hintIdx)
	}
	if out.busyHint != "" {
		t.Errorf("Enter-as-accept must not raise the busy trap, got %q", out.busyHint)
	}
}

// TestBusyEnter_PlainTextStillTrapped: the invariant guard — with no
// completion active, Enter on plain text keeps the old busy trap (no
// submission, hint points at Ctrl+Enter).
func TestBusyEnter_PlainTextStillTrapped(t *testing.T) {
	m := newTestModelForPanels(t)
	m.busy = true
	m.ta.SetValue("plain prompt text")

	next, _ := m.handleBusyKey(teaKeyEnter())
	out := next.(model)

	if !strings.Contains(out.busyHint, "Ctrl+Enter") {
		t.Errorf("expected the plain-text busy trap hint, got %q", out.busyHint)
	}
	if got := out.ta.Value(); got != "plain prompt text" {
		t.Errorf("buffer changed on trapped Enter: %q", got)
	}
}

// TestIdleEnter_AcceptsCompletion: the idle path still accepts via the
// shared helper (replacing the inlined block it was extracted from).
func TestIdleEnter_AcceptsCompletion(t *testing.T) {
	m := busyCompletionModel(t)
	m.busy = false

	next, _ := m.handleKey(tea.KeyMsg{Type: tea.KeyTab})
	nm := next.(model)
	if len(nm.tabMatches) == 0 {
		t.Fatal("Tab started no completion cycle")
	}

	next, _ = nm.handleKey(teaKeyEnter())
	out := next.(model)

	if len(out.tabMatches) != 0 {
		t.Error("idle Enter did not accept the completion")
	}
	if got := out.ta.Value(); !strings.HasPrefix(got, "/config") {
		t.Errorf("buffer after accept = %q", got)
	}
}
