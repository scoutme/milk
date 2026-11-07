package main

// Click cadence for EVERY side panel (panelClickAction): single =
// deferred focus (never an action), double = the panel's action with focus
// abandoned, triple (project file rows) = @-mention. Wired through repl.go's
// mouse press branch (no panel's press sets focus), flushed by the first
// key, superseded by a press in the main area. A release only counts as a
// click when press+release are close together (holding the button down is
// not a click), and a real drag (text selection) never changes focus —
// movement is not a click at all.

import (
	"context"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/workflow"
)

// probeRegionX returns the first screen column inside the given region.
func probeRegionX(t *testing.T, m *model, want panelRegion) int {
	t.Helper()
	for x := 0; x < m.width; x++ {
		if r, _ := m.regionAt(x); r == want {
			return x
		}
	}
	t.Fatalf("region %v not present at width %d", want, m.width)
	return 0
}

func TestPanelClickCadence_Dir_SingleDefers_DoubleExpands(t *testing.T) {
	m := projectClickModel(t)
	// Row 2 is the "▾ cmd/" dir (top level → expanded). Uniformity: even
	// folders act on the DOUBLE click — one click is focus, nothing else.

	if cmd := m.panelClickAction(regionProject, 2); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.projectExpanded["cmd/"] != false {
		t.Errorf("single click must not toggle the dir (uniform: action = double), got %v", m.projectExpanded["cmd/"])
	}
	if m.attached != nil {
		t.Error("a dir row never previews")
	}
	if !m.panelClickPending.valid {
		t.Fatal("dir click1 must defer focus")
	}

	// Double: the dir toggles (the panel's action) and the deferred focus
	// is abandoned — the action runs without moving focus.
	m.panelClickAction(regionProject, 2)
	if m.projectExpanded["cmd/"] != false {
		t.Errorf("double click should collapse the open dir, got %v", m.projectExpanded["cmd/"])
	}
	if m.panelClickPending.valid {
		t.Error("double click must consume the deferred focus")
	}
	if m.panelFocus != regionNone {
		t.Errorf("double click on a dir must never move focus, got %v", m.panelFocus)
	}
}

func TestPanelClickCadence_GitHeader_DoublePreviewsWithoutFocus(t *testing.T) {
	dir := scratchRepo(t)
	m := dragTestModel()
	m.st.cwd = dir
	m.gitLines, m.gitIDs = buildGitPanelLines(gitRowsFixture(), "main", 2, "", gitPanelInner)

	idx := -1
	for i, id := range m.gitIDs {
		if id == worktreeID {
			idx = i
		}
	}
	if idx < 0 {
		t.Fatalf("worktree header id not found in %v", m.gitIDs)
	}

	// click1: focus deferred, no preview.
	if cmd := m.panelClickAction(regionGit, idx); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.attached != nil {
		t.Error("single click on the git header must not preview")
	}
	gen1 := m.panelClickGen

	// click2: the working-tree preview, focus never moved.
	if cmd := m.panelClickAction(regionGit, idx); cmd == nil || m.attached == nil {
		t.Fatalf("double click should attach the working tree, got attached=%v", m.attached)
	}
	if m.attached.label != "working tree" {
		t.Errorf("label = %q, want %q", m.attached.label, "working tree")
	}
	if m.panelClickPending.valid || m.panelFocus != regionNone {
		t.Errorf("double click must abandon focus (pending=%v focus=%v)",
			m.panelClickPending.valid, m.panelFocus)
	}
	nm, _ := m.Update(panelClickFocusMsg{gen: gen1})
	if nm.(model).panelFocus != regionNone {
		t.Error("stale focus timer must be ignored after the double click")
	}
}

// TestPanelClick_Mouse_PressDefersFocus_KeyFlushes: through the real mouse
// path — pressing in a side panel does NOT move focus (repl.go's press
// branch defers for every panel); the deferred click focuses on the first
// key, before that key is routed.
func TestPanelClick_Mouse_PressDefersFocus_KeyFlushes(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelProject = true

	projX := probeRegionX(t, &m, regionProject)
	y, _ := m.viewportRows()

	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	if m.panelFocus != regionNone {
		t.Fatalf("press in the files panel must defer focus, got %v", m.panelFocus)
	}
	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mouseRelease}.msg())
	if !m.panelClickPending.valid {
		t.Fatal("the release should arm the deferred click")
	}

	// Any key flushes the pending focus first — then routes.
	nm, _ := m.handleKey(tea.KeyPressMsg{Code: tea.KeyPgDown})
	got := nm.(model)
	if got.panelFocus != regionProject {
		t.Errorf("key must land the deferred focus, got %v", got.panelFocus)
	}
	if got.panelClickPending.valid {
		t.Error("flush must consume the pending click")
	}
	// And the orphaned timer stays inert.
	nm, _ = got.Update(panelClickFocusMsg{gen: got.panelClickGen})
	if nm.(model).panelFocus != regionProject {
		t.Error("post-flush timer must not change focus")
	}
}

