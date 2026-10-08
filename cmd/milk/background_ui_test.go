package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// TestUpdate_BackgroundJobDoneMsg_NotifiesToast verifies a completed job
// surfaces as a timestamped notification toast (issue #162 — turn-unrelated
// job lifecycle events don't pollute the transcript), for both success and
// failure, independently of the turn-boundary drainBackgroundJobs path.
//
// Since the background-result-visibility fix, the job's *result* (or failure
// and partial result) is additionally appended to the transcript as a
// renderBackgroundJobDoneBlock block: issue #162's toast-only rule still
// governs the lifecycle notice itself, but the result is the job's content —
// it used to reach only the model's next-turn prompt and could pass by
// completely unseen if the model never reported it.
func TestUpdate_BackgroundJobDoneMsg_NotifiesToast(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	updated, _ := m.Update(backgroundJobDoneMsg{job: &local.Job{Label: "investigate X", Result: "found it"}})
	m2 := updated.(model)
	if !toastMentions(m2, `background agent "investigate X" completed`) {
		t.Errorf("expected a completion toast, got %#v", m2.toastVisible)
	}
	if !strings.Contains(m2.transcript.String(), "found it") {
		t.Errorf("job result must be visible in the transcript, got %q", m2.transcript.String())
	}

	updated2, _ := m2.Update(backgroundJobDoneMsg{job: &local.Job{Label: "investigate Y", Err: errors.New("boom")}})
	m3 := updated2.(model)
	if !toastMentions(m3, `background agent "investigate Y" failed: boom`) {
		t.Errorf("expected a failure toast, got %#v", m3.toastVisible)
	}
	if !strings.Contains(m3.transcript.String(), `failed: boom`) {
		t.Errorf("job failure must be visible in the transcript, got %q", m3.transcript.String())
	}
}

// TestRenderBackgroundJobDoneBlock_TruncatesWithShowHint: a result over the
// shared display budget is head+tail truncated with a /bg show <id> hint to
// the full text; a short one renders inline with no hint.
func TestRenderBackgroundJobDoneBlock_TruncatesWithShowHint(t *testing.T) {
	long := strings.Repeat("x", backgroundJobResultMaxChars+500)
	blk := renderBackgroundJobDoneBlock(&local.Job{ID: "7", Label: "l", Result: long})
	if !strings.Contains(blk, "[... ") || !strings.Contains(blk, "chars omitted") {
		t.Errorf("expected an omission marker in %q", blk)
	}
	if !strings.Contains(blk, "/bg show 7") {
		t.Errorf("expected a /bg show 7 hint in %q", blk)
	}
	short := renderBackgroundJobDoneBlock(&local.Job{ID: "7", Label: "l", Result: "tiny"})
	if !strings.Contains(short, "tiny") {
		t.Errorf("expected the result inline in %q", short)
	}
	if strings.Contains(short, "/bg show") || strings.Contains(short, "omitted") {
		t.Errorf("short result must render inline unhinted, got %q", short)
	}
}

// TestRenderBackgroundJobDoneBlock_FailurePartialResult: a failed job's
// preserved partial work is shown; an empty one doesn't claim any.
func TestRenderBackgroundJobDoneBlock_FailurePartialResult(t *testing.T) {
	withPartial := renderBackgroundJobDoneBlock(&local.Job{ID: "7", Label: "l", Result: "half-done", Err: errors.New("boom")})
	if !strings.Contains(withPartial, "failed: boom") || !strings.Contains(withPartial, "partial result preserved") || !strings.Contains(withPartial, "half-done") {
		t.Errorf("expected failure + partial result block, got %q", withPartial)
	}
	without := renderBackgroundJobDoneBlock(&local.Job{ID: "7", Label: "l", Err: errors.New("boom")})
	if strings.Contains(without, "partial result preserved") {
		t.Errorf("empty partial result must not be advertised, got %q", without)
	}
}

