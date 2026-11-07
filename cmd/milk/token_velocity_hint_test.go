package main

import (
	"context"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/loop"
	"github.com/scoutme/milk/internal/obs"
	"github.com/scoutme/milk/internal/oversight"
	"github.com/scoutme/milk/internal/session"
	"github.com/scoutme/milk/internal/testutil"
)

// newTestModelForTokenVelocity builds a minimal model with a real loop
// detector wired, following the same pattern as newTestModelForPanels.
// handleAgentDone recomputes m.lastTurnPrompt/lastTurnCompletion itself from
// obs's global per-session token accumulator (delta since the model's own
// primaryPrompt/primaryCompletion baseline, both zero on a fresh model) —
// setting those map fields directly before calling it has no effect, so the
// caller must go through obs.RecordTokens instead.
func newTestModelForTokenVelocity(t *testing.T) model {
	t.Helper()
	testutil.SandboxHome(t)
	obs.ResetSessionTokens()
	t.Cleanup(obs.ResetSessionTokens)
	sess, err := session.New("/repo", "")
	if err != nil {
		t.Fatal(err)
	}
	st := &interactiveState{sess: sess, cwd: "/repo", notifier: oversight.Noop{}}
	m := newModel(context.Background(), st, nil, dispatchAgents{}, nil)
	// A low threshold makes a single turn's recorded tokens fire reliably
	// and at high confidence (ratio-scaled, capped at 0.9 — see
	// checkTokenVelocity) without needing to replay multiple turns.
	m.loopDetector = loop.New(loop.Config{Enabled: true, TokenVelocityThreshold: 1000, TokenVelocitySeconds: 60})
	return m
}

// TestTokenVelocityHint_SuggestsEscalate: Track A Phase 5 of
// docs/internal/plans/escalation-and-context-enhancements-plan.md — token_velocity is the
// one cross-turn signal that fires with no corresponding loop/duplicate
// detection (legitimate but fast token burn), and previously was a pure
// warning nobody could act on. It must now suggest /escalate for the next
// turn, surfaced in the status bar (loopWarning), not just the transcript.
func TestTokenVelocityHint_SuggestsEscalate(t *testing.T) {
	m := newTestModelForTokenVelocity(t)
	obs.RecordTokens(context.Background(), "test-model", "primary", 4000, 1000)

	updated, _ := m.handleAgentDone(agentDoneMsg{})
	mm := updated.(model)

	if !strings.Contains(mm.loopWarning, "consider /escalate") {
		t.Errorf("expected the status-bar warning to suggest /escalate, got %q", mm.loopWarning)
	}
}

// TestTokenVelocityHint_NoSuggestionWhenAlreadyStickyEscalated confirms the
// hint is suppressed once the session is already heading to escalation —
// nothing new to suggest.
func TestTokenVelocityHint_NoSuggestionWhenAlreadyStickyEscalated(t *testing.T) {
	m := newTestModelForTokenVelocity(t)
	m.st.autoStickyEscalate = true
	obs.RecordTokens(context.Background(), "test-model", "escalation", 4000, 1000)

	updated, _ := m.handleAgentDone(agentDoneMsg{})
	mm := updated.(model)

	if strings.Contains(mm.loopWarning, "/escalate") {
		t.Errorf("expected no escalate suggestion while already sticky-escalated, got %q", mm.loopWarning)
	}
}

// TestTokenVelocityHint_OtherSignalsUnaffected confirms a non-token_velocity
// signal's warning text is never modified by this change — the suggestion
// is specific to token_velocity.
func TestTokenVelocityHint_OtherSignalsUnaffected(t *testing.T) {
	m := newTestModelForTokenVelocity(t)
	// silent_burn: high input tokens, near-zero output — a different signal
	// than token_velocity (threshold raised out of reach), should never
	// carry the escalate suggestion.
	m.loopDetector = loop.New(loop.Config{Enabled: true, MaxSilentBurnTokens: 100, TokenVelocityThreshold: 1 << 30})
	obs.RecordTokens(context.Background(), "test-model", "primary", 50000, 1)

	updated, _ := m.handleAgentDone(agentDoneMsg{})
	mm := updated.(model)

	if strings.Contains(mm.loopWarning, "/escalate") {
		t.Errorf("expected no escalate suggestion for a non-token_velocity signal, got %q", mm.loopWarning)
	}
}
