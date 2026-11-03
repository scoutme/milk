package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "charm.land/bubbletea/v2"
	"golang.org/x/net/http2"
)

// WorkflowChunkMsg carries a chunk of streamed workflow-stage output to the TUI.
// cmd/milk handles this type in its Update loop (case WorkflowChunkMsg).
type WorkflowChunkMsg struct{ Text string }

// workflowSendWriter forwards each Write as a WorkflowChunkMsg via send.
type workflowSendWriter struct {
	send func(tea.Msg)
}

func (w *workflowSendWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.send(WorkflowChunkMsg{Text: string(p)})
	}
	return len(p), nil
}

// MaxTurnRetries (the default retry count), TurnRetryBackoff (the default
// backoff) and the RetryPolicy type live in retry.go — issue #391.

// Turn invokes one agent turn for a workflow stage, streams output to the TUI
// via send, and returns the agent's plain-text response. A transient
// network/stream error (see IsRetryableTurnError) is retried up to
// MaxTurnRetries times with the default backoff; any other error, or a
// context cancellation/deadline, is returned immediately.
func Turn(ctx context.Context, r TurnRunner, prompt string, send func(tea.Msg)) (string, error) {
	return TurnWithPolicy(ctx, r, prompt, send, DefaultRetryPolicy())
}

// TurnWithPolicy is Turn with an explicit retry policy (issue #391): the
// caller resolves it from the acting agent's limits — RunConfig.RetryFor —
// so limits.turn_retries / limits.turn_retry_backoff_max_secs govern
// workflow turns exactly as they govern every other retry loop.
func TurnWithPolicy(ctx context.Context, r TurnRunner, prompt string, send func(tea.Msg), policy RetryPolicy) (string, error) {
	var out io.Writer
	if send != nil {
		out = &workflowSendWriter{send: send}
	} else {
		out = io.Discard
	}

	for attempt := 0; ; attempt++ {
		text, err := r.Run(ctx, prompt, out)
		if err == nil {
			return text, nil
		}
		if attempt >= policy.Max || !IsRetryableTurnError(err) {
			return "", err
		}
		if send != nil {
			send(WorkflowChunkMsg{Text: fmt.Sprintf(
				"\n[transient error, retrying %d/%d: %v]\n", attempt+1, policy.Max, err)})
		}
		select {
		case <-time.After(policy.Backoff(attempt)):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// IsRetryableTurnError reports whether err looks like a transient
// network/stream-level failure worth retrying — as opposed to a genuine
// request/response error (bad prompt, auth failure, quota, etc.) that
// retrying would not fix. A context cancellation/deadline is never
// retryable: that reflects a deliberate stop, not a hiccup.
//
// Retryable classes (issue #391 widened this from stream-level failures
// only): HTTP/2 stream resets and GOAWAYs — and, new, the
// connection-phase failures that happen *before* any HTTP/2 stream
// exists (TLS handshake timeouts, refused/reset dials, temporary DNS,
// truncated EOFs), which used to be classified non-retryable and killed
// the turn on attempt 1 with MaxTurnRetries sitting unused.
func IsRetryableTurnError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[http2.StreamError](err); ok {
		return true
	}
	if _, ok := errors.AsType[http2.GoAwayError](err); ok {
		return true
	}
	return isTransientTransportError(err)
}