// TestRenderBackgroundJobDoneBlock_StructuredMeta: the optional structured
// result tag's status/files_touched surface in the header and the raw tag is
// stripped from the shown text (same parsing as the model-side drain).
func TestRenderBackgroundJobDoneBlock_StructuredMeta(t *testing.T) {
	blk := renderBackgroundJobDoneBlock(&local.Job{ID: "7", Label: "l",
		Result: "all done\n<result status=\"partial\" files_touched=\"a.go,b.go\"/>"})
	if !strings.Contains(blk, "status=partial") || !strings.Contains(blk, "files_touched=a.go,b.go") {
		t.Errorf("expected structured meta in header, got %q", blk)
	}
	if strings.Contains(blk, "<result") {
		t.Errorf("raw result tag must not be shown, got %q", blk)
	}
	if !strings.Contains(blk, "all done") {
		t.Errorf("expected the result text, got %q", blk)
	}
}

// TestHandleBgCmd_Show covers the /bg show <id> full-result lookup both the
// truncation hint and the "take its output" question target: full uncapped
// text for a known job, a not-found line otherwise.
func TestHandleBgCmd_Show(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 1)
	full := strings.Repeat("R", backgroundJobResultMaxChars+500)
	job := mgr.Spawn("lbl", "task", "user", "m", func(ctx context.Context, jobID string, out io.Writer) (string, session.TokenUsage, error) {
		return full, session.TokenUsage{}, nil
	})
	deadline := time.Now().Add(5 * time.Second)
	for {
		if js := mgr.Jobs(); len(js) > 0 && js[0].Status != local.JobRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background job did not finish")
		}
		time.Sleep(5 * time.Millisecond)
	}

	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)
	updated, _ := m.handleBgCmd("show " + job.ID)
	got := updated.(model).transcript.String()
	if !strings.Contains(got, full) {
		t.Errorf("/bg show must print the full uncapped result, got %d chars", len(got))
	}
	if strings.Contains(got, "omitted") {
		t.Errorf("/bg show must not truncate, got %q", got)
	}

	updated2, _ := updated.(model).handleBgCmd("show nope")
	if miss := updated2.(model).transcript.String(); !strings.Contains(miss, "not found") {
		t.Errorf("expected a not-found line for an unknown id, got %q", miss)
	}
}

// TestStatusBar_BackgroundAgentCount reflects the Manager's ActiveCount and
// disappears once nothing is running.
func TestStatusBar_BackgroundAgentCount(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 1)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)
	m.width = 200

	if strings.Contains(m.statusBar(), "background agent") {
		t.Errorf("expected no background-agent indicator when idle, got %q", m.statusBar())
	}

	release := make(chan struct{})
	mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context, _ string, _ io.Writer) (string, session.TokenUsage, error) {
		<-release
		return "ok", session.TokenUsage{}, nil
	})
	if !strings.Contains(m.statusBar(), "1 background agent running") {
		t.Errorf("expected the status bar to show 1 running, got %q", m.statusBar())
	}
	close(release)
}

// TestMaybeAutoFollowup_IdleAndDone_DispatchesFollowupTurn verifies the fix
// for a real UX gap: an escalation agent promised "I'll follow up
// automatically when they finish" but nothing in the implementation ever
// actually triggered a new turn — results just sat in the Manager's queue
// until the user happened to send another message. When idle and every job
// has finished, backgroundBatchDoneMsg must dispatch a real follow-up turn.
func TestMaybeAutoFollowup_IdleAndDone_DispatchesFollowupTurn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 1)
	seedFinishedJob(t, mgr)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)
	m.pendingBackgroundFollowup = true // simulate: set when the batch finished while busy

	updated, cmd := m.Update(backgroundBatchDoneMsg{})
	m2 := updated.(model)

	if cmd == nil {
		t.Fatal("expected a non-nil tea.Cmd — the follow-up turn should have been dispatched")
	}
	if m2.pendingBackgroundFollowup {
		t.Error("expected pendingBackgroundFollowup to be cleared once dispatched")
	}
	if !m2.busy {
		t.Error("expected the follow-up turn to mark the model busy, like any other dispatched turn")
	}
	if !strings.Contains(m2.transcript.String(), backgroundFollowupPrompt) {
		t.Errorf("expected the transcript to show the follow-up prompt was submitted, got %q", m2.transcript.String())
	}
}

