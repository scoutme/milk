package main

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// TestPanelAltBackground_AlternatesByVisibleOrder verifies the alternating
// tint is assigned by left-to-right position among *currently open* panels,
// not a fixed parity per region — so closing a panel in the middle of the
// sequence doesn't leave two visually adjacent open panels sharing a
// background.
func TestPanelAltBackground_AlternatesByVisibleOrder(t *testing.T) {
	m := &model{st: &interactiveState{sess: &session.Session{}}}

	// All four open: memory(0)=false, tasks(1)=true, background(2)=false, workflow(3)=true.
	m.panelMemory = true
	m.panelTasks = true
	m.panelBackground = true
	m.workflowPanelOpen = true
	m.workflowState = &workflow.State{WorkflowName: "w"}
	m.width = 400 // wide enough for workflowPanelVisible to return true

	cases := []struct {
		region panelRegion
		want   bool
	}{
		{regionMemory, false},
		{regionTasks, true},
		{regionBackground, false},
		{regionWorkflow, true},
	}
	for _, c := range cases {
		if got := m.panelAltBackground(c.region); got != c.want {
			t.Errorf("region %v: alt=%v, want %v", c.region, got, c.want)
		}
	}

	// Close memory and background — tasks and workflow are now adjacent and
	// must land on opposite parities (0 and 1), not both share region-based
	// parity from before.
	m.panelMemory = false
	m.panelBackground = false
	if got := m.panelAltBackground(regionTasks); got != false {
		t.Errorf("tasks: alt=%v, want false (now first open panel)", got)
	}
	if got := m.panelAltBackground(regionWorkflow); got != true {
		t.Errorf("workflow: alt=%v, want true (now second open panel)", got)
	}
}

// TestWithPanelBackground_SurvivesEmbeddedResets verifies the background
// re-injection trick: a line containing ANSI-styled spans (which close with
// a full "\x1b[0m" reset) keeps its background applied end-to-end rather
// than losing it the moment an inner span resets.
func TestWithPanelBackground_SurvivesEmbeddedResets(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	bg := "\033[48;2;10;10;10m"
	line := "plain " + dim("dimmed") + " more"
	out := withPanelBackground(line, bg)

	if !strings.HasPrefix(out, bg) {
		t.Fatalf("expected line to start with the background code, got %q", out)
	}
	// The background code must reappear immediately after the embedded
	// reset from dim(), not just at the very start of the line.
	if strings.Count(out, bg) < 2 {
		t.Errorf("expected background re-applied after the embedded reset, got %q", out)
	}
	if !strings.HasSuffix(out, ansiReset) {
		t.Errorf("expected line to end with a final reset, got %q", out)
	}
}

// TestWithPanelBackground_EmptyBgIsNoop verifies passing "" (non-TTY, or a
// panel that isn't in the alternating slot) leaves the line untouched.
func TestWithPanelBackground_EmptyBgIsNoop(t *testing.T) {
	line := "hello " + dim("world")
	if got := withPanelBackground(line, ""); got != line {
		t.Errorf("expected no-op with empty bg, got %q want %q", got, line)
	}
}

// TestRenderSidePanel_BackgroundTintPreservesWidth verifies that applying
// the alternating background through renderSidePanel doesn't change the
// panel's visible (post-stripANSI) width — the border-consistency invariant
// that TestSidePanels_AllHaveConsistentWidthAndScrollbar guards elsewhere.
func TestRenderSidePanel_BackgroundTintPreservesWidth(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := &model{st: &interactiveState{sess: &session.Session{}}}
	m.panelMemory = true
	m.panelTasks = true // odd position -> tinted

	out := m.renderSidePanel(regionTasks, 5)
	if !strings.Contains(out, "48;2;") {
		t.Fatal("expected the tasks panel (second open panel) to carry a background tint")
	}
	for i, line := range strings.Split(out, "\n") {
		if w := utf8.RuneCountInString(stripANSI(line)); w != tasksPanelInner {
			t.Errorf("line %d: width %d, want %d", i, w, tasksPanelInner)
		}
	}

	memOut := m.renderSidePanel(regionMemory, 5)
	if strings.Contains(memOut, "48;2;") {
		t.Error("expected the memory panel (first open panel) to have no background tint")
	}
}
