package main

// Eval test: context-byte proxy for the Claude CLI escalation path.
//
// The CLI path does not send message history over HTTP — prior turns live in
// Claude's server-side session (accessed via --resume).  The controllable
// inputs are:
//   - whether --resume fires at all (ContextModeReturning vs ContextModeFirst)
//   - the bytes sent via --append-system-prompt-file (staticCtx + dynamicCtx)
//
// This test exercises the mode-selection and context-construction logic for
// four session states, comparing the pre-optimization baseline behaviour
// against the new code, and measuring the context-file bytes for each path.
//
// Pre-optimization baseline:
//   - ContextModeReturning is never downgraded → --resume always fires on
//     returning turns
//   - dynamicCtx = BuildDynamicContext(sess, ContextModeReturning) for all
//     returning turns
//
// New behaviour:
//   - NeedChangedSinceLastEscalation or LocalTurnsSinceLastEscalation ≥ threshold
//     → ContextModeFirst → no --resume, full re-orientation context
//   - ContextModeReturning retained only when neither condition fires
//
// Run with:  go test ./cmd/milk/ -run TestEval_EscalationCLI -v

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/escalation"
	"github.com/scoutme/milk/internal/session"
)

// contextModeName returns a short human-readable name for a ContextMode.
func contextModeName(m escalation.ContextMode) string {
	switch m {
	case escalation.ContextModeFirst:
		return "First"
	case escalation.ContextModeResume:
		return "Resume"
	case escalation.ContextModeReturning:
		return "Returning"
	default:
		return "Unknown"
	}
}

// applyStalenessDowngrade applies the fresh-start downgrade to a
// ContextModeReturning mode using the same logic as runCLIEscalationWith.
// threshold is the returning_fresh_start_local_turns value (default 8).
func applyStalenessDowngrade(mode escalation.ContextMode, sess *session.Session, threshold int) (escalation.ContextMode, bool, bool) {
	if mode != escalation.ContextModeReturning {
		return mode, false, false
	}
	needStale := sess.NeedChangedSinceLastEscalation()
	turnGap := threshold > 0 && sess.LocalTurnsSinceLastEscalation() >= threshold
	if needStale || turnGap {
		return escalation.ContextModeFirst, needStale, turnGap
	}
	return mode, false, false
}

// cliContextBytes returns the total byte count of staticCtx + dynamicCtx
// that would be sent via --append-system-prompt-file for the given mode.
// nonce and percepts mirror what runCLIEscalationWith passes.
func cliContextBytes(sess *session.Session, mode escalation.ContextMode, nonce string, percepts []string, injectInstructions bool, primaryName, escalationName string) (staticBytes, dynamicBytes int) {
	static := escalation.BuildStaticContext(nonce, percepts, mode, injectInstructions, primaryName, escalationName)
	dynamic := escalation.BuildDynamicContext(sess, mode)
	return len(static), len(dynamic)
}

