package main

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/scoutme/milk/internal/config"
)

func longTranscriptModel() *model {
	m := dragTestModel()
	for i := 0; i < 200; i++ {
		m.appendTranscript("line of transcript text\n")
	}
	m.setViewportContent()
	m.vp.SetYOffset(100)
	return m
}

func leftEv(action mouseAction, x, y int) tea.MouseMsg {
	return mouseEvent{X: x, Y: y, Button: tea.MouseLeft, Action: action}.msg()
}

func TestDragAutoScroll_AboveViewportScrollsUp(t *testing.T) {
	m := longTranscriptModel()
	m.handleMouse(leftEv(mousePress, 2, 10))
	before := m.vp.YOffset()

	_, cmd := m.handleMouse(leftEv(mouseMotion, 6, 0)) // header row, above the viewport
	if cmd == nil || !m.dragScrollArmed {
		t.Fatal("expected autoscroll to arm with a tick cmd")
	}
	if m.vp.YOffset() != before-1 {
		t.Fatalf("expected immediate one-line scroll up, offset %d -> %d", before, m.vp.YOffset())
	}
	if m.selEndLine != m.vp.YOffset() || m.selEndCol != 6 {
		t.Errorf("selection end should follow the top visible line, got line=%d col=%d offset=%d", m.selEndLine, m.selEndCol, m.vp.YOffset())
	}

	if next := m.handleDragScrollTick(m.dragScrollGen); next == nil {
		t.Fatal("tick should re-arm while the pointer stays above")
	}
	if m.vp.YOffset() != before-2 {
		t.Errorf("tick should scroll another line, offset=%d", m.vp.YOffset())
	}
}

func TestDragAutoScroll_BelowViewportScrollsDown(t *testing.T) {
	m := longTranscriptModel()
	m.handleMouse(leftEv(mousePress, 2, 10))
	before := m.vp.YOffset()

	m.handleMouse(leftEv(mouseMotion, 4, m.height-1)) // status/input rows, below the viewport
	if m.vp.YOffset() != before+1 {
		t.Fatalf("expected scroll down, offset %d -> %d", before, m.vp.YOffset())
	}
	if want := m.vp.YOffset() + m.vp.Height() - 1; m.selEndLine != want {
		t.Errorf("selection end should be the bottom visible line %d, got %d", want, m.selEndLine)
	}
}

func TestDragAutoScroll_ReturningInsideStopsTicks(t *testing.T) {
	m := longTranscriptModel()
	m.handleMouse(leftEv(mousePress, 2, 10))
	m.handleMouse(leftEv(mouseMotion, 4, 0))
	m.handleMouse(leftEv(mouseMotion, 4, 8))
	off := m.vp.YOffset()
	if next := m.handleDragScrollTick(m.dragScrollGen); next != nil || m.vp.YOffset() != off {
		t.Error("tick after re-entering the viewport must not scroll or re-arm")
	}
}

func TestDragAutoScroll_ReleaseOutsideFinalizesAndStops(t *testing.T) {
	copied := stubClipboard(t)
	on := true
	m := longTranscriptModel()
	m.st.cfg = config.Config{CopyOnSelect: &on}
	m.handleMouse(leftEv(mousePress, 0, 10))
	m.handleMouse(leftEv(mouseMotion, 5, 0))
	m.handleMouse(leftEv(mouseRelease, 5, 0))

	if m.dragScrollArmed || m.dragResetPending {
		t.Error("release must stop autoscroll and end the interaction")
	}
	if len(*copied) != 1 || !strings.Contains((*copied)[0], "line of transcript") {
		t.Errorf("expected the dragged-over text to be copied, got %q", *copied)
	}
	off := m.vp.YOffset()
	if m.handleDragScrollTick(m.dragScrollGen) != nil || m.vp.YOffset() != off {
		t.Error("stale tick after release must be ignored")
	}
}

func TestDragAutoScroll_ReleaseOutsideKeepsSelectionWithoutCopyOnSelect(t *testing.T) {
	copied := stubClipboard(t)
	m := longTranscriptModel()
	m.handleMouse(leftEv(mousePress, 0, 10))
	m.handleMouse(leftEv(mouseMotion, 5, 0))
	m.handleMouse(leftEv(mouseRelease, 5, 0))

	if len(*copied) != 0 {
		t.Fatalf("must not copy when copy_on_select is off, got %q", *copied)
	}
	if m.selText == "" {
		t.Error("selection text should be finalized for a later right-click copy")
	}
}

func TestDragAutoScroll_NoLiveDragDoesNothing(t *testing.T) {
	m := longTranscriptModel()
	off := m.vp.YOffset()
	_, cmd := m.handleMouse(leftEv(mouseMotion, 5, 0))
	if cmd != nil || m.dragScrollArmed || m.vp.YOffset() != off {
		t.Error("motion without a live drag must not autoscroll")
	}
}

func TestDragAutoScroll_AtLimitStopsAfterIdleTicks(t *testing.T) {
	m := longTranscriptModel()
	m.vp.GotoTop()
	m.handleMouse(leftEv(mousePress, 2, 10))
	m.handleMouse(leftEv(mouseMotion, 4, 0))
	for i := 0; i <= dragScrollMaxIdle; i++ {
		m.handleDragScrollTick(m.dragScrollGen)
	}
	if m.dragScrollArmed {
		t.Error("autoscroll should disarm after enough ticks at the scroll limit")
	}
}

func TestDragAutoScroll_PanelDrag(t *testing.T) {
	m := dragTestModel()
	m.panelTasks = true
	m.tasksOffset = 3
	m.taskStore = nil
	region := regionTasks
	m.panelSelRegion = region
	m.panelSelAnchorLine = 5
	m.panelSelAnchorCol = 0
	m.dragResetPending = true

	// Pointer above the panel rows, anywhere horizontally.
	m.handleMouse(leftEv(mouseMotion, m.mainWidth()+1, 0))
	if m.tasksOffset != 2 {
		t.Errorf("expected panel scrolled up one line, offset=%d", m.tasksOffset)
	}
	if m.panelSelEndLine != m.tasksOffset {
		t.Errorf("selection end should be the panel's top visible line, got %d", m.panelSelEndLine)
	}
}
