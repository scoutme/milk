package workflow_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"golang.org/x/net/http2"

	"github.com/scoutme/milk/internal/workflow"
)

type mockRunner struct {
	name     string
	response string
	err      error
}

func (r *mockRunner) Name() string { return r.name }
func (r *mockRunner) Run(_ context.Context, _ string, out io.Writer) (string, error) {
	if r.err != nil {
		return "", r.err
	}
	_, _ = out.Write([]byte(r.response))
	return r.response, nil
}

// flakyRunner fails with errs[0], errs[1], ... on successive calls, then
// succeeds with response once errs is exhausted.
type flakyRunner struct {
	name     string
	response string
	errs     []error
	calls    int
}

func (r *flakyRunner) Name() string { return r.name }
func (r *flakyRunner) Run(_ context.Context, _ string, out io.Writer) (string, error) {
	if r.calls < len(r.errs) {
		err := r.errs[r.calls]
		r.calls++
		return "", err
	}
	r.calls++
	_, _ = out.Write([]byte(r.response))
	return r.response, nil
}

func TestTurn_ReturnsResponse(t *testing.T) {
	runner := &mockRunner{name: "test", response: "hello from agent"}
	got, err := workflow.Turn(context.Background(), runner, "prompt", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "hello from agent" {
		t.Errorf("got %q, want %q", got, "hello from agent")
	}
}

func TestTurn_SendCalled(t *testing.T) {
	runner := &mockRunner{name: "test", response: "output text"}
	var msgs []tea.Msg
	send := func(msg tea.Msg) { msgs = append(msgs, msg) }

	_, err := workflow.Turn(context.Background(), runner, "prompt", send)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(msgs) == 0 {
		t.Error("expected at least one message sent to TUI, got none")
	}
	found := false
	for _, msg := range msgs {
		if chunk, ok := msg.(workflow.WorkflowChunkMsg); ok && chunk.Text == "output text" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected WorkflowChunkMsg{Text: %q} in sent messages %v", "output text", msgs)
	}
}

// streamErr builds a transient HTTP/2 stream-reset error like the one that
// halted a real multi-sprint workflow run: the peer resets the stream with
// INTERNAL_ERROR mid-response, which should not abort an otherwise-healthy
// unattended run.
func streamErr() error {
	return http2.StreamError{StreamID: 3, Code: http2.ErrCodeInternal}
}

func TestTurn_RetriesTransientStreamErrorThenSucceeds(t *testing.T) {
	runner := &flakyRunner{
		name:     "test",
		response: "sprint output",
		errs:     []error{streamErr(), streamErr()}, // fails twice, succeeds on 3rd call
	}
	start := time.Now()
	got, err := workflow.Turn(context.Background(), runner, "prompt", nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "sprint output" {
		t.Errorf("got %q, want %q", got, "sprint output")
	}
	if runner.calls != 3 {
		t.Errorf("runner called %d times, want 3 (2 failures + 1 success)", runner.calls)
	}
	if elapsed := time.Since(start); elapsed < time.Second {
		t.Errorf("expected a backoff delay between retries, elapsed only %v", elapsed)
	}
}

func TestTurn_GivesUpAfterMaxRetriesOnPersistentStreamError(t *testing.T) {
	runner := &flakyRunner{
		name: "test",
		errs: []error{streamErr(), streamErr(), streamErr(), streamErr()}, // always fails
	}
	_, err := workflow.Turn(context.Background(), runner, "prompt", nil)
	if err == nil {
		t.Fatal("expected error after exhausting retries, got nil")
	}
	if !errors.Is(err, streamErr()) && err.Error() != streamErr().Error() {
		t.Errorf("expected the final error to be the stream error, got %v", err)
	}
	if runner.calls != 3 {
		t.Errorf("runner called %d times, want 3 (1 initial + 2 retries, then give up)", runner.calls)
	}
}

func TestTurn_DoesNotRetryNonTransientError(t *testing.T) {
	runner := &mockRunner{name: "test", err: errors.New("inference server error 400: bad request")}
	_, err := workflow.Turn(context.Background(), runner, "prompt", nil)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if runner.err.Error() != err.Error() {
		t.Errorf("got %v, want the original non-retryable error unchanged", err)
	}
}

func TestTurn_DoesNotRetryContextCanceled(t *testing.T) {
	runner := &mockRunner{name: "test", err: context.Canceled}
	start := time.Now()
	_, err := workflow.Turn(context.Background(), runner, "prompt", nil)
	if !errors.Is(err, context.Canceled) {
		t.Errorf("got %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("context.Canceled must return immediately without a retry backoff, took %v", elapsed)
	}
}

func TestTurn_RetryNoticeSentToTUI(t *testing.T) {
	runner := &flakyRunner{name: "test", response: "ok", errs: []error{streamErr()}}
	var msgs []tea.Msg
	send := func(msg tea.Msg) { msgs = append(msgs, msg) }

	_, err := workflow.Turn(context.Background(), runner, "prompt", send)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	found := false
	for _, msg := range msgs {
		if chunk, ok := msg.(workflow.WorkflowChunkMsg); ok && strings.Contains(chunk.Text, "retrying") {
			found = true
		}
	}
	if !found {
		t.Error("expected a retry notice sent to the TUI, found none")
	}
}
