package main

import (
	"errors"
	"testing"
	"time"
)

// TestBoundedPings_ResultsInOrder: each probe's verdict maps to its own
// slot — nil-error → true, error → timeout → false.
func TestBoundedPings_ResultsInOrder(t *testing.T) {
	res := boundedPings(
		func() error { return nil },
		func() error { return errors.New("down") },
		func() error { return nil },
	)
	if len(res) != 3 || !res[0] || res[1] || !res[2] {
		t.Errorf("boundedPings = %v, want [true false true]", res)
	}
}

// TestBoundedPings_CapsASlowProbe: a probe that would take longer than
// availPingTimeout (the shape of a blackholed dial/HTTP wait) reports false
// at the bound — the startup wait never inherits the dependency's own
// timeout (issue #388).
func TestBoundedPings_CapsASlowProbe(t *testing.T) {
	start := time.Now()
	res := boundedPings(func() error {
		time.Sleep(availPingTimeout + 2*time.Second)
		return nil
	})
	elapsed := time.Since(start)
	if res[0] {
		t.Error("a probe still running at the bound must report false")
	}
	if elapsed > availPingTimeout+time.Second {
		t.Errorf("boundedPings took %v, must be capped near availPingTimeout (%v)", elapsed, availPingTimeout)
	}
}

// TestBoundedPings_RunsConcurrently: two probes slower than half the bound
// finish inside one bound together — sequential execution would take twice
// as long and break the ceiling startup promises (issue #388: a dependency
// costs ONE bounded wait, not one per probe).
func TestBoundedPings_RunsConcurrently(t *testing.T) {
	half := availPingTimeout * 6 / 10 // 1.8s of a 3s bound each
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