// TestPanelClick_Mouse_OtherRegionPressCancelsPending: a press anywhere
// else (the main area) takes focus for itself and supersedes the deferred
// files/git click — its stale timer can never jump focus later.
func TestPanelClick_Mouse_OtherRegionPressCancelsPending(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelProject = true

	projX := probeRegionX(t, &m, regionProject)
	mainX := probeRegionX(t, &m, regionNone)
	y, _ := m.viewportRows()

	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mouseRelease}.msg())
	if !m.panelClickPending.valid {
		t.Fatal("the files click should be armed")
	}
	gen := m.panelClickGen

	m.handleMouse(mouseEvent{X: mainX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	if m.panelClickPending.valid {
		t.Error("a press in the main area must cancel the deferred files click")
	}
	if m.panelFocus != regionNone {
		t.Fatalf("main-area press focuses main, got %v", m.panelFocus)
	}
	nm, _ := m.Update(panelClickFocusMsg{gen: gen})
	if nm.(model).panelFocus != regionNone {
		t.Error("the cancelled click's timer must never move focus")
	}
}

// TestPanelClick_Mouse_DragSelect_NeverChangesFocus: a real drag
// (selection) in any panel is not a click at all — press and release are
// not consistent (the movement IS the gesture) — so it must never move the
// keyboard target and never arms the cadence.
func TestPanelClick_Mouse_DragSelect_NeverChangesFocus(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelProject = true

	projX := probeRegionX(t, &m, regionProject)
	y, _ := m.viewportRows()

	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	m.handleMouse(mouseEvent{X: projX, Y: y + 1, Button: tea.MouseLeft, Action: mouseMotion}.msg())
	m.handleMouse(mouseEvent{X: projX, Y: y + 1, Button: tea.MouseLeft, Action: mouseRelease}.msg())

	if m.panelFocus != regionNone {
		t.Errorf("drag-release must never move focus, got %v", m.panelFocus)
	}
	if m.panelClickPending.valid {
		t.Error("a drag must not arm the click cadence")
	}
	if m.attached != nil {
		t.Error("a drag must not run the panel's action")
	}
}

// TestPanelClick_Mouse_HeldReleaseIsNotAClick: press and release must be
// close together for the release to count as a single click — keeping the
// button down long enough is not a normal click, so it neither defers nor
// lands focus.
func TestPanelClick_Mouse_HeldReleaseIsNotAClick(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelProject = true

	projX := probeRegionX(t, &m, regionProject)
	y, _ := m.viewportRows()

	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	m.panelPressAt = m.panelPressAt.Add(-2 * panelDoubleClickWindow) // held down
	m.handleMouse(mouseEvent{X: projX, Y: y, Button: tea.MouseLeft, Action: mouseRelease}.msg())

	if m.panelClickPending.valid {
		t.Error("a held-down release must not arm the click cadence")
	}
	if m.panelFocus != regionNone {
		t.Errorf("a held-down release must not move focus, got %v", m.panelFocus)
	}
}

// Uniformity: NO panel focuses on press anymore — every side panel runs
// the same cadence (single = deferred focus, double = action, never focus).
// The main area stays immediate: a press there focuses it and supersedes
// any pending deferred panel click; its stale timer can never jump focus.
func TestPanelClick_Mouse_PressDefersEveryPanel_MainImmediate(t *testing.T) {
	m := newTestModelForPanels(t)
	m.width, m.height = 400, 40
	m.panelMemory = true

	memX := probeRegionX(t, &m, regionMemory)
	mainX := probeRegionX(t, &m, regionNone)
	y, _ := m.viewportRows()

	m.handleMouse(mouseEvent{X: memX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	m.handleMouse(mouseEvent{X: memX, Y: y, Button: tea.MouseLeft, Action: mouseRelease}.msg())
	if !m.panelClickPending.valid {
		t.Fatal("the memory click should be armed")
	}
	if m.focusedPanel() != regionNone {
		t.Fatalf("memory press must defer focus, got %v", m.focusedPanel())
	}
	gen := m.panelClickGen

	m.handleMouse(mouseEvent{X: mainX, Y: y, Button: tea.MouseLeft, Action: mousePress}.msg())
	if m.panelClickPending.valid {
		t.Error("a press in the main area must cancel the deferred memory click")
	}
	if m.focusedPanel() != regionNone {
		t.Fatalf("main-area press focuses main, got %v", m.focusedPanel())
	}
	nm, _ := m.Update(panelClickFocusMsg{gen: gen})
	if nm.(model).panelFocus != regionNone {
		t.Error("the cancelled click's timer must never move focus")
	}
}

// --- uniform cadence: every panel's action lives on the double click ---

func TestPanelClickCadence_Memory_SingleDefers_DoubleExpands(t *testing.T) {
	mem, err := memory.NewStoreParams(t.TempDir(), "s1", memory.Params{})
	if err != nil {
		t.Fatalf("memory.NewStoreParams: %v", err)
	}
	m := dragTestModel()
	m.mem = mem
	m.st.sess.CurrentNeed = "the goal"

	_, ids := memoryPanelRows(m.mem, memoryPanelInner, m.currentSessionBricks(), "")
	idx := -1
	for i, id := range ids {
		if id == "need" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("need brick row not found in ids %v", ids)
	}

	if cmd := m.panelClickAction(regionMemory, idx); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.memoryExpanded != "" {
		t.Errorf("single click must not expand, got %q", m.memoryExpanded)
	}
	if m.panelFocus != regionNone || !m.panelClickPending.valid {
		t.Fatalf("click1 must defer focus (focus=%v pending=%v)", m.panelFocus, m.panelClickPending.valid)
	}

	m.panelClickAction(regionMemory, idx)
	if m.memoryExpanded != "need" {
		t.Errorf("double click should expand the need brick, got %q", m.memoryExpanded)
	}
	if m.panelClickPending.valid || m.panelFocus != regionNone {
		t.Errorf("double click must abandon focus (pending=%v focus=%v)", m.panelClickPending.valid, m.panelFocus)
	}
}

func TestPanelClickCadence_Tasks_SingleDefers_DoubleExpands(t *testing.T) {
	m, ts := tasksPanelClickModel(t)
	task, err := ts.Create("click cadence task", nil)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	row := taskRowLineIndex(t, ts, "", task.ID)

	if cmd := m.panelClickAction(regionTasks, row); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.taskExpanded != "" {
		t.Errorf("single click must not expand, got %q", m.taskExpanded)
	}

	m.panelClickAction(regionTasks, row)
	if m.taskExpanded != task.ID {
		t.Errorf("double click should expand the task, got %q", m.taskExpanded)
	}
	if m.panelClickPending.valid || m.panelFocus != regionNone {
		t.Errorf("double click must abandon focus (pending=%v focus=%v)", m.panelClickPending.valid, m.panelFocus)
	}
}

func TestPanelClickCadence_Background_SingleDefers_DoubleAttaches(t *testing.T) {
	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	m.agents.backgroundMgr = mgr
	spawnJobWithLiveContent(t, mgr, "cadence job", "cadence payload")

	// backgroundPanelLayout: line 0 title, 1 blank, 2 section header, 3 job.
	if cmd := m.panelClickAction(regionBackground, 3); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.attached != nil {
		t.Errorf("single click must not attach, got %+v", m.attached)
	}
	if m.panelFocus != regionNone {
		t.Errorf("single click must defer focus, got %v", m.panelFocus)
	}

	if cmd := m.panelClickAction(regionBackground, 3); cmd == nil || m.attached == nil {
		t.Fatalf("double click should attach the live buffer, got attached=%v", m.attached)
	}
	if m.attached.kind != attachBackground {
		t.Errorf("attached.kind = %v, want attachBackground", m.attached.kind)
	}
	if m.panelClickPending.valid || m.panelFocus != regionNone {
		t.Errorf("double click must abandon focus (pending=%v focus=%v)", m.panelClickPending.valid, m.panelFocus)
	}
}

func TestPanelClickCadence_Workflow_SingleDefers_DoubleAttaches(t *testing.T) {
	m := attachTestModel()
	m.workflowState = &workflow.State{WorkflowName: "cadence", Role: "designer"}
	m.workflowState.LiveBuffer().Append([]byte("stage output"))

	if cmd := m.panelClickAction(regionWorkflow, 0); cmd == nil {
		t.Fatal("click1 must arm the focus timer")
	}
	if m.attached != nil {
		t.Errorf("single click must not attach, got %+v", m.attached)
	}

	if cmd := m.panelClickAction(regionWorkflow, 0); cmd == nil || m.attached == nil {
		t.Fatalf("double click should attach the workflow buffer, got attached=%v", m.attached)
	}
	if m.attached.kind != attachWorkflow {
		t.Errorf("attached.kind = %v, want attachWorkflow", m.attached.kind)
	}
	if m.panelClickPending.valid || m.panelFocus != regionNone {
		t.Errorf("double click must abandon focus (pending=%v focus=%v)", m.panelClickPending.valid, m.panelFocus)
	}
}
