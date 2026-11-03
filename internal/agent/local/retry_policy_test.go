package local

// Issue #391: the local agent's own loops (streaming layer, background
// jobs) resolve their retry policy from the same limits fields every other
// loop uses — limits.turn_retries / limits.turn_retry_backoff_max_secs.

import (
	"testing"
	"time"

	"github.com/scoutme/milk/internal/config"
)

func TestAgentRetryPolicy_FromLimits(t *testing.T) {
	// No limits block → the built-in defaults.
	a := New("http://localhost:9", "m")
	p := a.retryPolicy()
	if p.Max != 2 || p.MaxBackoff != 30*time.Second {
		t.Errorf("default policy = %+v, want Max=2 MaxBackoff=30s", p)
	}

	// Exact values through the limits block.
	retries, backoff := 4, 7
	a.limits = &config.AgentLimits{TurnRetries: &retries, TurnRetryBackoffMaxSecs: &backoff}
	p = a.retryPolicy()
	if p.Max != 4 || p.MaxBackoff != 7*time.Second {
		t.Errorf("limits policy = %+v, want Max=4 MaxBackoff=7s", p)
	}

	// Negative retries = disabled (0 attempts beyond the first).
	disabled := -1
	a.limits = &config.AgentLimits{TurnRetries: &disabled}
	if p := a.retryPolicy(); p.Max != 0 {
		t.Errorf("disabled retries: Max=%d, want 0", p.Max)
	}

	// Zero / non-positive backoff cap = built-in default.
	zero := 0
	a.limits = &config.AgentLimits{TurnRetryBackoffMaxSecs: &zero}
	if p := a.retryPolicy(); p.MaxBackoff != 30*time.Second {
		t.Errorf("backoff cap 0: MaxBackoff=%v, want the 30s default", p.MaxBackoff)
	}

	// The Base is never configurable: always the built-in 1s nominal.
	a.limits = nil
	if p := a.retryPolicy(); p.Base != 0 {
		t.Errorf("policy Base = %v, want 0 (Backoff fills the built-in default)", p.Base)
	}
}
