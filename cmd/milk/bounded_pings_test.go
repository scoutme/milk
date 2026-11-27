package main

import (
	"errors"
	"testing"
	"time"
)

// shrinkPingWindows makes the availability bounds tiny for a test and
// restores them afterwards (they are vars exactly so tests don't have to
// sleep real seconds).
func shrinkPingWindows(t *testing.T, fast, grace time.Duration) {
	t.Helper()
	oldFast, oldGrace := availPingTimeout, availPingGrace
	availPingTimeout, availPingGrace = fast, grace
	t.Cleanup(func() { availPingTimeout, availPingGrace = oldFast, oldGrace })
}

// TestBoundedPings_ResultsInOrder: each probe's verdict maps to its own
// slot — nil-error → true, error → false.
func TestBoundedPings_ResultsInOrder(t *testing.T) {
	shrinkPingWindows(t, 200*time.Millisecond, 300*time.Millisecond)
	res := boundedPings(
		func() error { return nil },
		func() error { return errors.New("down") },
		func() error { return nil },
	)
	if len(res) != 3 || !res[0] || res[1] || !res[2] {
		t.Errorf("boundedPings = %v, want [true false true]", res)
	}
}

// TestBoundedPings_SlowSuccessIsAvailable pins the slow-link fix: a probe
// that blows the fast bound but answers within the grace window is UP. The
// old single-bound behavior reported such an agent unavailable — the shape
// of "neither primary nor escalation agent is available" on a slow-DNS link
// with a perfectly healthy endpoint.
func TestBoundedPings_SlowSuccessIsAvailable(t *testing.T) {
	shrinkPingWindows(t, 100*time.Millisecond, 500*time.Millisecond)
	start := time.Now()
	res := boundedPings(func() error {
		time.Sleep(250 * time.Millisecond) // past the fast bound, within grace
		return nil
	})
	elapsed := time.Since(start)
	if !res[0] {
		t.Error("a probe that answers within the grace window must report true (slow link ≠ down)")
	}
	if elapsed < 200*time.Millisecond {
		t.Errorf("boundedPings returned in %v — it must wait past the fast bound for the grace window", elapsed)
	}
}

// TestBoundedPings_CapsASlowProbe: a probe that would take longer than
// availPingTimeout+availPingGrace (the shape of a blackholed dial/HTTP wait)
// still reports false at the bound — the startup wait never inherits the
// dependency's own dial/HTTP timeout (issue #388).
func TestBoundedPings_CapsASlowProbe(t *testing.T) {
	fast, grace := 100*time.Millisecond, 200*time.Millisecond
	shrinkPingWindows(t, fast, grace)
	start := time.Now()
	res := boundedPings(func() error {
		time.Sleep(fast + grace + 2*time.Second)
		return nil
	})
	elapsed := time.Since(start)
	if res[0] {
		t.Error("a probe still running past the grace window must report false")
	}
	if elapsed > fast+grace+time.Second {
		t.Errorf("boundedPings took %v, must be capped near availPingTimeout+availPingGrace (%v)", elapsed, fast+grace)
	}
}

// TestBoundedPings_ImmediateErrorIsFast: a probe that fails outright keeps
// the fast-fail path — no grace is spent on a definitive answer.
func TestBoundedPings_ImmediateErrorIsFast(t *testing.T) {
	fast, grace := 100*time.Millisecond, 2*time.Second
	shrinkPingWindows(t, fast, grace)
	start := time.Now()
	res := boundedPings(func() error { return errors.New("connection refused") })
	elapsed := time.Since(start)
	if res[0] {
		t.Error("an errored probe must report false")
	}
	if elapsed > fast {
		t.Errorf("an immediate error took %v — it must not wait out the grace window", elapsed)
	}
}

// TestBoundedPings_RunsConcurrently: two probes slower than half the fast
// bound finish inside one bound together — sequential execution would take
// twice as long and break the ceiling startup promises (issue #388: a
// dependency costs ONE bounded wait, not one per probe).
func TestBoundedPings_RunsConcurrently(t *testing.T) {
	shrinkPingWindows(t, 300*time.Millisecond, 500*time.Millisecond)
	half := 180 * time.Millisecond // 60% of the fast bound each
	start := time.Now()
	res := boundedPings(
		func() error { time.Sleep(half); return nil },
		func() error { time.Sleep(half); return nil },
	)
	elapsed := time.Since(start)
	if !res[0] || !res[1] {
		t.Errorf("both probes should complete within the bound: %v", res)
	}
	if elapsed >= availPingTimeout {
		t.Errorf("two probes took %v — they must run concurrently (one bound total, not two)", elapsed)
	}
}
