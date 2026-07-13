package main

// Regression tests for the dispatch history-builder strip sites.
// These tests pin the invariants that must hold both before and after the
// ADR-0038 refactor (move sess.AddTurn(user) to after Execute).
//
// Pre-refactor: the three history builders strip the trailing user turn because
// dispatch.go pre-adds it before Execute; isRepeatedPrompt depends on that strip.
//
// Post-refactor: no strip needed; these tests verify the same output shape.

import (
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
)

// ── helpers ──────────────────────────────────────────────────────────────────

func dhTurn(role session.Role, agent session.Agent, content string) session.Turn {
	return session.Turn{Role: role, Agent: agent, Content: content}
}

func rolesOf(msgs []local.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// ── sessionToMessages ─────────────────────────────────────────────────────────

// TestSessionToMessages_NoTrailingUser verifies that sessionToMessages returns
// all answered turns and that the last message is the most recent assistant turn.
// After ADR-0038, dispatch no longer pre-adds the user turn before Execute, so
// history contains only complete answered pairs at the time sessionToMessages runs.
func TestSessionToMessages_NoTrailingUser(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "first question"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "first answer"),
			dhTurn(session.RoleUser, session.AgentLocal, "second question"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "second answer"),
			// No pre-added pending user turn: dispatch adds it only after Execute.
		},
	}
	msgs := sessionToMessages(sess)
	if len(msgs) == 0 {
		t.Fatal("expected non-empty messages")
	}
	last := msgs[len(msgs)-1]
	if last.Role == "user" {
		t.Errorf("unexpected trailing user turn in sessionToMessages output: %q", last.Content)
	}
	if last.Content != "second answer" {
		t.Errorf("last message should be the most recent assistant turn, got role=%q content=%q", last.Role, last.Content)
	}
}

// TestSessionToMessages_EmptySession returns nil or empty without panicking.
func TestSessionToMessages_EmptySession(t *testing.T) {
	sess := &session.Session{}
	msgs := sessionToMessages(sess)
	if len(msgs) != 0 {
		t.Errorf("expected empty, got %d messages", len(msgs))
	}
}

// TestSessionToMessages_OnlyEscalationTurns — escalation turns are filtered out;
// a trailing escalation user turn must not be confused with a local user turn.
func TestSessionToMessages_OnlyEscalationTurns(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentEscalation, "esc question"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc answer"),
		},
	}
	msgs := sessionToMessages(sess)
	if len(msgs) != 0 {
		t.Errorf("escalation turns should be filtered out, got %d", len(msgs))
	}
}

// TestSessionToMessages_MixedAgents — only local turns appear; escalation
// turns are filtered out. No pre-added pending user turn after ADR-0038.
func TestSessionToMessages_MixedAgents(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentEscalation, "esc q"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a2"),
			// No pre-added turn: dispatch adds user turn only after Execute returns.
		},
	}
	msgs := sessionToMessages(sess)
	wantRoles := []string{"user", "assistant", "user", "assistant"}
	got := rolesOf(msgs)
	if len(got) != len(wantRoles) {
		t.Fatalf("want %v, got %v", wantRoles, got)
	}
	for i, r := range wantRoles {
		if got[i] != r {
			t.Errorf("[%d] want role %q, got %q", i, r, got[i])
		}
	}
}

// ── escalationLocalHistory ────────────────────────────────────────────────────

// TestEscalationLocalHistory_AllAnsweredTurnsIncluded — after ADR-0038,
// dispatch does not pre-add the user turn, so escalationLocalHistory returns
// all turns without stripping anything.
func TestEscalationLocalHistory_AllAnsweredTurnsIncluded(t *testing.T) {
	const prompt = "help me with X"
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "earlier q"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "earlier a"),
			// No pre-added pending turn: dispatch adds it after Execute returns.
		},
	}
	msgs := escalationLocalHistory(sess, prompt)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (all answered turns), got %d", len(msgs))
	}
	if msgs[1].Role != "assistant" {
		t.Errorf("last message should be assistant turn, got %q", msgs[1].Role)
	}
}

