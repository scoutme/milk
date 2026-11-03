package main

import (
	"context"
	"fmt"
	"sync"
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
	// SingleTurnEscalate records that this turn went to escalation only
	// because of a one-shot /escalate <prompt> override (the flag routeTurn
	// just consumed). noteTurnSucceeded uses it to keep the one-shot form
	// from becoming auto-sticky — see there.
	SingleTurnEscalate bool
	// NeedsClaudeConsent is set when the turn is headed to the implicit
	// Claude CLI default — a target the user never configured, whether via
	// /escalate, the router, sticky escalation or a down primary — and they
	// have not yet approved it this session (gateClaude).
	NeedsClaudeConsent bool
	// NoAgent is set when the turn is headed to the implicit Claude default
	// and Claude Code is not installed — a local, deterministic check, unlike
	// a failed health ping, which a proxy or a slow link can fake.
	NoAgent bool
}

// availPingTimeout bounds the per-turn availability re-check so an
// unreachable host can't stall a turn.
const availPingTimeout = 3 * time.Second

// boundedPings runs every probe concurrently and returns their results in
// order, each capped at availPingTimeout: an unreachable dependency costs
// one bounded wait instead of its own dial/HTTP timeout. Startup probes use
// it (issue #388) so a blackholed agent host can't stall milk's launch — the
// healthy path is unchanged, probes answer in milliseconds. Probes are plain
// func() error so ctx-less CLI/subprocess pings get the same ceiling as the
// HTTP ones; the buffered result channel means an abandoned probe goroutine
// never blocks writing its outcome.
func boundedPings(probes ...func() error) []bool {
	res := make([]bool, len(probes))
	var wg sync.WaitGroup
	for i, probe := range probes {
		wg.Add(1)
		go func(i int, probe func() error) {
			defer wg.Done()
			done := make(chan error, 1)
			go func() { done <- probe() }()
			select {
			case err := <-done:
				res[i] = err == nil
			case <-time.After(availPingTimeout):
			}
		}(i, probe)
	}
	wg.Wait()
	return res
}

// refreshTurnAvailability re-checks any agent that reads unavailable before
// routing a turn. Availability is captured when the runners are built
// (startup, /config init commit, config reload) — but the agent may simply
// not have been up *yet* (server still booting, run_cmd started after the
// check, config reloaded while it was down), and a frozen false would route
// every turn to the other agent until a restart. Returns the flags this turn
// should route with. Where the agent set is shared the recovered flag sticks;
// the TUI's per-turn copy re-checks on later turns — one bounded health ping
// while an agent reads down, nothing once it reads up.
func refreshTurnAvailability(ctx context.Context, rtr *router.Router, agents *dispatchAgents) (localAvail, escalationAvail bool) {
	localAvail = agents.localAvail
	if !localAvail && pingTurnTarget(ctx, agents.primary) {
		localAvail = true
		agents.localAvail = true
		// A router built while the primary was down has no classifier — put
		// the classification step back.
		if agents.local != nil {
			rtr.SetLocalAgent(agents.local)
		}
	}
	escalationAvail = agents.escalationAvail
	if !escalationAvail && pingTurnTarget(ctx, agents.escalation) {
		escalationAvail = true
		agents.escalationAvail = true
	}
	return localAvail, escalationAvail
}

// pingTurnTarget reports whether a dispatch target's agent answers right now.
// The local HTTP agent's ping is bounded so an unreachable host can't stall
// the turn; cli/subprocess pings are local process checks. False when the
// runner has no pingable agent.
func pingTurnTarget(ctx context.Context, r TurnRunner) bool {
	switch t := r.(type) {
	case *localRunner:
		pingCtx, cancel := context.WithTimeout(ctx, availPingTimeout)
		defer cancel()
		return t.agent.Ping(pingCtx) == nil
	case *cliRunner:
		return t.agent.Ping() == nil
	case *subprocessRunner:
		return t.agent.Ping() == nil
	}
	return false
}

// routeTurn routes input for st's session. localAvail / escalationAvail say
// whether each agent can run; a decision for an unavailable one falls back to
// the other. It consumes the single-turn flags (forceEscalate, forcePrimary);
// pins (stickyEscalate, stickyPrimary, autoStickyEscalate) persist.
func routeTurn(ctx context.Context, st *interactiveState, rtr *router.Router, input string, localAvail, escalationAvail bool) (routedTurn, error) {
	forceEscalate := st.forceEscalate || st.stickyEscalate || st.autoStickyEscalate
	forcePrimary := st.forcePrimary || st.stickyPrimary
	// The one-shot /escalate <prompt> form, captured before the flag is
	// consumed below — noteTurnSucceeded needs to know a turn reached
	// escalation this way (issue #188).
	singleTurnEscalate := st.forceEscalate

	// Route first (fast); the per-agent idle watchdog is applied by the caller
	// once the target is known. This timeout bounds only the routing/classifier
	// step — its cause string is deliberately "routing timeout", never "turn
	// timeout": it does not run the agent's turn (issue #305).
	routeCtx, cancel := context.WithTimeoutCause(ctx, agentTimeout, fmt.Errorf("routing timeout"))
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

	rt := routedTurn{Decision: decision, Target: resolveTarget(decision.Target, localAvail, escalationAvail), SingleTurnEscalate: singleTurnEscalate}
	switch {
	case rt.Target == decision.Target:
	case rt.Target == router.TargetEscalation:
		rt.Fallback = "escalation"
	default:
		rt.Fallback = "primary"
	}
	// Both agents reading unavailable is not proof that nothing can run: the
	// health ping is a short GET that a proxy hiccup fails for every agent on
	// the same host. Keep the router's own choice and let the real request
	// (with its retries and its real error) decide, rather than redirecting
	// to an escalation agent that read down too or refusing the turn.
	if !localAvail && !escalationAvail {
		rt.Target, rt.Fallback = decision.Target, ""
	}
	rt.NoAgent = rt.Target == router.TargetEscalation && !escalationAvail && st.cfg.EscalationAgentIsImplicit()
	rt.NeedsClaudeConsent = rt.Target == router.TargetEscalation && escalationAvail &&
		st.cfg.EscalationAgentIsImplicit() && !st.claudeApproved
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
// The one-shot form — /escalate <prompt> (rt.SingleTurnEscalate) — does NOT
// become auto-sticky (issue #188 decision): the user asked for exactly one
// turn on escalation. The sticky meanings of "stay on escalation" are the
// pin (/escalate with no prompt) and the router's own choice; a forcePrimary
// turn breaks an existing auto-sticky (see routeTurn).
func noteTurnSucceeded(st *interactiveState, rt routedTurn) {
	// A turn that only ran on the implicit Claude default because the primary
	// was down (and the user approved it) must not pin the session there: the
	// approval covers the outage, not every later turn.
	implicitFallback := rt.Fallback == "escalation" && st.cfg.EscalationAgentIsImplicit()
	if rt.Target == router.TargetEscalation && !rt.SingleTurnEscalate && !st.stickyEscalate && !implicitFallback && st.cfg.StickyEscalationEnabled() {
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
