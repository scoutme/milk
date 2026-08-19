package main

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/scoutme/milk/internal/workflow"
)

// --- panelSelectionText ---

func TestPanelSelectionText_SingleLinePartial(t *testing.T) {
	lines := []string{"hello world"}
	got := panelSelectionText(lines, 0, 0, 0, 5)
	if got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestPanelSelectionText_MultiLine(t *testing.T) {
	lines := []string{"line zero", "line one", "line two"}
	got := panelSelectionText(lines, 0, 5, 2, 4)
	want := "zero\nline one\nline"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPanelSelectionText_ReversedAnchorEndIsOrderIndependent(t *testing.T) {
	lines := []string{"abcdef"}
	forward := panelSelectionText(lines, 0, 1, 0, 4)
	backward := panelSelectionText(lines, 0, 4, 0, 1)
	if forward != backward {
		t.Errorf("expected order-independent result, got %q vs %q", forward, backward)
	}
	if forward != "bcd" {
		t.Errorf("got %q, want %q", forward, "bcd")
	}
}

func TestPanelSelectionText_OutOfRangeLineClamped(t *testing.T) {
	lines := []string{"one", "two"}
	got := panelSelectionText(lines, 0, 0, 10, 3)
	want := "one\ntwo"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestPanelSelectionText_StripsANSI(t *testing.T) {
	lines := []string{dim("hello") + " world"}
	got := panelSelectionText(lines, 0, 0, 0, 11)
	if got != "hello world" {
		t.Errorf("expected ANSI stripped, got %q", got)
	}
}

func TestPanelSelectionText_NegativeAnchorClampedToZero(t *testing.T) {
	lines := []string{"hello"}
	got := panelSelectionText(lines, -1, 0, 0, 3)
	if got != "hel" {
		t.Errorf("got %q, want %q", got, "hel")
	}
}

// --- applyPanelSelectionHighlight ---

func TestApplyPanelSelectionHighlight_NoSelectionReturnsUnchanged(t *testing.T) {
	lines := []string{"one", "two"}
	got := applyPanelSelectionHighlight(lines, -1, 0, -1, 0)
	if got[0] != "one" || got[1] != "two" {
		t.Errorf("expected lines unchanged, got %v", got)
	}
}

// TestApplyPanelSelectionHighlight_PreservesTextContent verifies no characters
// are lost or duplicated across the before/selected/after split, regardless of
// whether the terminal's color profile actually renders the reverse-video
// escape (lipgloss no-ops styling when it detects no color support, e.g. under
// `go test`), by comparing ANSI-stripped output to the original plain text.
func TestApplyPanelSelectionHighlight_PreservesTextContent(t *testing.T) {
	lines := []string{"hello world", "second line", "third line"}
	got := applyPanelSelectionHighlight(lines, 0, 6, 1, 6)
	for i, l := range got {
		if stripped := stripANSI(l); stripped != lines[i] {
			t.Errorf("line %d: stripped result %q != original %q", i, stripped, lines[i])
		}
	}
}

func TestApplyPanelSelectionHighlight_DoesNotMutateInput(t *testing.T) {
	lines := []string{"hello"}
	_ = applyPanelSelectionHighlight(lines, 0, 0, 0, 5)
	if lines[0] != "hello" {
		t.Errorf("input slice must not be mutated, got %q", lines[0])
	}
}

// --- panelContentCol ---

func TestPanelContentCol_MemoryHasNoOffset(t *testing.T) {
	if got := panelContentCol(regionMemory, 5); got != 5 {
		t.Errorf("memory panel: got %d, want 5", got)
	}
}

func TestPanelContentCol_WorkflowSubtractsBorderAndPadding(t *testing.T) {
	if got := panelContentCol(regionWorkflow, 5); got != 3 {
		t.Errorf("workflow panel: got %d, want 3", got)
	}
}

func TestPanelContentCol_WorkflowClampsAtZero(t *testing.T) {
	if got := panelContentCol(regionWorkflow, 1); got != 0 {
		t.Errorf("workflow panel border click: got %d, want 0", got)
	}
}

// --- regionAt ---

func regionTestModel() *model {
	return &model{width: 200, panelMemory: true, workflowPanelOpen: true}
}

func TestRegionAt_Viewport(t *testing.T) {
	m := regionTestModel()
	region, x := m.regionAt(50)
	if region != regionNone {
		t.Errorf("expected regionNone, got %v", region)
	}
	if x != 50 {
		t.Errorf("expected unchanged x=50, got %d", x)
	}
}

func TestRegionAt_MemoryStartsAtMainWidth(t *testing.T) {
	m := regionTestModel()
	mw := m.mainWidth()
	region, x := m.regionAt(mw)
	if region != regionMemory {
		t.Errorf("expected regionMemory, got %v", region)
	}
	if x != 0 {
		t.Errorf("expected regionX=0 at panel start, got %d", x)
	}
}

func TestRegionAt_WorkflowStartsAfterMemory(t *testing.T) {
	m := regionTestModel()
	mw := m.mainWidth()
	region, x := m.regionAt(mw + memoryPanelWidth)
	if region != regionWorkflow {
		t.Errorf("expected regionWorkflow, got %v", region)
	}
	if x != 0 {
		t.Errorf("expected regionX=0 at panel start, got %d", x)
	}
}

func TestRegionAt_PastLastPanelIsNone(t *testing.T) {
	m := regionTestModel()
	mw := m.mainWidth()
	region, _ := m.regionAt(mw + memoryPanelWidth + workflowPanelWidth)
	if region != regionNone {
		t.Errorf("expected regionNone past the last panel, got %v", region)
	}
}

func TestRegionAt_WorkflowHiddenWhenTooNarrow(t *testing.T) {
	m := &model{width: 60, workflowPanelOpen: true}
	// width 60 < 0 (no memory panel) + workflowPanelWidth(31) + 40 = 71 -> hidden.
	if m.workflowPanelVisible() {
		t.Fatal("expected workflow panel hidden at width 60")
	}
	region, _ := m.regionAt(m.mainWidth())
	if region == regionWorkflow {
		t.Error("regionAt must never report regionWorkflow when the panel is not visible")
	}
}

// --- panelScrollOffset ---

func TestPanelScrollOffset(t *testing.T) {
	m := &model{panelOffset: 3, workflowPanelOffset: 7}
	if got := m.panelScrollOffset(regionMemory); got != 3 {
		t.Errorf("memory offset: got %d, want 3", got)
	}
	if got := m.panelScrollOffset(regionWorkflow); got != 7 {
		t.Errorf("workflow offset: got %d, want 7", got)
	}
	if got := m.panelScrollOffset(regionNone); got != 0 {
		t.Errorf("regionNone offset: got %d, want 0", got)
	}
}

// --- panelMaxOffset ---

// TestPanelMaxOffset_ClampsToRealContentLength guards against the offset-drift
// bug where render*Panel's clamp (m.panelOffset = maxOffset) ran on a
// throwaway copy of the model inside View() (a value-receiver method) and
// never reached the persisted state mutated by handleMouse (via Update's
// value-receiver, whose mutations do persist). panelMaxOffset must be usable
// from handleMouse itself so the real offset is bounded at the source.
func TestPanelMaxOffset_ClampsToRealContentLength(t *testing.T) {
	m := &model{
		workflowState: &workflow.State{WorkflowName: "dev", Role: "generator"},
	}
	total := len(buildWorkflowPanelLines(m.workflowState, workflowPanelContentWidth-2))
	if got := m.panelMaxOffset(regionWorkflow, total+10); got != 0 {
		t.Errorf("panel taller than content: got max offset %d, want 0", got)
	}
	h := total - 2
	if got, want := m.panelMaxOffset(regionWorkflow, h), 2; got != want {
		t.Errorf("panel shorter than content: got max offset %d, want %d", got, want)
	}
}

func TestPanelMaxOffset_UnknownRegionIsZero(t *testing.T) {
	m := &model{}
	if got := m.panelMaxOffset(regionNone, 5); got != 0 {
		t.Errorf("regionNone: got %d, want 0", got)
	}
}

// --- clearPanelSelection ---

func TestClearPanelSelection(t *testing.T) {
	m := &model{
		panelSelRegion:     regionWorkflow,
		panelSelAnchorLine: 2,
		panelSelAnchorCol:  3,
		panelSelEndLine:    4,
		panelSelEndCol:     5,
		panelSelDragging:   true,
		panelSelText:       "hi",
	}
	m.clearPanelSelection()
	if m.panelSelRegion != regionNone || m.panelSelAnchorLine != -1 || m.panelSelEndLine != -1 ||
		m.panelSelDragging || m.panelSelText != "" {
		t.Errorf("expected fully cleared selection, got %+v", m)
	}
}

// --- handlePanelMouse: click vs drag on the workflow panel ---

func workflowSelModel() *model {
	return &model{
		workflowPanelOpen: true,
		workflowState: &workflow.State{
			WorkflowName: "dev",
			Role:         "generator",
			VerdictHistory: []workflow.VerdictEntry{
				{Sprint: 1, Pass: 1, Verdict: "good_to_go"},
				{Sprint: 2, Pass: 1, Verdict: "needs_refinement"},
			},
		},
		panelSelAnchorLine: -1,
		panelSelEndLine:    -1,
		selAnchorLine:      -1,
		selEndLine:         -1,
	}
}

func TestHandlePanelMouse_ClickWithoutDragLeavesNoSelection(t *testing.T) {
	m := workflowSelModel()
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionRelease})
	if m.panelSelRegion != regionNone || m.panelSelText != "" {
		t.Errorf("a click (no drag) should leave no active selection, got region=%v text=%q", m.panelSelRegion, m.panelSelText)
	}
}

func TestHandlePanelMouse_DragSelectsText(t *testing.T) {
	m := workflowSelModel()
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 6, Action: tea.MouseActionMotion})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 6, Action: tea.MouseActionRelease})
	if m.panelSelRegion != regionWorkflow {
		t.Errorf("expected panelSelRegion=regionWorkflow after drag, got %v", m.panelSelRegion)
	}
	if m.panelSelText == "" {
		t.Error("expected non-empty panelSelText after a drag selection")
	}
}