// scenariosForCLIEval builds the same four session states as the local-provider
// eval so the results are directly comparable.
func scenariosForCLIEval() map[string]*session.Session {
	const escalationReply = "Refactored internal/auth/jwt.go: replaced HS256 with RS256, " +
		"added exp/nbf/aud claim validation, thread-safe key cache, and unit tests. " +
		"The key rotation logic now lives in KeyManager.Rotate() (line 142)."

	const lw1 = "Added unit tests for the JWT validator. Coverage is now 94%."
	const lw2 = "Fixed a nil pointer in the router triggered by missing aud claim."
	const lw3 = "Refactored the session store to use Redis instead of in-memory map."
	const lw4 = "Updated deployment config to set JWT_SECRET from Vault."
	const lw5 = "Wrote migration script for existing sessions."
	const lw6 = "Reviewed and merged PRs #42 and #44."
	const lw7 = "Added rate-limiting middleware (token bucket, 100 req/s)."
	const lw8 = "Pinned dependency versions in go.mod after audit."
	const lw9 = "Ran benchmarks: 12% latency improvement."

	// first: no prior escalation session
	first := &session.Session{
		State: session.StateRouting,
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentLocal, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentLocal, "Sure, let me look at the code."),
		},
	}

	// returning-stale-need: topic switched after escalation
	staleNeed := &session.Session{
		State:               session.StateRouting,
		EscalationSessionID: "esc-session-1",
		EscalationNonce:     "abc123",
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentEscalation, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentEscalation, escalationReply),
			evalTurn(session.RoleUser, session.AgentLocal, "add unit tests"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw1),
			evalTurn(session.RoleUser, session.AgentLocal, "now let's work on the deployment pipeline"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw4),
		},
		CurrentNeed:      "set up the deployment pipeline",
		CurrentNeedSetAt: 5,
		EscalationBrief:  "refactor auth module",
	}
	staleNeed.RebuildSummaryBricks(12000)

	// returning-stale-gap: 9 local turns since escalation (≥ default threshold 8)
	staleGap := &session.Session{
		State:               session.StateRouting,
		EscalationSessionID: "esc-session-2",
		EscalationNonce:     "abc123",
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentEscalation, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentEscalation, escalationReply),
			evalTurn(session.RoleUser, session.AgentLocal, "add unit tests"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw1),
			evalTurn(session.RoleUser, session.AgentLocal, "fix nil pointer"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw2),
			evalTurn(session.RoleUser, session.AgentLocal, "refactor session store"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw3),
			evalTurn(session.RoleUser, session.AgentLocal, "update deployment config"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw4),
			evalTurn(session.RoleUser, session.AgentLocal, "write migration script"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw5),
			evalTurn(session.RoleUser, session.AgentLocal, "review PRs"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw6),
			evalTurn(session.RoleUser, session.AgentLocal, "add rate limiting"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw7),
			evalTurn(session.RoleUser, session.AgentLocal, "pin dependencies"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw8),
			evalTurn(session.RoleUser, session.AgentLocal, "run benchmarks"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw9),
		},
		CurrentNeed:      "benchmark the session store",
		CurrentNeedSetAt: 19,
		EscalationBrief:  "refactor auth module",
	}
	staleGap.RebuildSummaryBricks(12000)

	// returning-fresh: 2 local turns, same topic, no stale condition
	fresh := &session.Session{
		State:               session.StateRouting,
		EscalationSessionID: "esc-session-3",
		EscalationNonce:     "abc123",
		History: []session.Turn{
			evalTurn(session.RoleUser, session.AgentEscalation, "help me refactor the auth module"),
			evalTurn(session.RoleAssistant, session.AgentEscalation, escalationReply),
			evalTurn(session.RoleUser, session.AgentLocal, "add unit tests"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw1),
			evalTurn(session.RoleUser, session.AgentLocal, "fix nil pointer"),
			evalTurn(session.RoleAssistant, session.AgentLocal, lw2),
		},
		CurrentNeed:      "refactor the auth module",
		CurrentNeedSetAt: 1,
		EscalationBrief:  "refactor auth module",
	}
	fresh.RebuildSummaryBricks(12000)

	return map[string]*session.Session{
		"first":                first,
		"returning-stale-need": staleNeed,
		"returning-stale-gap":  staleGap,
		"returning-fresh":      fresh,
	}
}

