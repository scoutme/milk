package main

import (
	"context"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

// attachTestModel is dragTestModel plus a non-nil session, needed by any test
// that actually triggers an attach: startAttach reaches viewportHeight() →
// headerBar(), which dereferences m.st.sess — dragTestModel alone leaves that
// nil, fine for the panel-selection tests it was written for, not for these.
func attachTestModel() *model {
	m := dragTestModel()
	m.st.sess = &session.Session{}
	return m
}

// spawnJobWithLiveContent creates and waits for a completed job whose Live
// buffer already contains text, for tests that need an attach target without
// caring about the job's actual execution.
func spawnJobWithLiveContent(t *testing.T, mgr *local.Manager, label, content string) *local.Job {
	t.Helper()
	done := make(chan *local.Job, 1)
	mgr.SetOnDone(func(j *local.Job) { done <- j })
	job := mgr.Spawn(label, "t", "primary", "m", func(_ context.Context, _ string, out io.Writer) (string, session.TokenUsage, error) {
		io.WriteString(out, content) //nolint:errcheck
		return "ok", session.TokenUsage{}, nil
	})
	<-done
	return job
}

func TestHandleBackgroundPanelClick_DoubleClickAttaches(t *testing.T) {
	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	m.agents.backgroundMgr = mgr
	job := spawnJobWithLiveContent(t, mgr, "investigate X", "distinctive job output")

	// backgroundPanelLayout: line 0 title, line 1 blank, line 2 the "agents
	// (1)" section header, line 3 the only job.
	if cmd := m.handleBackgroundPanelClick(3); cmd != nil {
		t.Fatalf("first click should only arm, not return a command")
	}
	if m.attached != nil {
		t.Fatalf("expected no attach after a single click, got %+v", m.attached)
	}

	cmd := m.handleBackgroundPanelClick(3)
	if m.attached == nil {
		t.Fatal("expected attach after second click within 400ms")
	}
	if m.attached.kind != attachBackground || m.attached.jobID != job.ID {
		t.Fatalf("attached = %+v, want kind=attachBackground jobID=%s", m.attached, job.ID)
	}
	if got := m.attached.buf.Snapshot(); got != "distinctive job output" {
		t.Fatalf("attached.buf.Snapshot() = %q, want the job's live content", got)
	}
	if cmd == nil {
		t.Fatal("expected startAttach to return the refresh-tick command")
	}
}

func TestHandleBackgroundPanelClick_OutOfRangeIsNoop(t *testing.T) {
	m := dragTestModel()
	mgr := local.NewManager(context.Background(), 1)
	m.agents.backgroundMgr = mgr
	spawnJobWithLiveContent(t, mgr, "job", "content")

	for _, lineIdx := range []int{-1, 0, 1, 99} {
		if cmd := m.handleBackgroundPanelClick(lineIdx); cmd != nil {
			t.Errorf("lineIdx=%d: expected nil cmd for an out-of-range row", lineIdx)
		}
		if m.attached != nil {
			t.Fatalf("lineIdx=%d: expected no attach for an out-of-range row, got %+v", lineIdx, m.attached)
		}
	}
}

func TestHandleBackgroundPanelClick_NilManagerIsNoop(t *testing.T) {
	m := dragTestModel()
	if cmd := m.handleBackgroundPanelClick(2); cmd != nil {
		t.Error("expected nil cmd with no background manager configured")
	}
	if m.attached != nil {
		t.Fatal("expected no attach with no background manager configured")
	}
}

func TestHandleWorkflowPanelClick_DoubleClickAttaches(t *testing.T) {
	m := attachTestModel()
	m.workflowState = &workflow.State{WorkflowName: "dev", Role: "designer"}
	m.workflowState.LiveBuffer().Append([]byte("distinctive stage output"))

	if cmd := m.handleWorkflowPanelClick(); cmd != nil {
		t.Fatal("first click should only arm, not return a command")
	}
	if m.attached != nil {
		t.Fatal("expected no attach after a single click")
	}

	cmd := m.handleWorkflowPanelClick()
	if m.attached == nil {
		t.Fatal("expected attach after second click within 400ms")
	}
	if m.attached.kind != attachWorkflow {
		t.Fatalf("attached.kind = %v, want attachWorkflow", m.attached.kind)
	}
	if got := m.attached.buf.Snapshot(); got != "distinctive stage output" {
		t.Fatalf("attached.buf.Snapshot() = %q, want the workflow's live content", got)
	}
	if cmd == nil {
		t.Fatal("expected startAttach to return the refresh-tick command")
	}
}

func TestHandleWorkflowPanelClick_NilStateIsNoop(t *testing.T) {
	m := dragTestModel()
	if cmd := m.handleWorkflowPanelClick(); cmd != nil {
		t.Error("expected nil cmd with no workflow running")
	}
	if m.attached != nil {
		t.Fatal("expected no attach with no workflow running")
	}
}

func TestHandleAttachKey_EscDetaches(t *testing.T) {
	m := attachTestModel()
	m.busy = true // an unrelated field the Esc handler must not touch
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.attached = &attachState{kind: attachBackground, jobID: job.ID, buf: job.Live}

	updated, _ := m.handleAttachKey(tea.KeyPressMsg{Code: tea.KeyEscape})
	mm := updated.(model)

	if mm.attached != nil {
		t.Fatalf("expected Esc to clear attached, got %+v", mm.attached)
	}
	if !mm.busy {
		t.Error("Esc-to-detach must not touch unrelated model state (m.busy)")
	}
}

func TestHandleAttachKey_OtherKeysAreSwallowed(t *testing.T) {
	m := dragTestModel()
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.attached = &attachState{kind: attachBackground, jobID: job.ID, buf: job.Live}

	updated, cmd := m.handleAttachKey(tea.KeyPressMsg{Code: []rune("j")[0], Text: "j"})
	mm := updated.(model)

	if mm.attached == nil {
		t.Fatal("expected a non-Esc key to leave attach state untouched")
	}
	if cmd != nil {
		t.Error("expected no command from a swallowed key")
	}
}

// TestStatusAgent_ShowsAttachedTarget verifies the status bar names what's
// currently attached (label truncated past 40 chars) — the persistent
// indicator that stays visible after the attach view's own header line
// (attach.go's "── attached: … ──") scrolls out of sight.
func TestStatusAgent_ShowsAttachedTarget(t *testing.T) {
	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.attached = &attachState{kind: attachBackground, jobID: job.ID, label: "investigate the flaky test", buf: job.Live}

	got := m.statusAgent()
	if !strings.Contains(got, "investigate the flaky test") {
		t.Errorf("statusAgent() = %q, want it to name the attached target", got)
	}
	if !strings.Contains(got, "Esc to detach") {
		t.Errorf("statusAgent() = %q, want a detach hint", got)
	}

	long := strings.Repeat("x", 80)
	m.attached.label = long
	got = m.statusAgent()
	if strings.Contains(got, long) {
		t.Errorf("statusAgent() = %q, want the label truncated", got)
	}
}

// TestStatusAgent_PendingPermTakesPriorityOverAttached verifies a pending
// permission prompt still surfaces in the status bar even while attached —
// the more urgent, actionable state wins.
func TestStatusAgent_PendingPermTakesPriorityOverAttached(t *testing.T) {
	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.attached = &attachState{kind: attachBackground, jobID: job.ID, label: "job", buf: job.Live}
	m.pendingPerm = &permRequestMsg{label: "[allow bash?]"}

	got := m.statusAgent()
	if !strings.Contains(got, "[allow bash?]") {
		t.Errorf("statusAgent() = %q, want the pending permission prompt to take priority", got)
	}
}

// TestUpdate_PendingPermReachesItsHandlerWhileAttached is the regression test
// for the key-routing bug this fix addresses: attach's key handling used to
// be checked before every pending prompt/wizard, so a permission prompt (or
// any other state needing a real decision) arising while attached would be
// unreachable — only Esc (detach) worked, every other key was swallowed by
// handleAttachKey instead of reaching handlePermKey.
func TestUpdate_PendingPermReachesItsHandlerWhileAttached(t *testing.T) {
	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.attached = &attachState{kind: attachBackground, jobID: job.ID, label: "job", buf: job.Live}
	m.pendingPerm = &permRequestMsg{label: "[allow bash?]", respCh: make(chan string, 1)}

	updated, _ := m.Update(tea.KeyPressMsg{Code: tea.KeyEnter})
	mm := updated.(model)

	if mm.pendingPerm != nil {
		t.Fatalf("expected Enter to resolve the pending permission prompt, got %+v", mm.pendingPerm)
	}
	if mm.attached == nil {
		t.Error("expected attach state to survive answering an unrelated permission prompt")
	}
}

// TestView_PendingPermShowsMainTranscriptNotAttachBuffer is the regression
// test for a gap found live: a permission prompt is printed into the main
// transcript, which the attach view would otherwise be covering — the user
// would see only the attach buffer and the status bar's "[allow?]" hint,
// with no way to read what they're actually being asked. View() must fall
// back to the main transcript whenever a pending prompt needs the user's
// attention, even while m.attached is still set (attach reappears
// automatically once the prompt resolves — nothing here calls detachAttach).
func TestView_PendingPermShowsMainTranscriptNotAttachBuffer(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.appendTranscript("please allow this tool call?\n")

	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "attached buffer sentinel")
	m.startAttach(attachBackground, job.ID, "job", job.Live)
	m.pendingPerm = &permRequestMsg{label: "[allow?]", respCh: make(chan string, 1)}

	view := m.viewString()
	if !strings.Contains(view, "please allow this tool call?") {
		t.Errorf("expected the pending permission prompt to be visible, got:\n%s", view)
	}
	if strings.Contains(view, "attached buffer sentinel") {
		t.Errorf("expected the attach buffer to be hidden while a permission prompt is pending, got:\n%s", view)
	}

	// Resolve the prompt; the attach view should reappear without needing
	// another double-click.
	m.pendingPerm = nil
	view = m.viewString()
	if !strings.Contains(view, "attached buffer sentinel") {
		t.Errorf("expected the attach view to reappear once the prompt resolved, got:\n%s", view)
	}
}