func TestHandlePanelMouse_PressClearsTranscriptSelection(t *testing.T) {
	m := workflowSelModel()
	m.selAnchorLine = 0
	m.selEndLine = 0
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	if m.selAnchorLine >= 0 {
		t.Error("starting a panel selection should clear any active transcript selection")
	}
}

// --- handlePanelMouse: ctrl+click extends an existing selection ---

func TestHandlePanelMouse_CtrlClickExtendsSelection(t *testing.T) {
	m := workflowSelModel()
	// Establish an initial selection via a normal drag.
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 5, Action: tea.MouseActionMotion})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 5, Action: tea.MouseActionRelease})
	firstEnd := m.panelSelEndLine
	anchor := m.panelSelAnchorLine

	// Ctrl+click further down should extend the selection, not restart it.
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 8, Ctrl: true, Action: tea.MouseActionPress})

	if m.panelSelAnchorLine != anchor {
		t.Errorf("ctrl+click must not move the anchor: got %d, want %d", m.panelSelAnchorLine, anchor)
	}
	if m.panelSelEndLine == firstEnd {
		t.Error("ctrl+click did not extend the selection end")
	}
	if m.panelSelText == "" {
		t.Error("expected non-empty panelSelText after ctrl+click extend")
	}
}

func TestHandlePanelMouse_CtrlClickCanShrinkSelection(t *testing.T) {
	m := workflowSelModel()
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 8, Action: tea.MouseActionMotion})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 8, Action: tea.MouseActionRelease})

	// Ctrl+click between anchor and the current end shrinks the selection.
	// (handlePanelMouse maps screen row Y to content line Y-2; see panelRowStart.)
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 5, Ctrl: true, Action: tea.MouseActionPress})

	if m.panelSelAnchorLine != 2 {
		t.Errorf("anchor must stay put: got %d, want 2", m.panelSelAnchorLine)
	}
	if m.panelSelEndLine != 3 {
		t.Errorf("expected selection end to move to the ctrl+click line, got %d", m.panelSelEndLine)
	}
}

