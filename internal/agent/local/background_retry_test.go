package local

// Regression tests for retryBackgroundTask, which wraps a spawned background
// job's execution with the same transient network/stream error retry that
// ordinary turns and tool-agent calls already get via
// workflow.IsRetryableTurnError — without it, a background job (invisible to
// the user, possibly minutes into a task) was permanently lost to a single
// HTTP/2 stream reset instead of silently retrying through it.

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
)

func bgStreamResetErr() error {
	return http2.StreamError{StreamID: 39, Code: http2.ErrCodeInternal}
}

// flakyBackgroundTask returns errs[0], errs[1], ... on successive calls,
// then succeeds with result/tokens once errs is exhausted.
type flakyBackgroundTask struct {
	errs   []error
	result string
	tokens session.TokenUsage
	calls  int
}

func (f *flakyBackgroundTask) run() (string, session.TokenUsage, error) {
	if f.calls < len(f.errs) {
		err := f.errs[f.calls]
		f.calls++
		return "", session.TokenUsage{}, err
	}
	f.calls++
	return f.result, f.tokens, nil
}

func TestRetryBackgroundTask_RecoversFromTransientStreamError(t *testing.T) {
	task := &flakyBackgroundTask{
		errs:   []error{bgStreamResetErr()},
		result: "recovered",
	}

	result, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", workflow.DefaultRetryPolicy(), task.run)
	if err != nil {
		t.Fatalf("retryBackgroundTask returned error: %v", err)
	}
	if result != "recovered" {
		t.Errorf("got %q, want %q", result, "recovered")
	}
	if task.calls != 2 {
		t.Errorf("want 2 calls (1 failure + 1 retry), got %d", task.calls)
	}
}

func TestRetryBackgroundTask_GivesUpAfterMaxRetries(t *testing.T) {
	task := &flakyBackgroundTask{
		errs: []error{bgStreamResetErr(), bgStreamResetErr(), bgStreamResetErr()},
	}

	_, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", workflow.DefaultRetryPolicy(), task.run)
	if err == nil {
		t.Fatal("want error after exhausting retries, got nil")
	}
	if task.calls != 3 {
		t.Errorf("want 3 calls (1 initial + 2 retries), got %d", task.calls)
	}
}

func TestRetryBackgroundTask_DoesNotRetryNonTransientError(t *testing.T) {
	// A non-transport failure: HTTP status errors surface as plain messages
	// and are deliberately never retried (issue #391 — the widened
	// classification covers connection-phase failures only; io.EOF-family
	// errors moved to the transient side with it).
	task := &flakyBackgroundTask{
		errs: []error{errors.New("inference server returned 401: unauthorized")},
	}

	_, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", workflow.DefaultRetryPolicy(), task.run)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if task.calls != 1 {
		t.Errorf("non-transient error must not be retried, got %d calls", task.calls)
	}
}

// TestRetryBackgroundTask_RetriesConnectionPhaseFailures pins issue #391's
// core: a connection-establishment failure (dial refused — the class that
// includes TLS handshake timeouts) is retryable, where before only HTTP/2
// stream errors were. A deliberately tiny backoff keeps the loop fast.
func TestRetryBackgroundTask_RetriesConnectionPhaseFailures(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	task := &flakyBackgroundTask{
		errs:   []error{refused},
		result: "recovered",
	}
	policy := workflow.RetryPolicy{Max: 1, Base: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	result, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", policy, task.run)
	if err != nil {
		t.Fatalf("connection-phase failure must be retried: %v", err)
	}
	if result != "recovered" || task.calls != 2 {
		t.Errorf("got result %q after %d calls, want \"recovered\" after 2", result, task.calls)
	}

	// And an explicit Max=0 (limits.turn_retries: negative) disables retries.
	task2 := &flakyBackgroundTask{errs: []error{refused}}
	_, _, err = retryBackgroundTask(context.Background(), "job_test", "test-model", workflow.RetryPolicy{Max: 0}, task2.run)
	if err == nil || task2.calls != 1 {
		t.Errorf("Max=0 must run exactly once, got err=%v calls=%d", err, task2.calls)
	}
}
