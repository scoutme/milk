package local

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
)

// TestManager_Spawn_DoesNotBlockCaller verifies Spawn returns immediately
// even when the concurrency semaphore is already full — the job queues
// inside its own goroutine, not on the caller's stack.
func TestManager_Spawn_DoesNotBlockCaller(t *testing.T) {
	mgr := NewManager(context.Background(), 1)
	release := make(chan struct{})
	mgr.Spawn("first", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
		<-release
		return "ok", session.TokenUsage{}, nil
	})

	done := make(chan struct{})
	go func() {
		mgr.Spawn("second", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
			return "ok", session.TokenUsage{}, nil
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Spawn blocked the caller while the semaphore was full")
	}
	close(release)
}

// TestManager_OutlivesCallerContext verifies the fix for a real bug: a job
// must keep running after the context of the turn that spawned it is
// cancelled — the TUI cancels each turn's context the instant that turn's
// runTurn call returns (cmd/milk/repl.go's `defer cancel()`), which happens
// almost immediately after Spawn returns. Only the Manager's own baseCtx,
// supplied at construction, should be able to stop a job.
func TestManager_OutlivesCallerContext(t *testing.T) {
	mgr := NewManager(context.Background(), 1)
	turnCtx, cancelTurn := context.WithCancel(context.Background())

	result := make(chan string, 1)
	mgr.Spawn("job", "t", "primary", "m", func(jobCtx context.Context) (string, session.TokenUsage, error) {
		<-turnCtx.Done() // the caller's turn "ends" shortly after Spawn returns
		select {
		case <-jobCtx.Done():
			result <- "job was cancelled along with the turn"
		case <-time.After(100 * time.Millisecond):
			result <- "job outlived the turn"
		}
		return "ok", session.TokenUsage{}, nil
	})

	cancelTurn() // simulate repl.go's defer cancel() firing right after Spawn
	if got := <-result; got != "job outlived the turn" {
		t.Errorf("expected the job to survive the spawning turn's context being cancelled, got: %s", got)
	}
}

// TestManager_ConcurrencyBounded verifies at most maxConcurrent jobs execute
// simultaneously; the rest queue until a slot frees up.
func TestManager_ConcurrencyBounded(t *testing.T) {
	mgr := NewManager(context.Background(), 2)
	started := make(chan struct{}, 5)
	release := make(chan struct{})

	for i := 0; i < 5; i++ {
		mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
			started <- struct{}{}
			<-release
			return "ok", session.TokenUsage{}, nil
		})
	}

	<-started
	<-started
	select {
	case <-started:
		t.Fatal("a third job started before any of the first two released — concurrency limit not enforced")
	case <-time.After(50 * time.Millisecond):
	}

	close(release)
	for i := 0; i < 3; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("queued jobs never started after a slot freed up")
		}
	}
}

// TestManager_Drain_ReturnsAndClears verifies completed jobs are returned
// once and only once.
func TestManager_Drain_ReturnsAndClears(t *testing.T) {
	mgr := NewManager(context.Background(), 3)
	var wg sync.WaitGroup
	mgr.SetOnDone(func(j *Job) { wg.Done() })

	wg.Add(2)
	mgr.Spawn("a", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
		return "a-result", session.TokenUsage{}, nil
	})
	mgr.Spawn("b", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
		return "b-result", session.TokenUsage{}, nil
	})
	wg.Wait()

	first := mgr.Drain()
	if len(first) != 2 {
		t.Fatalf("expected 2 jobs from first Drain, got %d", len(first))
	}
	second := mgr.Drain()
	if len(second) != 0 {
		t.Fatalf("expected Drain to clear pending jobs, got %d on second call", len(second))
	}
}

// TestManager_FailedRun_SetsJobFailed verifies a run() error is captured on
// the Job rather than silently swallowed.
func TestManager_FailedRun_SetsJobFailed(t *testing.T) {
	mgr := NewManager(context.Background(), 1)
	var wg sync.WaitGroup
	wg.Add(1)
	mgr.SetOnDone(func(j *Job) { wg.Done() })

	wantErr := errors.New("boom")
	mgr.Spawn("failing", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
		return "", session.TokenUsage{}, wantErr
	})
	wg.Wait()

	jobs := mgr.Drain()
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].Status != JobFailed {
		t.Errorf("expected JobFailed, got %v", jobs[0].Status)
	}
	if !errors.Is(jobs[0].Err, wantErr) {
		t.Errorf("expected Err to be %v, got %v", wantErr, jobs[0].Err)
	}
}

// TestManager_OnDone_FiresExactlyOncePerJob verifies the immediate-notify
// hook fires once per job, no more, no less.
func TestManager_OnDone_FiresExactlyOncePerJob(t *testing.T) {
	mgr := NewManager(context.Background(), 3)
	var calls atomic.Int32
	var wg sync.WaitGroup
	wg.Add(4)
	mgr.SetOnDone(func(j *Job) {
		calls.Add(1)
		wg.Done()
	})

	for i := 0; i < 4; i++ {
		mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
			return "ok", session.TokenUsage{}, nil
		})
	}
	wg.Wait()

	if got := calls.Load(); got != 4 {
		t.Errorf("expected onDone to fire exactly 4 times, got %d", got)
	}
}

// TestManager_ActiveCount reflects jobs still queued or executing, and drops
// to zero once everything completes.
func TestManager_ActiveCount(t *testing.T) {
	mgr := NewManager(context.Background(), 1)
	release := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	mgr.SetOnDone(func(j *Job) { wg.Done() })

	mgr.Spawn("job", "t", "primary", "m", func(ctx context.Context) (string, session.TokenUsage, error) {
		<-release
		return "ok", session.TokenUsage{}, nil
	})

	if got := mgr.ActiveCount(); got != 1 {
		t.Errorf("expected ActiveCount=1 while running, got %d", got)
	}
	close(release)
	wg.Wait()
	if got := mgr.ActiveCount(); got != 0 {
		t.Errorf("expected ActiveCount=0 after completion, got %d", got)
	}
}
