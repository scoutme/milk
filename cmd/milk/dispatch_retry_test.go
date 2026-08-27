package main

// Regression tests for executeWithRetry, which wraps TurnRunner.Execute with
// the same transient network/stream error retry that workflow turns already
// got via workflow.Turn — a single HTTP/2 stream reset should no longer end a
// standalone turn that a retry would have recovered.

import (
	"bytes"
	"context"
	"io"
	"testing"

	"golang.org/x/net/http2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
)

// flakyExecRunner fails with errs[0], errs[1], ... on successive Execute
// calls, then succeeds with res once errs is exhausted.
type flakyExecRunner struct {
	name  string
	errs  []error
	res   TurnResult
	calls int
}

func (r *flakyExecRunner) Name() string { return r.name }
func (r *flakyExecRunner) IsCLI() bool  { return false }
func (r *flakyExecRunner) Ping() error  { return nil }

func (r *flakyExecRunner) Execute(
	_ context.Context,
	_ config.Config,
	_ *session.Session,
	_ *memory.Store,
	_ AgentRole,
	_ escalation.ContextMode,
	_ string, _ string,
	_ []string,
	_ bool,
	_ string,
	_ TurnCallbacks,
	_ io.Writer,
) (TurnResult, error) {
	if r.calls < len(r.errs) {
		err := r.errs[r.calls]
		r.calls++
		return TurnResult{}, err
	}
	r.calls++
	return r.res, nil
}

func (r *flakyExecRunner) RunToolCall(_ context.Context, _ config.Config, _ string, _ io.Writer) (string, error) {
	return "", nil
}

func streamResetErr() error {
	return http2.StreamError{StreamID: 3, Code: http2.ErrCodeInternal}
}

func TestExecuteWithRetry_RecoversFromTransientStreamError(t *testing.T) {
	runner := &flakyExecRunner{
		name: "test",
		errs: []error{streamResetErr()},
		res:  TurnResult{Text: "recovered"},
	}

	res, err := executeWithRetry(context.Background(), runner, config.Config{}, &session.Session{},
		nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
	if err != nil {
		t.Fatalf("executeWithRetry returned error: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("got %q, want %q", res.Text, "recovered")
	}
	if runner.calls != 2 {
		t.Errorf("want 2 Execute calls (1 failure + 1 retry), got %d", runner.calls)
	}
}

func TestExecuteWithRetry_GivesUpAfterMaxRetries(t *testing.T) {
	runner := &flakyExecRunner{
		name: "test",
		errs: []error{streamResetErr(), streamResetErr(), streamResetErr()},
	}

	_, err := executeWithRetry(context.Background(), runner, config.Config{}, &session.Session{},
		nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
	if err == nil {
		t.Fatal("want error after exhausting retries, got nil")
	}
	if runner.calls != 3 {
		t.Errorf("want 3 Execute calls (1 initial + 2 retries), got %d", runner.calls)
	}
}

func TestExecuteWithRetry_DoesNotRetryNonTransientError(t *testing.T) {
	runner := &flakyExecRunner{
		name: "test",
		errs: []error{io.ErrUnexpectedEOF},
	}

	_, err := executeWithRetry(context.Background(), runner, config.Config{}, &session.Session{},
		nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if runner.calls != 1 {
		t.Errorf("want 1 Execute call (no retry for non-transient error), got %d", runner.calls)
	}
}

// TestRunPrimaryWithSession_SurvivesTransientStreamError is an end-to-end
// check that the retry is actually wired into runPrimaryWithSession, not just
// reachable via executeWithRetry in isolation.
func TestRunPrimaryWithSession_SurvivesTransientStreamError(t *testing.T) {
	withTempHome(t)

	runner := &flakyExecRunner{
		name: "test-primary",
		errs: []error{streamResetErr()},
		res:  TurnResult{Text: "pong"},
	}
	sess := &session.Session{ID: "test-session"}
	cfg := config.Config{Agents: []config.AgentConfig{{Name: "test-primary", Provider: "local"}}}
	var out bytes.Buffer

	if err := runPrimaryWithSession(context.Background(), cfg, sess, runner, nil, nil,
		"hi", "hi", &out, nil, nil, nil); err != nil {
		t.Fatalf("runPrimaryWithSession returned error: %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("want 2 Execute calls (1 failure + 1 retry), got %d", runner.calls)
	}
}
