package main

import (
	"context"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/router"
)

// The [milk] route announcement (routeChangeNote): a turn entering
// escalation on its own reports the reason in the transcript — one line per
// change, never per turn; the one-shot /escalate <prompt> is silent (its
// echo announces it); leaving escalation is silent.

func esc(rt routedTurn) routedTurn {
	rt.Target = router.TargetEscalation
	return rt
}

func TestRouteChangeNote_EdgeReasonAndDedup(t *testing.T) {
	st := &interactiveState{}

	// Edge into escalation: announced with agent + reason.
	note := routeChangeNote(st, esc(routedTurn{Decision: router.Decision{Target: router.TargetEscalation, Reason: "model classifier"}}), "big")
	if !strings.Contains(note, "[milk] escalated to big: model classifier") {
		t.Fatalf("edge into escalation must announce the reason, got %q", note)
	}

	// Steady state (same target + reason): silent.
	if note := routeChangeNote(st, esc(routedTurn{Decision: router.Decision{Target: router.TargetEscalation, Reason: "model classifier"}}), "big"); note != "" {
		t.Fatalf("a steady escalation must not be re-announced, got %q", note)
	}

	// Reason change while staying escalated: announced again.
	note = routeChangeNote(st, esc(routedTurn{ForceSource: "auto-sticky escalation"}), "big")
	if !strings.Contains(note, "auto-sticky escalation") {
		t.Fatalf("a reason change must be announced, got %q", note)
	}

	// Leaving escalation: silent, and re-entering is a fresh edge.
	if note := routeChangeNote(st, routedTurn{Target: router.TargetLocal, Decision: router.Decision{Target: router.TargetLocal, Reason: "default"}}, "small"); note != "" {
		t.Fatalf("leaving escalation must be silent, got %q", note)
	}
	if note := routeChangeNote(st, esc(routedTurn{Decision: router.Decision{Target: router.TargetEscalation, Reason: "rules: keywords"}}), "big"); !strings.Contains(note, "rules: keywords") {
		t.Fatalf("re-entering escalation must announce, got %q", note)
	}
}

func TestRouteChangeNote_OneShotIsSilent(t *testing.T) {
	st := &interactiveState{}
	rt := esc(routedTurn{SingleTurnEscalate: true, ForceSource: "one-shot /escalate <prompt>"})
	if note := routeChangeNote(st, rt, "big"); note != "" {
		t.Fatalf("the one-shot /escalate echo is the announcement, got %q", note)
	}
	// The state is recorded either way: the next (auto-sticky) turn sees a
	// reason change and announces itself.
	next := esc(routedTurn{ForceSource: "auto-sticky escalation"})
	if note := routeChangeNote(st, next, "big"); !strings.Contains(note, "auto-sticky escalation") {
		t.Fatalf("the automatic follow-up must be announced, got %q", note)
	}
}

func TestRouteChangeNote_FallbackSaysPrimaryUnavailable(t *testing.T) {
	st := &interactiveState{}
	rt := esc(routedTurn{Target: router.TargetEscalation, Fallback: "escalation", Decision: router.Decision{Target: router.TargetLocal, Reason: "default"}})
	note := routeChangeNote(st, rt, "big")
	if !strings.Contains(note, "primary unavailable") || strings.Contains(note, "default") {
		t.Fatalf("a fallback must report the liveness cause, not the router's choice: %q", note)
	}
}

// TestRouteChangeNote_ForceLabelsComeFromRouteTurn pins the honest labels
// against the real routeTurn: the router collapses every force flag into
// "--escalate flag", routedTurn.ForceSource restores which one it was.
func TestRouteChangeNote_ForceLabelsComeFromRouteTurn(t *testing.T) {
	for _, tc := range []struct {
		arm  func(*interactiveState)
		want string
	}{
		{func(st *interactiveState) { st.autoStickyEscalate = true }, "auto-sticky escalation"},
		{func(st *interactiveState) { st.stickyEscalate = true }, "sticky escalation (/escalate)"},
		{func(st *interactiveState) { st.forceEscalate = true }, "one-shot /escalate <prompt>"},
	} {
		st := routingState(t)
		tc.arm(st)
		rt, err := routeTurn(context.Background(), st, router.New(st.cfg, nil), "x", true, true)
		if err != nil {
			t.Fatal(err)
		}
		if rt.Target != router.TargetEscalation || rt.ForceSource != tc.want {
			t.Errorf("force label: got target=%v source=%q, want escalation %q", rt.Target, rt.ForceSource, tc.want)
		}
		if got := routeReason(rt); got != tc.want {
			t.Errorf("routeReason: got %q, want %q", got, tc.want)
		}
	}
}
