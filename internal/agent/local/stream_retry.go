package local

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/workflow"
)

// defaultStreamNoDataTimeout bounds how long one request may go without a
// single SSE data event. Servers hold a very large request open with
// ": PROCESSING" keep-alive comments; those prove the connection is alive but
// not that the request is progressing, and a 490K-token request was observed
// sitting in that state for minutes before the upstream reset it.
const defaultStreamNoDataTimeout = 5 * time.Minute

// errStreamStalled marks a request cancelled by the no-data watchdog.
var errStreamStalled = errors.New("inference server sent no data")

func (a *Agent) noDataTimeout() time.Duration {
	if a.streamNoDataTimeout > 0 {
		return a.streamNoDataTimeout
	}
	return defaultStreamNoDataTimeout
}

// retryPolicy resolves this agent's turn-retry policy from its limits
// block (issue #391: limits.turn_retries / limits.turn_retry_backoff_max_secs
// — nil limits or unset fields fall back to the built-in defaults). Used by
// the streaming layer and background-job loops; standalone, tool-agent and
// workflow turns resolve the very same fields through cmd/milk's
// retryPolicyFor, so one knob governs every loop alike.
func (a *Agent) retryPolicy() workflow.RetryPolicy {
	return workflow.RetryPolicy{
		Max:        config.TurnRetries(a.limits),
		MaxBackoff: config.TurnRetryBackoffMax(a.limits),
	}
}

// isRetryableStreamError reports whether re-sending the same request is safe
// and likely to help: a transient HTTP/2 failure, a stalled stream, or —
// since issue #391 widened workflow.IsRetryableTurnError — a
// connection-phase failure (TLS handshake timeout, refused dial, …).
func isRetryableStreamError(err error) bool {
	return errors.Is(err, errStreamStalled) || workflow.IsRetryableTurnError(err)
}

// streamCompletionRetrying re-sends only the failed request. A turn that has
// already run tools must not be restarted from its prompt: the tool results
// and file edits it produced exist only in msgs, so a restart loses the
// context while the side effects remain.
func (a *Agent) streamCompletionRetrying(ctx context.Context, msgs []Message, tools []map[string]any, out io.Writer, userMsgIdx int) (string, string, []toolCall, bool, string, error) {
	policy := a.retryPolicy() // issue #391: limits.turn_retries governs this loop too
	for attempt := 0; ; attempt++ {
		text, raw, tcs, empty, reasoning, err := a.streamCompletion(ctx, msgs, tools, out, userMsgIdx)
		if err == nil || attempt >= policy.Max || ctx.Err() != nil || !isRetryableStreamError(err) {
			return text, raw, tcs, empty, reasoning, err
		}
		a.logWarn("transient inference error, re-sending request",
			"model", a.model, "agent", a.logRole(),
			"attempt", attempt+1, "max", policy.Max, "err", err.Error())
		fmt.Fprintf(out, "\n[transient error, retrying request %d/%d: %v]\n", attempt+1, policy.Max, err)
		select {
		case <-time.After(policy.Backoff(attempt)):
		case <-ctx.Done():
			return "", "", nil, false, "", ctx.Err()
		}
	}
}

// stallWatchdog cancels a request if stop is not called within d.
type stallWatchdog struct {
	timer *time.Timer
	fired atomic.Bool
}

func newStallWatchdog(d time.Duration, cancel context.CancelFunc) *stallWatchdog {
	w := &stallWatchdog{}
	w.timer = time.AfterFunc(d, func() {
		w.fired.Store(true)
		cancel()
	})
	return w
}

func (w *stallWatchdog) stop() { w.timer.Stop() }

// err converts a failure caused by the watchdog's own cancellation into
// errStreamStalled; any other error, including the caller's cancellation, is
// returned unchanged.
func (w *stallWatchdog) err(parent context.Context, err error, d time.Duration) error {
	if w.fired.Load() && parent.Err() == nil {
		return fmt.Errorf("%w for %s, request cancelled (%v)", errStreamStalled, d, err)
	}
	return err
}

// dataSignalBody calls onData once, the first time a read contains an SSE
// data field (keep-alive comment lines don't count).
type dataSignalBody struct {
	io.ReadCloser
	once   sync.Once
	onData func()
}

func (b *dataSignalBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 && bytes.Contains(p[:n], []byte("data:")) {
		b.once.Do(b.onData)
	}
	return n, err
}