// TestStatusAgent_DetachHintIsYellowNotDim verifies the "Esc to detach"
// status-bar hint is highlighted (yellow, matching the busyHint/loopWarning
// convention) rather than dimmed — it needs to stay legible since it's the
// only persistent reminder of how to get back once the attach view's own
// header line has scrolled out of sight.
func TestStatusAgent_DetachHintIsYellowNotDim(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := attachTestModel()
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.attached = &attachState{kind: attachBackground, jobID: job.ID, label: "job", buf: job.Live}

	got := m.statusAgent()
	if !strings.Contains(got, yellow("[Esc to detach]")) {
		t.Errorf("statusAgent() = %q, want the detach hint highlighted in yellow, not dim", got)
	}
}

// TestTintBlock_PadsShortLinesAndAppliesBackground verifies every line gets
// padded to width before the background code wraps it (so the tint spans the
// full row, not just the underlying text) and closes with a reset.
func TestTintBlock_PadsShortLinesAndAppliesBackground(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	bg := "\033[48;2;24;24;28m"
	got := tintBlock("short\nlonger line here", 20, bg)

	lines := strings.Split(got, "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %q", len(lines), got)
	}
	for i, line := range lines {
		if !strings.HasPrefix(line, bg) {
			t.Errorf("line %d = %q, want it to start with the background code", i, line)
		}
		if !strings.HasSuffix(line, ansiReset) {
			t.Errorf("line %d = %q, want it to end with a reset", i, line)
		}
		if plain := ansi.Strip(line); utf8.RuneCountInString(plain) != 20 {
			t.Errorf("line %d visible width = %d, want padded to 20 (%q)", i, utf8.RuneCountInString(plain), plain)
		}
	}
}

