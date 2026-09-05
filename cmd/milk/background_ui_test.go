package main

import (
	"context"
	"errors"
	"strings"
	"testing"

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
