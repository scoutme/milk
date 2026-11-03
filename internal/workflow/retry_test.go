package workflow_test

// Issue #391 tests: the widened transient-error classification (connection-
// phase failures, not just HTTP/2 stream errors), the exponential+backoff
// policy, and the defaults mirroring config.TurnRetries /
// config.TurnRetryBackoffMax (the config package cannot import workflow, so
// the mirror is pinned from this side).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/http2"

	tea "charm.land/bubbletea/v2"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/workflow"
)

func TestIsRetryableTurnError_ClassTable(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		// stream-level (the original classes)
		{"http2 stream error", http2.StreamError{StreamID: 3, Code: http2.ErrCodeInternal}, true},
		{"http2 goaway", http2.GoAwayError{LastStreamID: 1, ErrCode: http2.ErrCodeNo}, true},
		{"wrapped stream error", fmt.Errorf("do: %w", http2.StreamError{StreamID: 5, Code: http2.ErrCodeCancel}), true},
		// connection-phase (issue #391 — before any HTTP/2 stream exists)
		{"operror refused", refused, true},
		{"url-style wrapped refused", fmt.Errorf("inference server unreachable: %w", refused), true},
		{"net.Error timeout", &net.DNSError{Err: "i/o timeout", Name: "api.example", IsTimeout: true}, true},
		{"temporary dns", &net.DNSError{Err: "temporary failure in name resolution", Name: "api.example", IsTemporary: true}, true},
		{"eof", io.EOF, true},
		{"unexpected eof", io.ErrUnexpectedEOF, true},
		// deliberate stops — never retryable (checked before transport classes)
		{"context canceled", context.Canceled, false},
		{"wrapped context canceled", fmt.Errorf("req: %w", context.Canceled), false},
		{"context deadline", context.DeadlineExceeded, false},
		{"wrapped context deadline", fmt.Errorf("req: %w", context.DeadlineExceeded), false},
		// permanent failures
		{"plain error", errors.New("inference server returned 401: unauthorized"), false},
		{"dns nxdomain", &net.DNSError{Err: "no such host", Name: "wrong.example", IsNotFound: true}, false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := workflow.IsRetryableTurnError(tc.err); got != tc.want {
				t.Errorf("IsRetryableTurnError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// TestIsRetryableTurnError_RealTLSHandshakeTimeout pins the exact chain that
// motivated issue #391 (a real net/http TLS handshake timeout, not a
// fabricated error): hold a TCP connection open without speaking TLS and let
// the transport's handshake deadline fire. Ground truth from the probe the
// issue was filed on: net.Error.Timeout() == true, and NOT
// context.DeadlineExceeded — so the deliberate-stop check above it must not
// swallow it.
func TestIsRetryableTurnError_RealTLSHandshakeTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			_ = c // hold open, never speak TLS
		}
	}()
	tr := &http.Transport{TLSHandshakeTimeout: 200 * time.Millisecond}
	defer tr.CloseIdleConnections()
	cl := &http.Client{Transport: tr}
	_, err = cl.Post("https://"+ln.Addr().String()+"/", "application/json", nil)
	if err == nil {
		t.Fatal("want a TLS handshake timeout, got no error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		t.Log("note: chain unexpectedly carries context.DeadlineExceeded — classification may rely on the net.Error timeout path")
	}
	if !workflow.IsRetryableTurnError(err) {
		t.Fatalf("a real TLS handshake timeout must be retryable (issue #391), got: %v", err)
	}
}

// TestIsRetryableTurnError_RealConnectionRefused: dial a closed local port —
// the full url.Error → *net.OpError → *os.SyscallError → syscall.Errno chain
// as the transport actually builds it.
func TestIsRetryableTurnError_RealConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listens anymore
	cl := &http.Client{Timeout: 3 * time.Second}
	_, err = cl.Post("https://"+addr+"/", "application/json", nil)
	if err == nil {
		t.Fatal("want connection refused, got no error")
	}
	if !workflow.IsRetryableTurnError(err) {
		t.Fatalf("connection refused must be retryable, got: %v", err)
	}
}

func TestRetryPolicy_BackoffExponentialCappedWithJitter(t *testing.T) {
	p := workflow.RetryPolicy{Base: time.Second, MaxBackoff: 30 * time.Second}
	// Nominal series is the floor: 1s, 2s, 4s — jitter only adds (≤ +25%),
	// so the existing "first retry waits at least a second" pins hold.
	for i, floor := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		got := p.Backoff(i)
		if got < floor || got > floor+floor/4 {
			t.Errorf("attempt %d: backoff %v outside [%v, %v] (nominal + ≤25%% jitter)", i, got, floor, floor+floor/4)
		}
	}
	// The cap is hard: at/above it, exactly the cap — jitter never exceeds it.
	for i := 8; i < 12; i++ {
		if got := p.Backoff(i); got != 30*time.Second {
			t.Errorf("attempt %d: backoff %v, want the hard cap 30s", i, got)
		}
	}
	// Jitter actually exists: some sample strictly above the floor.
	sawJitter := false
	for i := 0; i < 40; i++ {
		if d := p.Backoff(0); d > time.Second {
			sawJitter = true
			break
		}
	}
	if !sawJitter {
		t.Error("40 samples of Backoff(0) were all the floor — jitter is missing")
	}
	// A cap below the nominal first delay collapses to the cap itself.
	small := workflow.RetryPolicy{Base: 100 * time.Millisecond, MaxBackoff: 10 * time.Millisecond}
	if got := small.Backoff(0); got != 10*time.Millisecond {
		t.Errorf("capped backoff = %v, want exactly the 10ms cap", got)
	}
	// The zero policy's timing defaults to the built-in 1s/30s.
	if got := (workflow.RetryPolicy{}).Backoff(0); got < time.Second || got > time.Second+250*time.Millisecond {
		t.Errorf("zero-policy first backoff = %v, want the default ~1s", got)
	}
}

