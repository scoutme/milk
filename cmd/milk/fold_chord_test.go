package main

import (
	"testing"
	"time"
)

// TestCtrlTChord_FoldsAllBlocks: two presses within the window fold every
// fold container — thinking and tool alike — pinning both fold statuses,
// and the chord leg stamps the clock (a rapid third press chains fold-alls,
// idempotent, instead of toggling — held-key auto-repeat settles folded).
func TestCtrlTChord_FoldsAllBlocks(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("a\n")
	m.appendTranscript("x\n")
	m.appendThinkingStreamed("b\n")
	m.appendTranscript("y\n")
	m.setAllThinkBlocks(true) // open + pinned, status open
	m = toolUpdate(t, m, startMsg("t1", "bash", "git status", `{"command":"git status"}`, ""))
	m.setAllToolBlocks(true)

	m = m.handleCtrlT() // first press: single toggle (the last block)
	m = m.handleCtrlT() // second press inside the window: fold all

	for i := range m.thinkBlocks {
		if m.thinkBlocks[i].expanded {
			t.Errorf("think block %d still expanded after the chord", i)
		}
	}
	for i := range m.toolBlocks {
		if m.toolBlocks[i].expanded {
			t.Errorf("tool block %d still expanded after the chord", i)
		}
	}
	if m.thinkFoldOpen || m.toolFoldOpen {
		t.Errorf("fold statuses must be folded after the chord: think=%v tool=%v",
			m.thinkFoldOpen, m.toolFoldOpen)
	}
	if time.Since(m.lastCtrlTPress) > ctrlTChordWindow {
		t.Errorf("the chord leg must stamp lastCtrlTPress, stamped %v ago",
			time.Since(m.lastCtrlTPress))
	}
}

// TestCtrlT_SinglePressUnchanged: one press stays byte-identical to the old
// behavior — only the most recent thinking block toggles (pinned), other
// think blocks and tool blocks are untouched, and the press stamps the
// chord clock.
func TestCtrlT_SinglePressUnchanged(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("a\n")
	m.appendTranscript("x\n")
	m.appendThinkingStreamed("b\n")
	m.appendTranscript("y\n")
	m = toolUpdate(t, m, startMsg("t1", "bash", "s", `{}`, ""))
	m.setAllToolBlocks(true)

	m = m.handleCtrlT()

	if !m.thinkBlocks[1].expanded || !m.thinkBlocks[1].userSet {
		t.Errorf("single press must toggle+pin the last think block, got %+v", m.thinkBlocks[1])
	}
	if m.thinkBlocks[0].expanded || m.thinkBlocks[0].userSet {
		t.Errorf("the earlier think block must be untouched, got %+v", m.thinkBlocks[0])
	}
	if !m.toolBlocks[0].expanded {
		t.Error("single press must not touch tool blocks")
	}
	if m.lastCtrlTPress.IsZero() {
		t.Error("single press must stamp the chord clock")
	}
}

// TestCtrlTChord_ThirdPressOutsideWindowIsFresh: after the chord, a press
// past the window is a fresh single toggle again (the stamp is absolute,
// never cleared).
func TestCtrlTChord_ThirdPressOutsideWindowIsFresh(t *testing.T) {
	m := newTestModelForPanels(t)
	m.appendThinkingStreamed("a\n")
	m.appendTranscript("x\n")
	m.setAllThinkBlocks(false)
	m = m.handleCtrlT()                                      // single: expands the block
	m = m.handleCtrlT()                                      // chord: folds all
	m.lastCtrlTPress = time.Now().Add(-2 * ctrlTChordWindow) // age the clock

	m = m.handleCtrlT() // outside the window: fresh single toggle

	if !m.thinkBlocks[0].expanded {
		t.Error("a press outside the window must be a fresh single toggle (expand)")
	}
}
