package local

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/session"
)

// TestLiveBackgroundObservability is the live end-to-end check for the
// background-agent triage/observability suite (ADR-0043): real inference
// traffic, real Manager lifecycle, real job state file, real OTel signals.
//
// Skipped unless MILK_LIVE_TEST=1 is set, since it spends real tokens against
// the configured inference endpoint:
//
//	MILK_LIVE_TEST=1 go test ./internal/agent/local -run TestLiveBackgroundObservability -v
//
// It verifies, one by one, every triage property the observability work
// promised:
//
//  1. a real background job completes and records token usage,
//  2. job records persist to disk and LoadJobs round-trips them,
//  3. the per-job timeout is classified explicitly (TimedOut, "timed out
//     after …") instead of looking like an ordinary upstream failure,
//  4. a panicking job body fails its own Job record instead of crashing the
//     process,
//  5. the heartbeat advances LastAliveAt while a job runs (the killed-milk
//     "was it still alive?" signal),
//  6. background.spawned / background.completed / background.failed land in
//     logs.jsonl with job IDs.
func TestLiveBackgroundObservability(t *testing.T) {
	if os.Getenv("MILK_LIVE_TEST") == "" {
		t.Skip("set MILK_LIVE_TEST=1 to run live background observability checks (spends tokens)")
	}

	cfg, err := config.LoadMerged()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	ac := cfg.ActiveAgent()
	if ac.URL == "" {
		ac = cfg.EscalationAgentConfig()
	}
	if ac.URL == "" {
		t.Fatal("no inference-backed agent configured (active and escalation both lack URL)")
	}
	ag := NewFromConfig(ac)
	model := ac.Model
	if model == "" {
		model = ac.Name
	}

	// Real signal files in a temp dir so assertions can read them back.
	otelDir := t.TempDir()
	shutdown, err := obs.Init(config.OtelConfig{
		Enabled:   true,
		LogLevel:  "DEBUG",
		LogFormat: "json",
	}, otelDir)
	if err != nil {
		t.Fatalf("obs init: %v", err)
	}
	// Shutdown explicitly at the end (flushes the log batch processor so
	// logs.jsonl is readable); no deferred double-shutdown — the provider
	// would report a spurious error on the second call.

	tmp := t.TempDir()
	statePath := filepath.Join(tmp, "jobs.json")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// --- 1+2+5: a real job through the production retry wrapper -----------
	mgr := NewManager(ctx, 2)
	mgr.SetStateFile(statePath)
	mgr.SetHeartbeatInterval(150 * time.Millisecond)

	done := make(chan *Job, 1)
	mgr.SetOnDone(func(j *Job) { done <- j })

	job := mgr.Spawn("live-e2e", "reply with the word OK", "primary", model,
		func(jobCtx context.Context, jobID string) (string, session.TokenUsage, error) {
			return ag.runBackgroundTaskWithRetry(jobCtx, jobID, tmp,
				"Reply with exactly the word: OK. Do not use any tools.")
		})

	// While it runs, the state file must already show it — a hard-killed
	// milk's triage starts here.
	waitFor(t, 2*time.Second, "running job record appears on disk", func() bool {
		recs, err := LoadJobs(statePath)
		return err == nil && len(recs) == 1 && recs[0].Status == JobRunning
	})

	j := waitDone(t, done, 2*time.Minute)
	if j.ID != job.ID {
		t.Fatalf("done for %s, want %s", j.ID, job.ID)
	}
	if j.Status != JobCompleted {
		t.Fatalf("job status %s, err=%v", j.Status, j.Err)
	}
	if strings.TrimSpace(j.Result) == "" {
		t.Error("empty job result from real inference")
	}
	if j.Tokens.Prompt == 0 && j.Tokens.Completion == 0 {
		t.Error("zero token usage recorded for real inference job")
	}
	t.Logf("job %s completed: result=%q tokens=%+v", j.ID, truncateRunes(j.Result, 60), j.Tokens)

	recs, err := LoadJobs(statePath)
	if err != nil {
		t.Fatalf("LoadJobs: %v", err)
	}
	if len(recs) != 1 || recs[0].Status != JobCompleted || recs[0].Result == "" {
		t.Fatalf("persisted record mismatch: %+v", recs)
	}
	if recs[0].LastAliveAt.IsZero() || recs[0].EndedAt.IsZero() {
		t.Error("persisted record missing last_alive_at/ended_at")
	}

	// --- 3: timeout classification on a real, cut-off request -------------
	mgr2 := NewManager(ctx, 1)
	mgr2.SetStateFile(filepath.Join(tmp, "jobs-timeout.json"))
	mgr2.SetJobTimeout(300 * time.Millisecond)
	done2 := make(chan *Job, 1)
	mgr2.SetOnDone(func(j *Job) { done2 <- j })
	mgr2.Spawn("live-timeout", "reply verbosely", "primary", model,
		func(jobCtx context.Context, jobID string) (string, session.TokenUsage, error) {
			return ag.runBackgroundTaskWithRetry(jobCtx, jobID, tmp,
				"Write a very long, detailed essay about the history of computing. Do not use any tools.")
		})
	tj := waitDone(t, done2, 2*time.Minute)
	if tj.Status != JobFailed {
		t.Fatalf("timeout job status %s, want failed", tj.Status)
	}
	if !tj.TimedOut {
		t.Error("TimedOut not set on deadline-killed job")
	}
	if !errors.Is(tj.Err, context.DeadlineExceeded) {
		t.Errorf("err not DeadlineExceeded: %v", tj.Err)
	}
	if !strings.Contains(tj.Err.Error(), "timed out after") {
		t.Errorf("err text %q lacks 'timed out after'", tj.Err)
	}
	t.Logf("job %s timed out as expected: %v", tj.ID, tj.Err)

	// --- 4: panic containment ---------------------------------------------
	mgr3 := NewManager(ctx, 1)
	mgr3.SetStateFile(filepath.Join(tmp, "jobs-panic.json"))
	done3 := make(chan *Job, 1)
	mgr3.SetOnDone(func(j *Job) { done3 <- j })
	mgr3.Spawn("live-panic", "panic body", "primary", model,
		func(context.Context, string) (string, session.TokenUsage, error) {
			panic("live-test boom")
		})
	pj := waitDone(t, done3, 30*time.Second)
	if pj.Status != JobFailed {
		t.Fatalf("panic job status %s, want failed", pj.Status)
	}
	if !strings.Contains(pj.Err.Error(), "panic in background job") ||
		!strings.Contains(pj.Err.Error(), "live-test boom") {
		t.Errorf("panic error %q lacks panic text", pj.Err)
	}
	t.Logf("job %s contained panic: %v", pj.ID, truncateRunes(pj.Err.Error(), 80))

	// --- 5: heartbeat advances LastAliveAt while running -------------------
	mgr4 := NewManager(ctx, 1)
	mgr4.SetStateFile(filepath.Join(tmp, "jobs-hb.json"))
	mgr4.SetHeartbeatInterval(100 * time.Millisecond)
	done4 := make(chan *Job, 1)
	mgr4.SetOnDone(func(j *Job) { done4 <- j })
	mgr4.Spawn("live-heartbeat", "slow body", "primary", model,
		func(jctx context.Context, _ string) (string, session.TokenUsage, error) {
			select {
			case <-time.After(800 * time.Millisecond):
			case <-jctx.Done():
			}
			return "slow done", session.TokenUsage{}, nil
		})
	var firstAlive time.Time
	waitFor(t, 2*time.Second, "heartbeat bumps last_alive_at", func() bool {
		recs, err := LoadJobs(filepath.Join(tmp, "jobs-hb.json"))
		if err != nil || len(recs) != 1 {
			return false
		}
		if firstAlive.IsZero() {
			firstAlive = recs[0].LastAliveAt
			return false
		}
		return recs[0].LastAliveAt.After(firstAlive)
	})
	waitDone(t, done4, 30*time.Second)

	// --- 6: lifecycle events in logs.jsonl ---------------------------------
	if err := shutdown(context.Background()); err != nil {
		t.Fatalf("obs shutdown: %v", err)
	}
	logs, err := os.ReadFile(filepath.Join(otelDir, "logs.jsonl"))
	if err != nil {
		t.Fatalf("read logs.jsonl: %v", err)
	}
	for _, want := range []string{
		"background.spawned", "background.completed", "background.failed",
		job.ID, tj.ID, pj.ID, "timed_out",
	} {
		if !strings.Contains(string(logs), want) {
			t.Errorf("logs.jsonl missing %q", want)
		}
	}
	t.Logf("logs.jsonl carries all lifecycle events (%d bytes)", len(logs))
}

func waitDone(t *testing.T, done chan *Job, timeout time.Duration) *Job {
	t.Helper()
	select {
	case j := <-done:
		return j
	case <-time.After(timeout):
		t.Fatal("timed out waiting for job completion")
		return nil
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
