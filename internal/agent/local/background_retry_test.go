package local

// Regression tests for retryBackgroundTask, which wraps a spawned background
// job's execution with the same transient network/stream error retry that
// ordinary turns and tool-agent calls already get via
// workflow.IsRetryableTurnError — without it, a background job (invisible to
// the user, possibly minutes into a task) was permanently lost to a single
// HTTP/2 stream reset instead of silently retrying through it.

import (
	"context"
	"io"
	"testing"

	"golang.org/x/net/http2"

	"github.com/scoutme/milk/internal/session"
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

	result, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", task.run)
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

	_, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", task.run)
	if err == nil {
		t.Fatal("want error after exhausting retries, got nil")
	}
	if task.calls != 3 {
		t.Errorf("want 3 calls (1 initial + 2 retries), got %d", task.calls)
	}
}

func TestRetryBackgroundTask_DoesNotRetryNonTransientError(t *testing.T) {
	task := &flakyBackgroundTask{
		errs: []error{io.ErrUnexpectedEOF},
	}

	_, _, err := retryBackgroundTask(context.Background(), "job_test", "test-model", task.run)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if task.calls != 1 {
		t.Errorf("non-transient error must not be retried, got %d calls", task.calls)
	}
}
