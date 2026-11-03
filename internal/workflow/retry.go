package workflow

import (
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"time"
)

// Turn-retry policy (issue #391): how often a failed turn is re-attempted
// and how long it waits between attempts. Every retry loop in milk —
// workflow turns, standalone turns (executeWithRetry), tool-agent calls,
// background jobs and the streaming request layer — takes one of these,
// resolved from the agent's limits (config.TurnRetries /
// config.TurnRetryBackoffMax) so a knob change applies to all of them
// alike. The zero value is never meaningful on its own: callers always
// build a policy through DefaultRetryPolicy or from config, and Backoff
// fills a missing base/backoff cap with the built-in defaults.
type RetryPolicy struct {
	// Max is the number of *additional* attempts after the first
	// (0 = run once, never retry). Mirrors the loop condition
	// `attempt >= policy.Max`, so an explicit 0 is honored as "disabled"
	// — set by limits.turn_retries: negative.
	Max int
	// Base is the nominal delay before the first retry; it doubles per
	// attempt (1s, 2s, 4s, …). Zero means the built-in 1s.
	Base time.Duration
	// MaxBackoff caps the nominal delay (the built-in default is 30s —
	// DefaultRetryBackoffMax). Zero means the built-in cap. Jitter never
	// pushes a delay above it: the cap is hard.
	MaxBackoff time.Duration
}

// MaxTurnRetries is the number of additional attempts made when a turn fails
// with a transient network/stream error, when no explicit policy is given.
// Workflow runs are long and mostly unattended (multiple sprints, each a
// multi-minute generator/evaluator exchange); without this, a single upstream
// hiccup — e.g. the peer resetting an HTTP/2 stream — aborts the entire run
// and requires a manual /workflow resume. Exported so cmd/milk can apply the
// same retry behavior to standalone (non-workflow) turns.
const MaxTurnRetries = 2

// DefaultRetryBackoffMax is the built-in cap for the exponential retry
// backoff (issue #391: "1s, 2s, 4s… capped ~30s"). The config mirror is
// limits.turn_retry_backoff_max_secs — keep the two equal (pinned by
// TestRetryPolicy_DefaultsMatchConfig in this package).
const DefaultRetryBackoffMax = 30 * time.Second

// defaultRetryBase is the nominal first-retry delay; not configurable —
// only the cap is (per the issue: knobs are max retries and backoff cap).
const defaultRetryBase = time.Second

// DefaultRetryPolicy returns the built-in policy: MaxTurnRetries extra
// attempts, 1s nominal backoff doubling per attempt, capped at 30s.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{Max: MaxTurnRetries, Base: defaultRetryBase, MaxBackoff: DefaultRetryBackoffMax}
}

// withDefaults fills the timing fields when unset. Max is deliberately
// left as-is: an explicit 0 means "no retries" and must never be
// resurrected into the default of 2.
func (p RetryPolicy) withDefaults() RetryPolicy {
	if p.Base <= 0 {
		p.Base = defaultRetryBase
	}
	if p.MaxBackoff <= 0 {
		p.MaxBackoff = DefaultRetryBackoffMax
	}
	return p
}

// Backoff returns the delay before retry attempt N (0-indexed): the
// nominal exponential series Base·2^N capped at MaxBackoff, plus up to
// 25% positive jitter so simultaneous failures don't retry in lockstep
// (e.g. after a shared upstream blip). The nominal value is the floor —
// attempt 0 always waits at least Base — and the cap is hard: a delay
// never exceeds MaxBackoff, jitter included.
func (p RetryPolicy) Backoff(attempt int) time.Duration {
	p = p.withDefaults()
	nominal := p.Base
	for i := 0; i < attempt && nominal < p.MaxBackoff; i++ {
		nominal *= 2
	}
	if nominal >= p.MaxBackoff {
		return p.MaxBackoff // at the cap: hard ceiling, no jitter above it
	}
	// Up to +25% jitter, bounded by the cap.
	jitter := time.Duration(rand.Int64N(int64(nominal)/4 + 1))
	if d := nominal + jitter; d <= p.MaxBackoff {
		return d
	}
	return p.MaxBackoff
}

// TurnRetryBackoff returns the delay before retry attempt N (0-indexed)
// under the default policy. Kept for callers/tests that don't carry a
// policy; new code passes a RetryPolicy (issue #391).
func TurnRetryBackoff(attempt int) time.Duration {
	return DefaultRetryPolicy().Backoff(attempt)
}

// isTransientTransportError classifies connection-establishment and
// transport-level failures as transient (issue #391). Ground truth for
// the chains (probed against net/http, not assumed): a TLS handshake
// timeout surfaces as url.Error → http.tlsHandshakeTimeoutError with
// net.Error.Timeout() == true but *not* context.DeadlineExceeded;
// connection refused surfaces as url.Error → *net.OpError →
// *os.SyscallError → syscall.Errno. Matching on net.OpError rather than
// individual syscall errnos keeps this portable (no per-GOOS errno
// list): every dial/read syscall failure the transport surfaces as an
// OpError is a condition a retry can outlive. Deliberately NOT matched:
// context errors (checked first by IsRetryableTurnError — a turn-level
// deadline would fail identically), TLS/x509 certificate problems
// (configuration, not hiccup), HTTP status errors (surfaced separately
// from client.Do), and DNS NXDOMAIN (IsNotFound — a wrong base URL never
// fixes itself).
func isTransientTransportError(err error) bool {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true // TLS handshake timeout, dial timeout, response-header timeout
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		return true // refused, reset, unreachable, EPIPE — syscall-level transport failures
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && (dnsErr.IsTemporary || dnsErr.IsTimeout) {
		return true // temporary DNS (EAI_AGAIN / resolver timeout); IsNotFound falls through
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true // connection or body cut short mid-request
	}
	return false
}