// TestSyncAttachedContent_AppliesBackgroundTintWhenTTY is the integration
// check that startAttach's real content actually carries the tint, not just
// that the tintBlock helper works in isolation.
func TestSyncAttachedContent_AppliesBackgroundTintWhenTTY(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := layoutTestModel(t, 100, 30)
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.startAttach(attachBackground, job.ID, "job", job.Live)

	if !strings.Contains(m.attached.vp.View(), "\033[48;2;") {
		t.Error("expected the attach viewport's rendered content to include a background-tint escape code")
	}
}

func TestView_AttachedRendersLiveBufferNotMainTranscript(t *testing.T) {
	m := layoutTestModel(t, 120, 40)
	m.appendTranscript("main transcript sentinel\n")

	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "attached buffer sentinel")
	cmd := m.startAttach(attachBackground, job.ID, "job (completed)", job.Live)
	if cmd == nil {
		t.Fatal("expected startAttach to return the refresh-tick command")
	}

	view := m.viewString()
	if !strings.Contains(view, "attached buffer sentinel") {
		t.Errorf("expected attach view to render the live buffer content, got:\n%s", view)
	}
	if strings.Contains(view, "main transcript sentinel") {
		t.Errorf("expected attach view to hide the main transcript, got:\n%s", view)
	}
}