// TestRetryPolicy_DefaultsMatchConfig pins the cross-package mirror the
// config package cannot check itself (config must not import workflow):
// limits.turn_retries / limits.turn_retry_backoff_max_secs built-ins equal
// workflow's defaults.
func TestRetryPolicy_DefaultsMatchConfig(t *testing.T) {
	if got := config.TurnRetries(nil); got != workflow.MaxTurnRetries {
		t.Errorf("config default retries = %d, workflow.MaxTurnRetries = %d", got, workflow.MaxTurnRetries)
	}
	if got := config.TurnRetryBackoffMax(nil); got != workflow.DefaultRetryBackoffMax {
		t.Errorf("config default backoff cap = %v, workflow.DefaultRetryBackoffMax = %v", got, workflow.DefaultRetryBackoffMax)
	}
	if got := workflow.DefaultRetryPolicy(); got.Max != workflow.MaxTurnRetries || got.Base != time.Second || got.MaxBackoff != workflow.DefaultRetryBackoffMax {
		t.Errorf("DefaultRetryPolicy = %+v", got)
	}
}

func TestRunConfig_PolicyFor(t *testing.T) {
	// Zero RunConfig → the built-in default (never silently "no retries").
	if got := (workflow.RunConfig{}).PolicyFor("any"); got != workflow.DefaultRetryPolicy() {
		t.Errorf("zero RunConfig PolicyFor = %+v, want the default policy", got)
	}
	var seen string
	cfg := workflow.RunConfig{RetryFor: func(name string) workflow.RetryPolicy {
		seen = name
		return workflow.RetryPolicy{Max: 7, Base: time.Millisecond, MaxBackoff: time.Second}
	}}
	got := cfg.PolicyFor("agent-a")
	if seen != "agent-a" || got.Max != 7 {
		t.Errorf("PolicyFor = %+v, seen %q — want the resolver called with the agent name", got, seen)
	}
}

// TestTurnWithPolicy_GovernedByPolicy: Max=0 (limits.turn_retries: negative)
// runs once and never retries; a positive policy retries the connection-phase
// failure with its own (tiny, test-local) backoff.
func TestTurnWithPolicy_GovernedByPolicy(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}

	noRetry := &flakyRunner{name: "test", response: "x", errs: []error{refused, refused, refused}}
	if _, err := workflow.TurnWithPolicy(context.Background(), noRetry, "p", nil, workflow.RetryPolicy{Max: 0}); err == nil {
		t.Fatal("want the refused error surfaced when Max=0")
	}
	if noRetry.calls != 1 {
		t.Errorf("Max=0: %d runs, want exactly 1", noRetry.calls)
	}

	fast := workflow.RetryPolicy{Max: 1, Base: time.Millisecond, MaxBackoff: 2 * time.Millisecond}
	recovered := &flakyRunner{name: "test", response: "ok", errs: []error{refused}}
	got, err := workflow.TurnWithPolicy(context.Background(), recovered, "p", nil, fast)
	if err != nil || got != "ok" || recovered.calls != 2 {
		t.Errorf("retryable policy: got %q err=%v after %d runs, want \"ok\"/nil/2", got, err, recovered.calls)
	}
}

// TestTurnWithPolicy_NoticeCarriesPolicyMax: the [transient error, retrying
// N/M] line reports the configured M, not the old constant.
func TestTurnWithPolicy_NoticeCarriesPolicyMax(t *testing.T) {
	var chunks string
	send := func(msg tea.Msg) {
		if c, ok := msg.(workflow.WorkflowChunkMsg); ok {
			chunks += c.Text
		}
	}
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	r := &flakyRunner{name: "test", response: "ok", errs: []error{refused}}
	policy := workflow.RetryPolicy{Max: 4, Base: time.Millisecond, MaxBackoff: time.Millisecond}
	if _, err := workflow.TurnWithPolicy(context.Background(), r, "p", send, policy); err != nil {
		t.Fatal(err)
	}
	if want := "retrying 1/4"; !strings.Contains(chunks, want) {
		t.Errorf("notice %q must carry the policy max (%q)", chunks, want)
	}
}