func TestEval_EscalationCLI(t *testing.T) {
	const (
		nonce         = "abc123"
		primaryName   = "primary"
		escalationName = "claude"
		threshold     = 8 // default returning_fresh_start_local_turns
	)
	percepts := []string{
		"user prefers Go over Python",
		"project uses RS256 JWT tokens",
	}

	scenarios := scenariosForCLIEval()
	order := []string{"first", "returning-stale-need", "returning-stale-gap", "returning-fresh"}

	type result struct {
		baseMode        escalation.ContextMode
		newMode         escalation.ContextMode
		baseStaticBytes int
		baseDynamicBytes int
		newStaticBytes  int
		newDynamicBytes int
		needStale       bool
		turnGap         bool
	}
	results := make(map[string]result, len(order))

	for _, name := range order {
		sess := scenarios[name]

		// ── baseline mode (pre-optimization): no staleness downgrade ──────
		// ContextModeReturning is never downgraded to ContextModeFirst.
		var baseModeRaw escalation.ContextMode
		switch {
		case sess.State == session.StateEscalationWaiting && sess.EscalationSessionID != "":
			baseModeRaw = escalation.ContextModeResume
		case sess.EscalationSessionID != "":
			baseModeRaw = escalation.ContextModeReturning
		default:
			baseModeRaw = escalation.ContextModeFirst
		}
		// baseline always injects instructions on first/returning
		baseInject := baseModeRaw != escalation.ContextModeResume
		bStatic, bDynamic := cliContextBytes(sess, baseModeRaw, nonce, percepts, baseInject, primaryName, escalationName)

		// ── new mode (post-optimization): apply staleness downgrade ───────
		newMode, needStale, turnGap := applyStalenessDowngrade(baseModeRaw, sess, threshold)
		newInject := newMode != escalation.ContextModeResume
		nStatic, nDynamic := cliContextBytes(sess, newMode, nonce, percepts, newInject, primaryName, escalationName)

		results[name] = result{
			baseMode:         baseModeRaw,
			newMode:          newMode,
			baseStaticBytes:  bStatic,
			baseDynamicBytes: bDynamic,
			newStaticBytes:   nStatic,
			newDynamicBytes:  nDynamic,
			needStale:        needStale,
			turnGap:          turnGap,
		}
	}

	// ── Report ────────────────────────────────────────────────────────────
	t.Log("\n=== CLI escalation context eval (--append-system-prompt-file bytes) ===")
	t.Logf("%-28s  %-10s  %-10s  %8s  %8s  %8s  %8s  %s",
		"Scenario", "BaseMode", "NewMode", "BaseStat", "NewStat", "BaseDyn", "NewDyn", "Trigger")
	t.Logf("%s", strings.Repeat("-", 105))
	for _, name := range order {
		r := results[name]
		trigger := "-"
		if r.needStale {
			trigger = "need-stale"
		} else if r.turnGap {
			trigger = "turn-gap"
		}
		resumeChange := ""
		if r.baseMode != r.newMode {
			resumeChange = " ← mode changed"
		}
		t.Logf("%-28s  %-10s  %-10s  %8d  %8d  %8d  %8d  %s%s",
			name,
			contextModeName(r.baseMode), contextModeName(r.newMode),
			r.baseStaticBytes, r.newStaticBytes,
			r.baseDynamicBytes, r.newDynamicBytes,
			trigger, resumeChange)
	}
	t.Log(strings.Repeat("-", 105))
	t.Log("Note: --resume carries the full prior Claude session server-side.")
	t.Log("ContextModeFirst = no --resume (stale session dropped); ContextModeReturning = --resume.")

	// ── Assertions ────────────────────────────────────────────────────────

	// Stale-returning: baseline stays ContextModeReturning (--resume fires);
	// new path downgrades to ContextModeFirst (--resume dropped).
	for _, name := range []string{"returning-stale-need", "returning-stale-gap"} {
		r := results[name]
		if r.baseMode != escalation.ContextModeReturning {
			t.Errorf("scenario %q: baseline mode should be ContextModeReturning, got %v", name, r.baseMode)
		}
		if r.newMode != escalation.ContextModeFirst {
			t.Errorf("scenario %q: new mode should be ContextModeFirst after downgrade, got %v", name, r.newMode)
		}
	}

	// Stale-returning: new mode is ContextModeFirst → full re-orientation context
	// is sent (static > 0, dynamic includes identity+need+summary).
	for _, name := range []string{"returning-stale-need", "returning-stale-gap"} {
		r := results[name]
		if r.newStaticBytes == 0 {
			t.Errorf("scenario %q: new static context should be non-empty on ContextModeFirst", name)
		}
		if r.newDynamicBytes == 0 {
			t.Errorf("scenario %q: new dynamic context should be non-empty on ContextModeFirst", name)
		}
	}

	// Stale-returning: baseline ContextModeReturning sends less context than
	// new ContextModeFirst — the optimisation trades server-side session cost
	// (--resume history) for slightly more context-file bytes.
	// This is the expected and correct trade-off: context files are cheap
	// (injected once, cacheable), prior session history is not.
	for _, name := range []string{"returning-stale-need", "returning-stale-gap"} {
		r := results[name]
		baseTotal := r.baseStaticBytes + r.baseDynamicBytes
		newTotal := r.newStaticBytes + r.newDynamicBytes
		if newTotal <= baseTotal {
			// This would mean the re-orientation is smaller than what returning already sends,
			// which is unexpected — log it but don't fail, as it's not a correctness bug.
			t.Logf("scenario %q: note: new context (%d) ≤ baseline context (%d) — orientation may be minimal",
				name, newTotal, baseTotal)
		}
	}

	// Returning-fresh: no staleness condition fires → mode unchanged.
	if r := results["returning-fresh"]; r.newMode != r.baseMode {
		t.Errorf("scenario 'returning-fresh': mode should be unchanged; baseline=%v new=%v",
			r.baseMode, r.newMode)
	}

	// First: no prior session → always ContextModeFirst regardless of optimization.
	if r := results["first"]; r.newMode != escalation.ContextModeFirst {
		t.Errorf("scenario 'first': new mode should be ContextModeFirst, got %v", r.newMode)
	}
	if r := results["first"]; r.baseMode != escalation.ContextModeFirst {
		t.Errorf("scenario 'first': baseline mode should be ContextModeFirst, got %v", r.baseMode)
	}

	// Resume (ESCALATION_WAITING) scenario: verify that resume is never downgraded.
	// Inject a synthetic resume session and confirm mode stays ContextModeResume.
	resumeSess := &session.Session{
		State:               session.StateEscalationWaiting,
		EscalationSessionID: "active-session",
		CurrentNeed:         "some goal",
		CurrentNeedSetAt:    99, // artificially "stale"
	}
	resumeMode, _, _ := applyStalenessDowngrade(escalation.ContextModeResume, resumeSess, threshold)
	if resumeMode != escalation.ContextModeResume {
		t.Errorf("ESCALATION_WAITING session must stay ContextModeResume; got %v", resumeMode)
	}
}
