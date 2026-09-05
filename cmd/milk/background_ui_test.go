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
