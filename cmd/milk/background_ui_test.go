package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
)

// TestUpdate_BackgroundJobDoneMsg_AppendsTranscript verifies a completed job
// is surfaced in the transcript immediately (independent of the
// turn-boundary drainBackgroundJobs path), for both success and failure.
func TestUpdate_BackgroundJobDoneMsg_AppendsTranscript(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)

	updated, _ := m.Update(backgroundJobDoneMsg{job: &local.Job{Label: "investigate X", Result: "found it"}})
	m2 := updated.(model)
	if !strings.Contains(m2.transcript.String(), `background agent "investigate X" completed`) {
		t.Errorf("expected transcript to mention completion, got %q", m2.transcript.String())
	}

	updated2, _ := m2.Update(backgroundJobDoneMsg{job: &local.Job{Label: "investigate Y", Err: errors.New("boom")}})
	m3 := updated2.(model)
	if !strings.Contains(m3.transcript.String(), `background agent "investigate Y" failed: boom`) {
		t.Errorf("expected transcript to mention failure, got %q", m3.transcript.String())
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
	mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
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
	mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
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

// TestHandleBusyKey_SecondEnterSpawnsBackgroundAgent verifies the new
// busy-key flow: the first Enter while busy just arms a hint (does not
// touch the textarea), and a second Enter while still armed spawns a
// background job from whatever is currently in the textarea instead of
// just re-showing the hint.
func TestHandleBusyKey_SecondEnterSpawnsBackgroundAgent(t *testing.T) {
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

	// First Enter: arms the hint, leaves the textarea untouched.
	updated, _ := m.handleBusyKey(teaKeyEnter())
	m2 := updated.(model)
	if !m2.busySpawnArmed {
		t.Fatal("expected the first Enter to arm the spawn hint")
	}
	if m2.ta.Value() != "dig into the physics module" {
		t.Errorf("expected the textarea to survive the first Enter, got %q", m2.ta.Value())
	}

	// Second Enter: spawns.
	updated2, _ := m2.handleBusyKey(teaKeyEnter())
	m3 := updated2.(model)
	if m3.busySpawnArmed {
		t.Error("expected busySpawnArmed to be cleared after spawning")
	}
	if m3.ta.Value() != "" {
		t.Errorf("expected the textarea to be cleared after spawning, got %q", m3.ta.Value())
	}
	if !strings.Contains(m3.transcript.String(), "spawned background agent") {
		t.Errorf("expected a transcript line confirming the spawn, got %q", m3.transcript.String())
	}
	if got := mgr.ActiveCount(); got != 1 {
		t.Errorf("expected 1 active job after spawning, got %d", got)
	}
}

// TestHandleBusyKey_EmptyInputDoesNotArm verifies pressing Enter with
// nothing typed falls back to the plain "interrupt" hint rather than
// arming a spawn that has nothing to spawn from.
func TestHandleBusyKey_EmptyInputDoesNotArm(t *testing.T) {
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
	if m2.busySpawnArmed {
		t.Error("expected empty input not to arm the spawn hint")
	}
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
	m := newModel(context.Background(), st, nil, dispatchAgents{backgroundMgr: mgr}, nil)

	release := make(chan struct{})
	mgr.Spawn("still running", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
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
	if !strings.Contains(m2.transcript.String(), "background agents panel: on") {
		t.Errorf("expected a confirmation line, got %q", m2.transcript.String())
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
