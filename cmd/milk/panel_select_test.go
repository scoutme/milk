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

func TestPanelContentCol_IsIdentity(t *testing.T) {
	// No panel reserves left-side columns (border/padding lives on the right
	// as the scrollbar column, outside any region's own width), so this is a
	// pass-through for every region.
	for _, region := range []panelRegion{regionMemory, regionTasks, regionBackground, regionWorkflow} {
		if got := panelContentCol(region, 5); got != 5 {
			t.Errorf("region %v: got %d, want 5", region, got)
		}
	}
}

func TestPanelContentCol_ClampsNegativeToZero(t *testing.T) {
	if got := panelContentCol(regionMemory, -1); got != 0 {
		t.Errorf("got %d, want 0", got)
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
	// width 60 < 0 (no memory panel) + workflowPanelWidth(33) + 40 = 73 -> hidden.
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
	m := &model{panelOffset: 3, tasksOffset: 5, backgroundOffset: 9, workflowPanelOffset: 7}
	if got := m.panelScrollOffset(regionMemory); got != 3 {
		t.Errorf("memory offset: got %d, want 3", got)
	}
	if got := m.panelScrollOffset(regionTasks); got != 5 {
		t.Errorf("tasks offset: got %d, want 5", got)
	}
	if got := m.panelScrollOffset(regionBackground); got != 9 {
		t.Errorf("background offset: got %d, want 9", got)
	}
	if got := m.panelScrollOffset(regionWorkflow); got != 7 {
		t.Errorf("workflow offset: got %d, want 7", got)
	}
	if got := m.panelScrollOffset(regionNone); got != 0 {
		t.Errorf("regionNone offset: got %d, want 0", got)
	}
}

// TestPanelOffsetPtr_MutatesPersistedField guards the parity fix that made
// wheel scrolling and click-selection identical across all four side panels:
// panelOffsetPtr must return a pointer into the model's own field, not a copy,
// so mutating through it (as handleMouse's wheel cases do) actually persists.
func TestPanelOffsetPtr_MutatesPersistedField(t *testing.T) {
	m := &model{}
	for _, tc := range []struct {
		region panelRegion
		get    func() int
	}{
		{regionMemory, func() int { return m.panelOffset }},
		{regionTasks, func() int { return m.tasksOffset }},
		{regionBackground, func() int { return m.backgroundOffset }},
		{regionWorkflow, func() int { return m.workflowPanelOffset }},
	} {
		p := m.panelOffsetPtr(tc.region)
		if p == nil {
			t.Fatalf("region %v: expected non-nil pointer", tc.region)
		}
		*p = 42
		if got := tc.get(); got != 42 {
			t.Errorf("region %v: field not mutated through pointer, got %d", tc.region, got)
		}
	}
	if m.panelOffsetPtr(regionNone) != nil {
		t.Error("regionNone: expected nil pointer")
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
	total := len(buildWorkflowPanelLines(m.workflowState, workflowPanelInner))
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

// --- handlePanelMouse: parity — tasks and background must select just like
// memory and workflow (previously they were excluded from click/drag
// handling entirely). ---

func TestHandlePanelMouse_DragSelectsText_Tasks(t *testing.T) {
	m := &model{} // nil taskStore still yields a few placeholder content lines
	m.handlePanelMouse(regionTasks, 2, tea.MouseEvent{Y: 2, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionTasks, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionMotion})
	m.handlePanelMouse(regionTasks, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionRelease})
	if m.panelSelRegion != regionTasks {
		t.Errorf("expected panelSelRegion=regionTasks after drag, got %v", m.panelSelRegion)
	}
	if m.panelSelText == "" {
		t.Error("expected non-empty panelSelText after a drag selection on the tasks panel")
	}
}

func TestHandlePanelMouse_DragSelectsText_Background(t *testing.T) {
	m := &model{} // nil backgroundMgr still yields a few placeholder content lines
	m.handlePanelMouse(regionBackground, 2, tea.MouseEvent{Y: 2, Action: tea.MouseActionPress})
	m.handlePanelMouse(regionBackground, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionMotion})
	m.handlePanelMouse(regionBackground, 2, tea.MouseEvent{Y: 4, Action: tea.MouseActionRelease})
	if m.panelSelRegion != regionBackground {
		t.Errorf("expected panelSelRegion=regionBackground after drag, got %v", m.panelSelRegion)
	}
	if m.panelSelText == "" {
		t.Error("expected non-empty panelSelText after a drag selection on the background panel")
	}
}

// TestHandleMouse_LeftClickRoutesAnySidePanelToHandlePanelMouse guards the
// generalization of handleMouse's MouseButtonLeft case from
// "region == regionMemory || region == regionWorkflow" to "region !=
// regionNone" — tasks and background must reach handlePanelMouse exactly
// like memory and workflow do, not just scroll.
func TestHandleMouse_LeftClickRoutesAnySidePanelToHandlePanelMouse(t *testing.T) {
	m := &model{width: 200, height: 40, panelTasks: true}
	mw := m.mainWidth()
	// First column inside the tasks panel region.
	_, cmd := m.handleMouse(tea.MouseMsg(tea.MouseEvent{X: mw, Y: 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}))
	_ = cmd
	if m.panelSelRegion != regionTasks {
		t.Errorf("expected a left click inside the tasks panel to start a tasks selection, got region=%v", m.panelSelRegion)
	}
}
