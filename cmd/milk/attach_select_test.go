package main

// Regression tests for issue #207: mouse selection in the attach view must
// act on the attach viewport's own content lines (the "── attached: … ──"
// header included), never on the hidden main transcript — selecting, copying,
// and the status bar all have to describe what is actually on screen, and a
// pending prompt must hand selection back to the transcript.

import (
	"context"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/muesli/termenv"
	"github.com/scoutme/milk/internal/agent/local"
)

// attachSelTestModel returns a model with an attach view showing content,
// viewport sized like a real session, and all selection state freshly cleared.
func attachSelTestModel(t *testing.T, content string) *model {
	t.Helper()
	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", content)
	m.startAttach(attachBackground, job.ID, "job (completed)", job.Live)
	// startAttach clears selection state; assert the invariant the fix relies on.
	if m.attachSelAnchorLine != -1 {
		t.Fatalf("startAttach must reset attach selection, got anchor %d", m.attachSelAnchorLine)
	}
	return m
}

// attachClick sends a left-button press at the given screen row (content row 0
// starts at vpRowStart = 2).
func attachClick(m *model, x, y int) tea.Cmd {
	_, cmd := m.handleMouse(tea.MouseMsg(tea.MouseEvent{
		X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress,
	}))
	return cmd
}

func attachDrag(m *model, x, y int) {
	m.handleMouse(tea.MouseMsg(tea.MouseEvent{
		X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionMotion,
	}))
}

func attachRelease(m *model, x, y int) {
	m.handleMouse(tea.MouseMsg(tea.MouseEvent{
		X: x, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionRelease,
	}))
}

// TestAttachSelection_ExtractsAttachedTextNotTranscript is the core repro:
// press+drag+release over the attached buffer must select the attached text
// (header offset included in the coordinate space) and leave the transcript's
// selection state untouched.
func TestAttachSelection_ExtractsAttachedTextNotTranscript(t *testing.T) {
	m := attachSelTestModel(t, "SELECTME from the attached buffer\nsecond line of output")

	// Line 0 is the header, line 1 blank, line 2 is the body's first line.
	const bodyLine = 2
	attachClick(m, 0, 2+bodyLine) // Y = vpRowStart(2) + content line
	attachDrag(m, 8, 2+bodyLine)
	attachRelease(m, 8, 2+bodyLine)

	if m.attachSelText != "SELECTME" {
		t.Errorf("attachSelText = %q, want %q", m.attachSelText, "SELECTME")
	}
	if m.selAnchorLine != -1 || m.selEndLine != -1 || m.selText != "" {
		t.Errorf("transcript selection must stay untouched while attached, got anchor=%d end=%d text=%q",
			m.selAnchorLine, m.selEndLine, m.selText)
	}
}

// TestAttachSelection_TranscriptSelectionSurvivesAttachSelection covers the
// explicit regression requirement: a transcript selection made *before*
// attaching must survive attach-view interactions and reappear on detach.
func TestAttachSelection_TranscriptSelectionSurvivesAttachSelection(t *testing.T) {
	m := attachSelTestModel(t, "attached body")
	m.selAnchorLine = 5
	m.selAnchorCol = 3
	m.selEndLine = 6
	m.selEndCol = 9
	m.selText = "prior transcript text"

	attachClick(m, 0, 4)
	attachDrag(m, 6, 4)
	attachRelease(m, 6, 4)

	if m.selAnchorLine != 5 || m.selEndLine != 6 || m.selText != "prior transcript text" {
		t.Errorf("transcript selection changed while attached: anchor=%d end=%d text=%q",
			m.selAnchorLine, m.selEndLine, m.selText)
	}
	if m.attachSelText == "" {
		t.Fatal("expected an attach selection to have been made")
	}

	m.detachAttach()
	if m.attachSelAnchorLine != -1 {
		t.Errorf("detachAttach must clear the attach selection, got anchor %d", m.attachSelAnchorLine)
	}
	if m.selAnchorLine != 5 || m.selText != "prior transcript text" {
		t.Errorf("transcript selection must reappear untouched after detach: anchor=%d text=%q",
			m.selAnchorLine, m.selText)
	}
}

// TestAttachSelection_HighlightReachesAttachViewport verifies the highlight is
// painted into the attach viewport's own content (syncAttachedContent), not
// into the hidden transcript. Forces a color profile so the reverse-video
// escape actually renders under `go test`.
func TestAttachSelection_HighlightReachesAttachViewport(t *testing.T) {
	oldProfile := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(termenv.TrueColor)
	t.Cleanup(func() { lipgloss.SetColorProfile(oldProfile) })

	m := attachSelTestModel(t, "highlight target text")
	attachClick(m, 0, 4)
	attachDrag(m, 8, 4)

	got := m.attached.vp.View()
	if !strings.Contains(got, "\x1b[7m") {
		t.Errorf("attach viewport content must carry the reverse-video selection highlight, got %q", got)
	}
	// The selected word must still be present (highlight wraps it, doesn't eat it).
	if plain := stripANSI(got); !strings.Contains(plain, "highlight") {
		t.Errorf("attach viewport lost its text content: %q", plain)
	}
	// The transcript must not be rebuilt with the selection.
	if m.selAnchorLine != -1 {
		t.Errorf("transcript selection leaked: anchor=%d", m.selAnchorLine)
	}
}