func TestHandlePanelMouse_CtrlClickWithoutExistingSelectionActsAsNormalClick(t *testing.T) {
	m := workflowSelModel()
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Ctrl: true, Action: tea.MouseActionPress})

	if m.panelSelRegion != regionWorkflow || m.panelSelAnchorLine != 2 {
		t.Errorf("ctrl+click with no prior selection should start a fresh anchor, got region=%v anchor=%d",
			m.panelSelRegion, m.panelSelAnchorLine)
	}
	if m.panelSelDragging {
		t.Error("expected no drag in progress from a bare ctrl+click press")
	}
}

func TestHandlePanelMouse_SwitchingRegionClearsPriorSelection(t *testing.T) {
	m := workflowSelModel()
	// Start (and finish) a drag selection in the workflow panel.
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 6, Action: tea.MouseActionMotion})
	m.handlePanelMouse(regionWorkflow, 2, tea.MouseEvent{Y: 6, Action: tea.MouseActionRelease})
	if m.panelSelText == "" {
		t.Fatal("setup: expected a populated selection before switching regions")
	}
	// A fresh press in a different region must discard the stale selection.
	m.handlePanelMouse(regionMemory, 0, tea.MouseEvent{Y: 4, Action: tea.MouseActionPress})
	if m.panelSelRegion != regionMemory {
		t.Errorf("expected panelSelRegion=regionMemory, got %v", m.panelSelRegion)
	}
	if m.panelSelText != "" {
		t.Errorf("expected the workflow panel's stale selection text cleared, got %q", m.panelSelText)
	}
}
