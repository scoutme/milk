package main

// Regression tests for executeWithRetry, which wraps TurnRunner.Execute with
// the same transient network/stream error retry that workflow turns already
// got via workflow.Turn — a single HTTP/2 stream reset should no longer end a
// standalone turn that a retry would have recovered.

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/http2"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/workflow"
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

func (r *flakyExecRunner) RunToolCall(_ context.Context, _ config.Config, _ string, _ []local.ContentPart, _ io.Writer) (string, error) {
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
	// HTTP status errors surface as plain messages and are deliberately
	// never retried (issue #391 — the widened classification covers
	// connection-phase failures; io.EOF-family moved to the transient side
	// with it and is pinned in workflow's classifier table).
	runner := &flakyExecRunner{
		name: "test",
		errs: []error{errors.New("inference server returned 401: unauthorized")},
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

// A turn that already ran tools must not be restarted from its prompt: its
// results and reasoning exist only in the failed run while its edits are on
// disk, so a restart redoes the work blind (seen live as "partial work from
// someone else" after a 2.5h turn was re-run from scratch).
func TestExecuteWithRetry_DoesNotRestartTurnThatRanTools(t *testing.T) {
	runner := &trailFailRunner{trail: editTrail(), err: streamResetErr()}

	res, err := executeWithRetry(context.Background(), runner, config.Config{}, &session.Session{},
		nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
	if err == nil {
		t.Fatal("want the transient error returned, got nil")
	}
	if runner.calls != 1 {
		t.Errorf("want 1 Execute call (no restart after tools ran), got %d", runner.calls)
	}
	if len(res.Trail) == 0 {
		t.Error("the failed turn's trail must be returned so it can be recorded")
	}
}

// trailFailRunner always fails with err and reports the trail it built up.
type trailFailRunner struct {
	flakyExecRunner
	trail []session.TrailStep
	err   error
	calls int
}

func (r *trailFailRunner) Execute(_ context.Context, _ config.Config, _ *session.Session, _ *memory.Store, _ AgentRole,
	_ escalation.ContextMode, _, _ string, _ []string, _ bool, _ string, _ TurnCallbacks, _ io.Writer) (TurnResult, error) {
	r.calls++
	return TurnResult{Trail: r.trail}, r.err
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
		"hi", "hi", &out, nil, nil, nil, nil); err != nil {
		t.Fatalf("runPrimaryWithSession returned error: %v", err)
	}
	if runner.calls != 2 {
		t.Errorf("want 2 Execute calls (1 failure + 1 retry), got %d", runner.calls)
	}
}

// connRefusedErr builds the connection-phase failure class issue #391
// widened the classifier for: a real *net.OpError as the transport wraps
// dial failures (and TLS handshake timeouts classify via the net.Error
// timeout path — see workflow's real-chain probes).
func connRefusedErr() error {
	return &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
}

// TestExecuteWithRetry_RetriesConnectionPhaseFailure pins the core of
// issue #391 end-to-end at the standalone-turn seam: a connection-phase
// error now retries through the same loop that already existed for HTTP/2
// stream resets.
func TestExecuteWithRetry_RetriesConnectionPhaseFailure(t *testing.T) {
	runner := &flakyExecRunner{
		name: "test",
		errs: []error{connRefusedErr()},
		res:  TurnResult{Text: "recovered"},
	}

	res, err := executeWithRetry(context.Background(), runner, config.Config{}, &session.Session{},
		nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
	if err != nil {
		t.Fatalf("connection-phase failure must be retried: %v", err)
	}
	if res.Text != "recovered" {
		t.Errorf("got %q, want %q", res.Text, "recovered")
	}
	if runner.calls != 2 {
		t.Errorf("want 2 Execute calls (1 failure + 1 retry), got %d", runner.calls)
	}
}

// TestExecuteWithRetry_LimitsGovernRetries: limits.turn_retries on the
// acting agent's entry decides the loop — positive = exact extra attempts,
// negative = disabled (run once). Backoff comes from the same policy
// (backoff_max_secs floors at 1s here — the knob is whole seconds).
func TestExecuteWithRetry_LimitsGovernRetries(t *testing.T) {
	t.Run("positive = exact retries", func(t *testing.T) {
		retries := 1
		cfg := config.Config{Agents: []config.AgentConfig{{Name: "test",
			Limits: &config.AgentLimits{TurnRetries: &retries}}}}
		runner := &flakyExecRunner{
			name: "test",
			errs: []error{connRefusedErr(), connRefusedErr(), connRefusedErr()},
		}
		_, err := executeWithRetry(context.Background(), runner, cfg, &session.Session{},
			nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
		if err == nil {
			t.Fatal("want the failure surfaced after the configured retries")
		}
		if runner.calls != 2 {
			t.Errorf("turn_retries=1: %d calls, want 2 (1 + 1 retry)", runner.calls)
		}
	})
	t.Run("negative = disabled", func(t *testing.T) {
		disabled := -1
		cfg := config.Config{Agents: []config.AgentConfig{{Name: "test",
			Limits: &config.AgentLimits{TurnRetries: &disabled}}}}
		runner := &flakyExecRunner{name: "test", errs: []error{connRefusedErr()}}
		_, err := executeWithRetry(context.Background(), runner, cfg, &session.Session{},
			nil, RolePrimary, escalation.ContextModeFirst, "", "", nil, false, "hi", TurnCallbacks{}, io.Discard)
		if err == nil {
			t.Fatal("want the error — nothing may retry a disabled policy")
		}
		if runner.calls != 1 {
			t.Errorf("turn_retries disabled: %d calls, want exactly 1", runner.calls)
		}
	})
}

// TestRetryToolCall_PolicyGoverned: the tool-agent loop takes its policy
// from the agent being called — fast test-local timing, and Max=0 runs once.
func TestRetryToolCall_PolicyGoverned(t *testing.T) {
	fast := workflow.RetryPolicy{Max: 1, Base: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	calls := 0
	fn := func() (string, error) {
		calls++
		if calls == 1 {
			return "", connRefusedErr()
		}
		return "ok", nil
	}
	got, err := retryToolCall(context.Background(), fast, fn)
	if err != nil || got != "ok" || calls != 2 {
		t.Errorf("got %q err=%v after %d calls, want \"ok\"/nil/2", got, err, calls)
	}

	calls = 0
	fn0 := func() (string, error) { calls++; return "", connRefusedErr() }
	if _, err := retryToolCall(context.Background(), workflow.RetryPolicy{Max: 0}, fn0); err == nil {
		t.Fatal("want the error surfaced when the policy disables retries")
	}
	if calls != 1 {
		t.Errorf("Max=0: %d calls, want exactly 1", calls)
	}
}