// TestAttachSelection_RightClickCopesAttachedText: right-click copies what is
// on screen (the attach selection), not a stale transcript selection.
func TestAttachSelection_RightClickCopiesAttachedText(t *testing.T) {
	m := attachSelTestModel(t, "COPYME attached line")
	// A stale transcript selection from before attaching must not win.
	m.selAnchorLine = 5
	m.selEndLine = 5
	m.selEndCol = 4
	m.selText = "stale transcript"

	attachClick(m, 0, 4)
	attachDrag(m, 6, 4)
	attachRelease(m, 6, 4)
	if m.attachSelText != "COPYME" {
		t.Fatalf("attachSelText = %q, want COPYME", m.attachSelText)
	}

	m.handleMouse(tea.MouseMsg(tea.MouseEvent{
		X: 6, Y: 4, Button: tea.MouseButtonRight, Action: tea.MouseActionPress,
	}))

	if !strings.Contains(m.copyFeedback, "copied 6 chars") {
		t.Errorf("copyFeedback = %q, want the attach selection (6 chars) copied", m.copyFeedback)
	}
	if m.attachSelAnchorLine != -1 {
		t.Errorf("attach selection should be consumed by copy, anchor=%d", m.attachSelAnchorLine)
	}
	if m.selText != "stale transcript" {
		t.Errorf("stale transcript selection must not be copied or cleared while attached, got %q", m.selText)
	}
}

// TestAttachSelection_PendingPromptReturnsSelectionToTranscript: while a
// pending prompt temporarily re-reveals the transcript (activeViewport
// fallback), a click must select transcript lines, not attach lines.
func TestAttachSelection_PendingPromptReturnsSelectionToTranscript(t *testing.T) {
	m := attachSelTestModel(t, "attached body")
	m.pendingPerm = &permRequestMsg{label: "[allow?]", respCh: make(chan string, 1)}

	attachClick(m, 0, 4) // same coordinates as an attach click would use

	if m.selAnchorLine != 2 {
		t.Errorf("transcript selAnchorLine = %d, want 2 (clicked row over the revealed transcript)", m.selAnchorLine)
	}
	if m.attachSelAnchorLine != -1 {
		t.Errorf("attach selection must not start under a pending prompt, anchor=%d", m.attachSelAnchorLine)
	}
}

// TestAttachSelection_StatusBarNamesTheAttachView: while attached and
// selecting, the status bar must report the attach view's coordinates — not
// the hidden transcript's.
func TestAttachSelection_StatusBarNamesTheAttachView(t *testing.T) {
	m := attachSelTestModel(t, "body")
	// Stale transcript selection on screen behind the attach view.
	m.selAnchorLine = 9
	m.selEndLine = 9
	m.selDragging = true

	attachClick(m, 0, 4)
	attachDrag(m, 4, 4) // mid-drag: the status should report live coordinates

	bar := stripANSI(m.statusBar())
	if !strings.Contains(bar, "selecting attached view") {
		t.Errorf("status bar must name the attach view while selecting in it, got %q", bar)
	}
	if strings.Contains(bar, "selecting: line") {
		t.Errorf("status bar reported transcript coordinates while attached: %q", bar)
	}
}

// TestHandleAttachKey_EscClearsSelectionBeforeDetach: esc follows the
// transcript convention — first press clears the selection, second detaches.
func TestHandleAttachKey_EscClearsSelectionBeforeDetach(t *testing.T) {
	m := attachSelTestModel(t, "body")
	attachClick(m, 0, 4)
	attachDrag(m, 6, 4) // left dragging: anchor set, end follows motion

	updated, _ := m.handleAttachKey(tea.KeyMsg{Type: tea.KeyEsc})
	mm := updated.(model)
	if mm.attached == nil {
		t.Fatal("first Esc must clear the selection, not detach")
	}
	if mm.attachSelAnchorLine != -1 {
		t.Errorf("first Esc must clear the attach selection, anchor=%d", mm.attachSelAnchorLine)
	}

	updated, _ = mm.handleAttachKey(tea.KeyMsg{Type: tea.KeyEsc})
	mm = updated.(model)
	if mm.attached != nil {
		t.Error("second Esc must detach")
	}
}

// TestHandleAttachKey_CtrlCCopiesSelection: handleCtrlC is unreachable while
// attached (keys route to handleAttachKey first), so ctrl+c needs its own
// copy branch there.
func TestHandleAttachKey_CtrlCCopiesSelection(t *testing.T) {
	m := attachSelTestModel(t, "CTRLCOPY body text")
	attachClick(m, 0, 4)
	attachDrag(m, 8, 4)
	attachRelease(m, 8, 4)
	if m.attachSelText != "CTRLCOPY" {
		t.Fatalf("attachSelText = %q, want CTRLCOPY", m.attachSelText)
	}

	updated, cmd := m.handleAttachKey(tea.KeyMsg{Type: tea.KeyCtrlC})
	mm := updated.(model)
	if !strings.Contains(mm.copyFeedback, "copied 8 chars") {
		t.Errorf("copyFeedback = %q, want the attach selection copied", mm.copyFeedback)
	}
	if mm.attachSelAnchorLine != -1 {
		t.Errorf("copy must consume the selection, anchor=%d", mm.attachSelAnchorLine)
	}
	if cmd == nil {
		t.Error("expected a copyFeedbackClearCmd")
	}
}

// TestAttachSelection_PressClearsPanelSelection keeps the one-live-selection
// invariant across regions: clicking the attach view ends a panel selection.
func TestAttachSelection_PressClearsPanelSelection(t *testing.T) {
	m := attachSelTestModel(t, "body")
	m.panelSelRegion = regionWorkflow
	m.panelSelAnchorLine = 3
	m.panelSelEndLine = 3

	attachClick(m, 0, 4)

	if m.panelSelRegion != regionNone || m.panelSelAnchorLine != -1 {
		t.Errorf("panel selection must be cleared by an attach-view press, region=%d anchor=%d",
			m.panelSelRegion, m.panelSelAnchorLine)
	}
	if m.attachSelAnchorLine != 2 {
		t.Errorf("attach anchor = %d, want line 2", m.attachSelAnchorLine)
	}
}