// TestMaybeAutoFollowup_Busy_DefersUntilIdle verifies a wave finishing while
// the model is busy does not interrupt the in-flight turn, and instead
// retries via handleAgentDone once that turn completes.
func TestMaybeAutoFollowup_Busy_DefersUntilIdle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 1)
	seedFinishedJob(t, mgr)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)
	m.busy = true
	before := m.transcript.String()

	updated, cmd := m.Update(backgroundBatchDoneMsg{})
	m2 := updated.(model)

	if cmd != nil {
		t.Error("expected no dispatch while busy")
	}
	if !m2.pendingBackgroundFollowup {
		t.Error("expected pendingBackgroundFollowup to be set for retry once idle")
	}
	if m2.transcript.String() != before {
		t.Errorf("expected no transcript change while deferring, got %q", m2.transcript.String())
	}

	// handleAgentDone (turn completion) must retry and succeed now that
	// busy is about to be cleared.
	updated2, cmd2 := m2.handleAgentDone(agentDoneMsg{})
	m3 := updated2.(model)
	if cmd2 == nil {
		t.Fatal("expected handleAgentDone to dispatch the deferred follow-up turn")
	}
	if m3.pendingBackgroundFollowup {
		t.Error("expected pendingBackgroundFollowup to be cleared after the retry")
	}
}

// TestMaybeAutoFollowup_ActiveJobsRemain_DoesNotDispatch verifies that if a
// newer wave started before the follow-up fired, it waits for that wave's
// own completion signal instead of firing early.
func TestMaybeAutoFollowup_ActiveJobsRemain_DoesNotDispatch(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 1)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)
	m.pendingBackgroundFollowup = true

	release := make(chan struct{})
	mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context, _ string, _ io.Writer) (string, session.TokenUsage, error) {
		<-release
		return "ok", session.TokenUsage{}, nil
	})

	updated, cmd := m.Update(backgroundBatchDoneMsg{})
	m2 := updated.(model)
	close(release)

	if cmd != nil {
		t.Error("expected no dispatch while a newer wave is still running")
	}
	if m2.pendingBackgroundFollowup {
		t.Error("expected the stale pending flag to be cleared, not retried, for a wave that hasn't finished yet")
	}
}

// TestHandleBusyKey_CtrlJSpawnsBackgroundAgent verifies the busy-key flow:
// Enter while busy just shows a hint (does not touch the textarea), and
// Ctrl+J (the universal fallback for Ctrl+Enter) spawns a background job
// from whatever is currently in the textarea.
func TestHandleBusyKey_CtrlJSpawnsBackgroundAgent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	escAgent := local.New("http://127.0.0.1:1", "esc-model")
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr, escalationLocal: escAgent}, nil)
	m.busy = true
	m.ta.SetValue("dig into the physics module")

	// Enter: shows the hint, leaves the textarea untouched.
	updated, _ := m.handleBusyKey(teaKeyEnter())
	m2 := updated.(model)
	if m2.ta.Value() != "dig into the physics module" {
		t.Errorf("expected the textarea to survive Enter, got %q", m2.ta.Value())
	}
	if !strings.Contains(m2.busyHint, "Ctrl+Enter") {
		t.Errorf("expected the hint to mention Ctrl+Enter, got %q", m2.busyHint)
	}

	// Ctrl+J: spawns — the actual Spawn call is deferred into a
	// tea.Cmd to avoid deadlocking bubbletea's unbuffered msgs channel
	// (p.Send inside onStart would block if called from within Update).
	updated2, cmd := m2.handleBusyKey(tea.KeyMsg{Type: tea.KeyCtrlJ})
	m3 := updated2.(model)
	if m3.ta.Value() != "" {
		t.Errorf("expected the textarea to be cleared after spawning, got %q", m3.ta.Value())
	}
	// Execute the deferred spawn Cmd, then feed its message back into Update.
	if cmd == nil {
		t.Fatal("expected a non-nil Cmd to spawn the background agent asynchronously")
	}
	msg := cmd()
	updated3, _ := m3.Update(msg)
	m4 := updated3.(model)
	// Spawn confirmation is a turn-unrelated status event (issue #162): it
	// surfaces as a toast, never as a transcript line.
	if !toastMentions(m4, "spawned background agent") {
		t.Errorf("expected a spawn toast, got %#v", m4.toastVisible)
	}
	if strings.Contains(m4.transcript.String(), "spawned background agent") {
		t.Errorf("spawn confirmation must not be appended to the transcript (#162), got %q", m4.transcript.String())
	}
	if got := mgr.ActiveCount(); got != 1 {
		t.Errorf("expected 1 active job after spawning, got %d", got)
	}
}