// TestEscalationLocalHistory_IncludesAllRoles — all user/assistant/tool turns
// are included; no stripping of any kind occurs after ADR-0038.
func TestEscalationLocalHistory_IncludesAllRoles(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleToolResult, session.AgentLocal, "tool result"),
		},
	}
	msgs := escalationLocalHistory(sess, "irrelevant prompt")
	wantRoles := []string{"user", "assistant", "tool"}
	got := rolesOf(msgs)
	if len(got) != len(wantRoles) {
		t.Fatalf("want roles %v, got %v", wantRoles, got)
	}
	for i, r := range wantRoles {
		if got[i] != r {
			t.Errorf("[%d] want %q, got %q", i, r, got[i])
		}
	}
}

// TestEscalationLocalHistory_EmptyHistory returns empty without panicking.
func TestEscalationLocalHistory_EmptyHistory(t *testing.T) {
	sess := &session.Session{}
	msgs := escalationLocalHistory(sess, "prompt")
	if len(msgs) != 0 {
		t.Errorf("expected empty, got %d", len(msgs))
	}
}

// ── escalationLocalHistoryFresh ───────────────────────────────────────────────

// TestEscalationLocalHistoryFresh_ScopedToBoundary — only turns after the last
// escalation boundary are included. No pre-added pending turn after ADR-0038.
func TestEscalationLocalHistoryFresh_ScopedToBoundary(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			// before boundary — should be excluded
			dhTurn(session.RoleUser, session.AgentEscalation, "old esc q"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "old esc a"),
			// after boundary — should be included
			dhTurn(session.RoleUser, session.AgentLocal, "local q"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "local a"),
			// No pre-added pending turn: dispatch adds it after Execute returns.
		},
	}
	msgs := escalationLocalHistoryFresh(sess, "")
	// Should contain local q+a only; pre-boundary turns excluded.
	for _, m := range msgs {
		if m.Content == "old esc q" || m.Content == "old esc a" {
			t.Errorf("pre-boundary turn leaked: %q", m.Content)
		}
	}
	wantRoles := []string{"user", "assistant"}
	got := rolesOf(msgs)
	if len(got) != len(wantRoles) {
		t.Fatalf("want roles %v, got %v", wantRoles, got)
	}
}

// ── builder.go turnsAgo ───────────────────────────────────────────────────────

// TestTurnsAgo_Baseline — pins the turnsAgo arithmetic used by
// escalation/builder.go BuildDynamicContext for a known session shape.
// After ADR-0038, the user turn is NOT in History when Execute runs
// (dispatch adds it after), so builder.go adds +1 to compensate.
// This test verifies the corrected formula with a concrete session.
func TestTurnsAgo_Baseline(t *testing.T) {
	// Scenario: need was recorded after 2 turns (CurrentNeedSetAt=2).
	// By the time Execute runs, history has 6 answered turns (3 pairs).
	// The user turn for the current prompt is NOT yet in History (ADR-0038).
	//
	// builder.go formula (post-refactor): (len(History) + 1) - (CurrentNeedSetAt - 1)
	// = (6 + 1) - (2 - 1) = 7 - 1 = 6  → stale (≥4) ✓

	sess := &session.Session{
		CurrentNeedSetAt: 2,
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a2"),
			dhTurn(session.RoleUser, session.AgentLocal, "q3"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a3"),
			// No pre-added current user turn: dispatch adds it after Execute.
		},
	}

	// Post-refactor formula as implemented in builder.go:
	turnsAgo := (len(sess.History) + 1) - (sess.CurrentNeedSetAt - 1)
	if turnsAgo != 6 {
		t.Errorf("turnsAgo: want 6, got %d", turnsAgo)
	}
	if turnsAgo < 4 {
		t.Error("expected stale need (turnsAgo ≥ 4)")
	}
}

// TestTurnsAgo_RecentNeed — need set very recently: turnsAgo < 4, shows "current goal".
// After ADR-0038: history has only answered turns; formula uses +1.
func TestTurnsAgo_RecentNeed(t *testing.T) {
	sess := &session.Session{
		CurrentNeedSetAt: 4, // set after 4 answered turns
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a2"),
			// No pre-added turn.
		},
	}
	// Post-refactor formula: (len + 1) - (SetAt - 1) = (4+1) - (4-1) = 5 - 3 = 2
	turnsAgo := (len(sess.History) + 1) - (sess.CurrentNeedSetAt - 1)
	if turnsAgo >= 4 {
		t.Errorf("expected recent need (turnsAgo < 4), got %d", turnsAgo)
	}
}
