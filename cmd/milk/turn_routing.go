package main

import (
	"context"
	"fmt"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/router"
)

// The routing half of a turn, shared by the TUI (runTurn in repl.go) and the
// ACP server (acpSession.runTurn): apply the session's pin/force flags to the
// router, consume the single-turn overrides, resolve the target against which
// agents are actually available, and afterwards apply the auto-sticky rule and
// record the turn's metrics. Channel-specific parts — timeouts shown to the
// user, remote-oversight notifications, output plumbing — stay with the
// callers.

// routedTurn is the outcome of routeTurn.
type routedTurn struct {
	Decision router.Decision
	// Target is where the turn will run, after the availability fallback.
	Target router.Target
	// Fallback is "escalation" or "primary" when the router's choice was
	// unavailable and Target differs from it, else "".
	Fallback string
}

// routeTurn routes input for st's session. localAvail / escalationAvail say
// whether each agent can run; a decision for an unavailable one falls back to
// the other. It consumes the single-turn flags (forceEscalate, forcePrimary);
// pins (stickyEscalate, stickyPrimary, autoStickyEscalate) persist.
func routeTurn(ctx context.Context, st *interactiveState, rtr *router.Router, input string, localAvail, escalationAvail bool) (routedTurn, error) {
	forceEscalate := st.forceEscalate || st.stickyEscalate || st.autoStickyEscalate
	forcePrimary := st.forcePrimary || st.stickyPrimary

	// Route first (fast); the per-agent turn timeout is applied by the caller
	// once the target is known.
	routeCtx, cancel := context.WithTimeoutCause(ctx, agentTimeout, fmt.Errorf("turn timeout"))
	decision, err := rtr.Route(routeCtx, st.sess, input, forceEscalate, forcePrimary)
	cancel()
	if err != nil {
		return routedTurn{}, fmt.Errorf("routing: %w", err)
	}

	st.forceEscalate = false
	// A forcePrimary turn (single-turn /primary <prompt>) breaks auto-sticky so
	// the next turn is re-evaluated by the router rather than staying on escalation.
	if st.forcePrimary {
		st.autoStickyEscalate = false
	}
	st.forcePrimary = false

	rt := routedTurn{Decision: decision, Target: resolveTarget(decision.Target, localAvail, escalationAvail)}
	switch {
	case rt.Target == decision.Target:
	case rt.Target == router.TargetEscalation:
		rt.Fallback = "escalation"
	default:
		rt.Fallback = "primary"
	}
	return rt, nil
}

// clearRoutingPins drops every routing pin — explicit and automatic — so the
// router decides again (the TUI's Ctrl+C on empty input). An ACP client's
// "auto" routing option does the same.
func clearRoutingPins(st *interactiveState) {
	st.forceEscalate = false
	st.forcePrimary = false
	st.stickyEscalate = false
	st.stickyPrimary = false
	st.autoStickyEscalate = false
}

// noteTurnSucceeded applies the auto-sticky rule after a turn that finished
// without error: if the router (not an explicit /escalate pin) sent the turn
// to the escalation agent, keep later turns there so the escalation agent
// doesn't lose continuity between turns. Disabled by sticky_escalation: false.
//
// (The single-turn forceEscalate flag has already been consumed by routeTurn
// when this runs, so it cannot be consulted here; a `/escalate <prompt>` turn
// therefore also becomes auto-sticky, as it always has in the TUI.)
func noteTurnSucceeded(st *interactiveState, target router.Target) {
	if target == router.TargetEscalation && !st.stickyEscalate && st.cfg.StickyEscalationEnabled() {
		st.autoStickyEscalate = true
	}
}

// turnSource is the "source" label for milk.turns.total: user (pinned),
// auto_sticky, or auto (router-decided).
func turnSource(st *interactiveState) string {
	if st.stickyEscalate || st.stickyPrimary {
		return "user"
	}
	if st.autoStickyEscalate {
		return "auto_sticky"
	}
	return "auto"
}

// recordTurn emits the per-turn metrics. source should be captured before the
// turn runs (turnSource), since the turn can change the pins.
func recordTurn(ctx context.Context, target router.Target, source string, start time.Time, turnErr error) {
	label := string(target)
	obs.Inc(ctx, milkScope, "milk.turns.total",
		attribute.String("target", label),
		attribute.String("source", source),
	)
	obs.RecordDuration(ctx, milkScope, "milk.turns.latency_ms", time.Since(start),
		attribute.String("target", label),
	)
	if turnErr != nil {
		obs.Inc(ctx, milkScope, "milk.turns.errors",
			attribute.String("target", label),
			attribute.String("kind", "inference"),
		)
	}
}
