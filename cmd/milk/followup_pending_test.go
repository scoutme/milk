package main

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
)

// seedFinishedJob leaves one finished, not-yet-drained job in mgr — the state
// in which a background follow-up turn has something to report.
func seedFinishedJob(t *testing.T, mgr *local.Manager) {
	t.Helper()
	done := make(chan struct{})
	mgr.SetOnDone(func(*local.Job) { close(done) })
	mgr.Spawn("seed", "t", "user", "m", func(context.Context, string, io.Writer) (string, session.TokenUsage, error) {
		return "r", session.TokenUsage{}, nil
	})
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("seed job did not finish")
	}
	mgr.SetOnDone(nil)
	if mgr.PendingCount() != 1 {
		t.Fatalf("pending = %d, want 1", mgr.PendingCount())
	}
}

// A user job's completion sends both the per-job and the batch-done message;
// once the first follow-up has drained the results, the deferred second one
// must not start another (empty) turn.
func TestMaybeAutoFollowup_SkipsWhenNothingPending(t *testing.T) {
	m := newTestModelForPanels(t)
	m.agents.backgroundMgr = local.NewManager(context.Background(), 1)
	m.pendingBackgroundFollowup = true

	updated, cmd := m.maybeAutoFollowupBackgroundJobs(true)
	if cmd != nil {
		t.Error("no pending job results: follow-up must not start")
	}
	if updated.(model).pendingBackgroundFollowup {
		t.Error("a skipped follow-up must clear its retry flag")
	}
}