// TestHandleBusyKey_EnterDoesNotSpawn verifies Enter while busy only shows
// the hint — it never spawns a background agent (the old double-Enter
// trigger was too easy to fire accidentally).
func TestHandleBusyKey_EnterDoesNotSpawn(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	escAgent := local.New("http://127.0.0.1:1", "esc-model")
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr, escalationLocal: escAgent}, nil)
	m.busy = true
	m.ta.SetValue("dig into the physics module")

	// First Enter: shows hint.
	updated, _ := m.handleBusyKey(teaKeyEnter())
	m2 := updated.(model)
	if !strings.Contains(m2.busyHint, "Ctrl+Enter") {
		t.Errorf("expected the hint to mention Ctrl+Enter, got %q", m2.busyHint)
	}
	if m2.ta.Value() != "dig into the physics module" {
		t.Errorf("expected the textarea to survive Enter, got %q", m2.ta.Value())
	}

	// Second Enter: just shows the hint again — must NOT spawn.
	updated2, _ := m2.handleBusyKey(teaKeyEnter())
	m3 := updated2.(model)
	if m3.ta.Value() != "dig into the physics module" {
		t.Errorf("expected the textarea to survive a second Enter, got %q", m3.ta.Value())
	}
	if got := mgr.ActiveCount(); got != 0 {
		t.Errorf("expected 0 active jobs after double-Enter, got %d", got)
	}
}

// TestHandleBusyKey_EmptyInputShowsInterruptHint verifies pressing Enter with
// nothing typed falls back to the plain "interrupt" hint.
func TestHandleBusyKey_EmptyInputShowsInterruptHint(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)
	m.busy = true

	updated, _ := m.handleBusyKey(teaKeyEnter())
	m2 := updated.(model)
	if !strings.Contains(m2.busyHint, "Ctrl+C") {
		t.Errorf("expected the plain interrupt hint, got %q", m2.busyHint)
	}
}

// TestMaybeAutoFollowup_UserJob_FiresEvenIfOtherJobsStillRunning verifies
// the key difference from the agent-wave case: a user-initiated job's
// result should reach the main agent as soon as it's free, even while
// other jobs (agent- or user-initiated) are still running — there is no
// "wave" to wait for.
func TestMaybeAutoFollowup_UserJob_FiresEvenIfOtherJobsStillRunning(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 2)
	seedFinishedJob(t, mgr)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	release := make(chan struct{})
	mgr.Spawn("still running", "t", "primary", "m", func(ctx context.Context, _ string, _ io.Writer) (string, session.TokenUsage, error) {
		<-release
		return "ok", session.TokenUsage{}, nil
	})
	defer close(release)

	updated, cmd := m.Update(backgroundUserJobDoneMsg{})
	m2 := updated.(model)

	if cmd == nil {
		t.Fatal("expected the user-job follow-up to dispatch even with another job still running")
	}
	if m2.pendingUserBackgroundFollowup {
		t.Error("expected pendingUserBackgroundFollowup to be cleared once dispatched")
	}
}

func teaKeyEnter() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyEnter}
}

// TestF4TogglesBackgroundPanel verifies the global F1-F4 panel shortcuts:
// F3 toggles the background-agents panel the same way /panel background
// does, and works regardless of busy state (toggling how much screen space
// a panel takes doesn't conflict with an in-flight turn). F1-F4 follow the
// same left-to-right order panels are joined in View(): memory, tasks,
// background, workflow.
func TestF3TogglesBackgroundPanel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)
	m.busy = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyF3})
	m2 := updated.(model)
	if !m2.panelBackground {
		t.Fatal("expected F3 to open the background panel")
	}
	if !toastMentions(m2, "background agents panel: on") {
		t.Errorf("expected a panel-toggle toast, got %#v", m2.toastVisible)
	}
	if strings.Contains(m2.transcript.String(), "background agents panel: on") {
		t.Errorf("panel confirmation must not be appended to the transcript (#162), got %q", m2.transcript.String())
	}

	updated2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyF3})
	m3 := updated2.(model)
	if m3.panelBackground {
		t.Error("expected a second F3 to close it again")
	}
}