func TestHandleResize_ResizesAttachedViewport(t *testing.T) {
	m := layoutTestModel(t, 100, 40)
	mgr := local.NewManager(context.Background(), 1)
	job := spawnJobWithLiveContent(t, mgr, "job", "content")
	m.startAttach(attachBackground, job.ID, "job", job.Live)

	updated, _ := m.handleResize(tea.WindowSizeMsg{Width: 160, Height: 50})
	mm := updated.(model)

	if mm.attached == nil {
		t.Fatal("expected attach state to survive a resize")
	}
	if mm.attached.vp.Width() != mm.vpWidth() {
		t.Errorf("attached.vp.Width = %d, want %d (mm.vpWidth())", mm.attached.vp.Width(), mm.vpWidth())
	}
	if mm.attached.vp.Height() != mm.viewportHeight() {
		t.Errorf("attached.vp.Height = %d, want %d (mm.viewportHeight())", mm.attached.vp.Height(), mm.viewportHeight())
	}
}

func TestHandleMouse_WheelScrollsAttachedViewportNotMain(t *testing.T) {
	m := attachTestModel()
	m.height = 10
	// Enough lines in both viewports to make wheel scrolling observable.
	var longMain strings.Builder
	for range 50 {
		longMain.WriteString("main line\n")
	}
	m.vp.SetContent(longMain.String())
	m.vp.GotoBottom()
	mainOffsetBefore := m.vp.YOffset()

	mgr := local.NewManager(context.Background(), 1)
	var content strings.Builder
	for range 50 {
		content.WriteString("attach line\n")
	}
	job := spawnJobWithLiveContent(t, mgr, "job", content.String())
	m.startAttach(attachBackground, job.ID, "job", job.Live)
	m.attached.vp.GotoBottom()
	attachOffsetBefore := m.attached.vp.YOffset()

	updated, _ := m.handleMouse(mouseEvent{X: 5, Y: 5, Button: tea.MouseWheelUp}.msg())
	mm := updated.(*model)

	if mm.vp.YOffset() != mainOffsetBefore {
		t.Errorf("main viewport YOffset changed from %d to %d — wheel should have scrolled the attached view instead", mainOffsetBefore, mm.vp.YOffset())
	}
	if mm.attached.vp.YOffset() >= attachOffsetBefore {
		t.Errorf("attached viewport YOffset = %d, want less than %d (scrolled up)", mm.attached.vp.YOffset(), attachOffsetBefore)
	}
}

// A workflow's reasoning arrives as one dim span per streamed chunk, and the
// wrap splits spans across rows: every body row must still open its own dim —
// the attach tint closes each row with a reset, so a row that merely inherited
// dim from the one above would render white.
func TestAttachContentLines_WrappedDimChunksStayDim(t *testing.T) {
	old := isTTY
	isTTY = true
	t.Cleanup(func() { isTTY = old })

	m := layoutTestModel(t, 60, 30)
	mgr := local.NewManager(context.Background(), 1)
	var content strings.Builder
	for _, w := range strings.Fields(strings.Repeat("evaluator checks each item in isolation from the others and ", 8)) {
		content.WriteString(dim(w + " "))
	}
	job := spawnJobWithLiveContent(t, mgr, "job", content.String())
	m.startAttach(attachBackground, job.ID, "job", job.Live)

	body := m.attachContentLines()[2:] // header line + blank separator
	if len(body) < 3 {
		t.Fatalf("expected the reasoning to wrap, got %d rows", len(body))
	}
	for i, row := range body {
		if ansi.Strip(row) == "" {
			continue
		}
		if !strings.HasPrefix(row, ansiDim) {
			t.Errorf("row %d does not open dim: %q", i, row)
		}
		if !rowBalanced(row) {
			t.Errorf("row %d leaves styling open: %q", i, row)
		}
	}
}
