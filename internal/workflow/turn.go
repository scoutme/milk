package workflow

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// maxTurnRetries is the number of additional attempts made when a turn fails
// with a transient network/stream error. Workflow runs are long and mostly
// unattended (multiple sprints, each a multi-minute generator/evaluator
// exchange); without this, a single upstream hiccup — e.g. the peer
// resetting an HTTP/2 stream — aborts the entire run and requires a manual
// /workflow resume.
const maxTurnRetries = 2

// turnRetryBackoff returns the delay before retry attempt N (0-indexed).
func turnRetryBackoff(attempt int) time.Duration {
	return time.Duration(attempt+1) * time.Second
}

// Turn invokes one agent turn for a workflow stage, streams output to the TUI
// via send, and returns the agent's plain-text response. A transient
// network/stream error (see isRetryableTurnError) is retried up to
// maxTurnRetries times with a short backoff; any other error, or a context
// cancellation/deadline, is returned immediately.
func Turn(ctx context.Context, r TurnRunner, prompt string, send func(tea.Msg)) (string, error) {
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
		if attempt >= maxTurnRetries || !isRetryableTurnError(err) {
			return "", err
		}
		if send != nil {
			send(WorkflowChunkMsg{Text: fmt.Sprintf(
				"\n[transient error, retrying %d/%d: %v]\n", attempt+1, maxTurnRetries, err)})
		}
		select {
		case <-time.After(turnRetryBackoff(attempt)):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
}

// isRetryableTurnError reports whether err looks like a transient
// network/stream-level failure worth retrying — as opposed to a genuine
// request/response error (bad prompt, auth failure, quota, etc.) that
// retrying would not fix. A context cancellation/deadline is never
// retryable: that reflects a deliberate stop, not a hiccup.
func isRetryableTurnError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if _, ok := errors.AsType[http2.StreamError](err); ok {
		return true
	}
	if _, ok := errors.AsType[http2.GoAwayError](err); ok {
		return true
	}
	return false
}
