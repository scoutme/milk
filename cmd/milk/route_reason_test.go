package main

import (
	"testing"

	"github.com/scoutme/milk/internal/router"
)

// TestResultHopReason pins the honesty rules for the reason recorded on the
// result line's route_history hop: an availability fallback override is
// labeled as liveness ("primary unavailable" / "escalation unavailable"),
// never with the router's now-irrelevant reasoning; otherwise the router's
// own decision reason passes through verbatim; an empty reason degrades to a
// neutral placeholder (mirrors routeReason in turn_routing.go).
func TestResultHopReason(t *testing.T) {
	cases := []struct {
		name     string
		decision router.Decision
		resolved router.Target
		want     string
	}{
		{
			name:     "router reason passes through",
			decision: router.Decision{Target: router.TargetEscalation, Reason: "model classifier"},
			resolved: router.TargetEscalation,
			want:     "model classifier",
		},
		{
			name:     "signals reason passes through verbatim",
			decision: router.Decision{Target: router.TargetEscalation, Reason: "signals: open-question"},
			resolved: router.TargetEscalation,
			want:     "signals: open-question",
		},
		{
			name:     "primary down fallback",
			decision: router.Decision{Target: router.TargetLocal, Reason: "default"},
			resolved: router.TargetEscalation,
			want:     "primary unavailable",
		},
		{
			name:     "escalation down fallback",
			decision: router.Decision{Target: router.TargetEscalation, Reason: "model classifier"},
			resolved: router.TargetLocal,
			want:     "escalation unavailable",
		},
		{
			name:     "empty reason degrades to placeholder",
			decision: router.Decision{Target: router.TargetLocal},
			resolved: router.TargetLocal,
			want:     "routing decision",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := resultHopReason(tc.decision, tc.resolved); got != tc.want {
				t.Errorf("resultHopReason(%+v, %q) = %q, want %q", tc.decision, tc.resolved, got, tc.want)
			}
		})
	}
}