// TestF4TogglesWorkflowPanel covers the other half of the F1-F4 reordering:
// F4 now maps to the workflow panel (last in View()'s join order), not
// background.
func TestF4TogglesWorkflowPanel(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)
	m.busy = true

	updated, _ := m.Update(tea.KeyMsg{Type: tea.KeyF4})
	m2 := updated.(model)
	if !m2.workflowPanelOpen {
		t.Fatal("expected F4 to open the workflow panel")
	}

	updated2, _ := m2.Update(tea.KeyMsg{Type: tea.KeyF4})
	m3 := updated2.(model)
	if m3.workflowPanelOpen {
		t.Error("expected a second F4 to close it again")
	}
}

// TestPanelCommand_Background verifies /panel background toggles the same
// field F4 does — the shortcut and the slash command are two paths to the
// same state.
func TestPanelCommand_Background(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	updated, _ := m.handlePanelCmd("background")
	m2 := updated.(model)
	if !m2.panelBackground {
		t.Fatal("expected /panel background to open the panel")
	}
}

// TestBgCmd_List verifies /bg list renders the background-agents table.
func TestBgCmd_List(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	updated, _ := m.handleSlashInput("/bg", "list")
	m2 := updated.(model)
	transcript := m2.transcript.String()
	if !strings.Contains(transcript, "background agents:") {
		t.Errorf("expected background agents header, got %q", transcript)
	}
	if !strings.Contains(transcript, "(none)") {
		t.Errorf("expected (none) for empty list, got %q", transcript)
	}
}

// TestBgCmd_ListWithJobs verifies /bg list shows job ID, status, and label.
func TestBgCmd_ListWithJobs(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	release := make(chan struct{})
	defer close(release)
	mgr.Spawn("investigate X", "do something", "user", "test-model", func(ctx context.Context, _ string, _ io.Writer) (string, session.TokenUsage, error) {
		<-release
		return "ok", session.TokenUsage{}, nil
	})

	updated, _ := m.handleSlashInput("/bg", "list")
	m2 := updated.(model)
	transcript := m2.transcript.String()
	if !strings.Contains(transcript, "running") {
		t.Errorf("expected running status, got %q", transcript)
	}
	if !strings.Contains(transcript, "investigate X") {
		t.Errorf("expected job label, got %q", transcript)
	}
}

// TestBgCmd_Stop verifies /bg stop cancels a running job.
func TestBgCmd_Stop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	job := mgr.Spawn("cancel me", "t", "user", "test-model", func(ctx context.Context, _ string, _ io.Writer) (string, session.TokenUsage, error) {
		<-ctx.Done()
		return "", session.TokenUsage{}, ctx.Err()
	})

	updated, _ := m.handleSlashInput("/bg", "stop "+job.ID)
	m2 := updated.(model)
	if !strings.Contains(m2.transcript.String(), "cancelling") {
		t.Errorf("expected cancelling message, got %q", m2.transcript.String())
	}

	// Wait for the job to wind down.
	for i := 0; i < 100; i++ {
		if mgr.ActiveCount() == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestBgCmd_StopUnknownID verifies /bg stop with an unknown ID reports the miss.
func TestBgCmd_StopUnknownID(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	updated, _ := m.handleSlashInput("/bg", "stop job_999")
	m2 := updated.(model)
	if !strings.Contains(m2.transcript.String(), "not found") {
		t.Errorf("expected not-found message, got %q", m2.transcript.String())
	}
}

// TestBgCmd_Usage verifies /bg with an unknown subcommand shows usage.
func TestBgCmd_Usage(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	mgr := local.NewManager(context.Background(), 3)
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	updated, _ := m.handleSlashInput("/bg", "frobnicate")
	m2 := updated.(model)
	if !strings.Contains(m2.transcript.String(), "usage:") {
		t.Errorf("expected usage message, got %q", m2.transcript.String())
	}
}
