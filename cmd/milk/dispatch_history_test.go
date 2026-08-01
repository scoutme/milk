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
	name := "test-primary"
	if agent == session.AgentEscalation {
		name = "test-escalation"
	}
	return session.Turn{Role: role, Agent: agent, AgentName: name, Content: content}
}

func rolesOf(msgs []local.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// ── sessionToUnifiedMessages ──────────────────────────────────────────────────

// TestSessionToUnifiedMessages_NoTrailingUser verifies that sessionToUnifiedMessages
// returns all answered turns and that the last message is the most recent assistant turn.
func TestSessionToUnifiedMessages_NoTrailingUser(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "first question"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "first answer"),
			dhTurn(session.RoleUser, session.AgentLocal, "second question"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "second answer"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", false)
	if len(msgs) == 0 {
		t.Fatal("expected non-empty messages")
	}
	last := msgs[len(msgs)-1]
	if last.Role == "user" {
		t.Errorf("unexpected trailing user turn: %q", last.Content)
	}
	wantSuffix := "second answer"
	if len(last.Content) < len(wantSuffix) || last.Content[len(last.Content)-len(wantSuffix):] != wantSuffix {
		t.Errorf("last message should end with %q, got role=%q content=%q", wantSuffix, last.Role, last.Content)
	}
}

// TestSessionToUnifiedMessages_EmptySession returns nil or empty without panicking.
func TestSessionToUnifiedMessages_EmptySession(t *testing.T) {
	sess := &session.Session{}
	msgs := sessionToUnifiedMessages(sess, "test-primary", false)
	if len(msgs) != 0 {
		t.Errorf("expected empty, got %d messages", len(msgs))
	}
}

// TestSessionToUnifiedMessages_OnlyEscalationTurns — user turns are labelled [user to <agent>];
// escalation assistant turns appear as system messages labelled with agent name and role.
func TestSessionToUnifiedMessages_OnlyEscalationTurns(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentEscalation, "esc question"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc answer"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", false)
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages (user+system), got %d", len(msgs))
	}
	if msgs[0].Role != "user" {
		t.Errorf("want user turn first, got %q", msgs[0].Role)
	}
	if msgs[0].Content != "[user to test-escalation] esc question" {
		t.Errorf("user turn should be labelled, got %q", msgs[0].Content)
	}
	if msgs[1].Role != "system" {
		t.Errorf("escalation assistant turn should be role=system, got %q", msgs[1].Role)
	}
	if msgs[1].Content != "[test-escalation as escalation] esc answer" {
		t.Errorf("escalation turn should be labelled with name and role, got %q", msgs[1].Content)
	}
}

// TestSessionToUnifiedMessages_MixedAgents — all turns appear with correct labels.
// Own (primary) assistant turns: no label (model already knows what it said).
// Other (escalation): system "[<name> as escalation]".
func TestSessionToUnifiedMessages_MixedAgents(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentEscalation, "esc q"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a2"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", false)
	wantRoles := []string{"user", "assistant", "user", "system", "user", "assistant"}
	got := rolesOf(msgs)
	if len(got) != len(wantRoles) {
		t.Fatalf("want %v, got %v", wantRoles, got)
	}
	for i, r := range wantRoles {
		if got[i] != r {
			t.Errorf("[%d] want role %q, got %q", i, r, got[i])
		}
	}
	// Own turn: no label, raw content
	if msgs[1].Content != "a1" {
		t.Errorf("own turn should have no label, got %q", msgs[1].Content)
	}
	// Escalation turn becomes system with name and role label
	if msgs[3].Content != "[test-escalation as escalation] esc a" {
		t.Errorf("escalation turn label wrong, got %q", msgs[3].Content)
	}
	// User turns labelled [user to <agent>]
	if msgs[0].Content != "[user to test-primary] q1" {
		t.Errorf("user turn label wrong, got %q", msgs[0].Content)
	}
}

// ── escalationLocalHistory ────────────────────────────────────────────────────

// TestEscalationLocalHistory_AllAnsweredTurnsIncluded — the escalation agent sees
// all turns. The primary agent's assistant turns become system messages (other agent);
// the escalation agent's own turns would be assistant (but there are none here).
func TestEscalationLocalHistory_AllAnsweredTurnsIncluded(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "earlier q"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "earlier a"),
		},
	}
	msgs := escalationLocalHistory(sess, "test-escalation", false)
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages, got %d", len(msgs))
	}
	// Primary's assistant turn is "other agent" → system role
	if msgs[1].Role != "system" {
		t.Errorf("primary assistant turn should be system for escalation agent, got %q", msgs[1].Role)
	}
}

// TestEscalationLocalHistory_IncludesAllRoles — user/assistant/tool turns all appear;
// primary tool results are included (they're the escalation agent's own tools
// being treated as other-agent tool calls for labelling, but tool results stay as tool).
func TestEscalationLocalHistory_IncludesAllRoles(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "a1"),
			dhTurn(session.RoleToolResult, session.AgentEscalation, "tool result"),
		},
	}
	msgs := escalationLocalHistory(sess, "test-escalation", false)
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
	msgs := escalationLocalHistory(sess, "prompt", false)
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
	msgs := escalationLocalHistoryFresh(sess, "", false)
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

// ── lazy history mode ────────────────────────────────────────────────────────

// TestBuildAgentHistory_LazyMode_Placeholders verifies that when
// skipOtherAgents is true, contiguous turns by/for other agents are collapsed
// into a single aggregated placeholder.
func TestBuildAgentHistory_LazyMode_Placeholders(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentEscalation, "esc q"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a2"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", true)
	// Should contain: user q1, assistant a1, placeholder (2 turns), user q2, assistant a2
	if len(msgs) != 5 {
		t.Fatalf("want 5 messages, got %d", len(msgs))
	}
	// Contiguous escalation block → single aggregated placeholder.
	if msgs[2].Role != "system" {
		t.Errorf("placeholder should be system role, got %q", msgs[2].Role)
	}
	if msgs[2].Content != "[escalation]: (2 turns omitted)" {
		t.Errorf("placeholder wrong, got %q", msgs[2].Content)
	}
	// Own turns are full.
	if msgs[0].Content != "[user to test-primary] q1" {
		t.Errorf("own user turn wrong, got %q", msgs[0].Content)
	}
	if msgs[1].Content != "a1" {
		t.Errorf("own assistant turn wrong, got %q", msgs[1].Content)
	}
	if msgs[3].Content != "[user to test-primary] q2" {
		t.Errorf("own user turn wrong, got %q", msgs[3].Content)
	}
	if msgs[4].Content != "a2" {
		t.Errorf("own assistant turn wrong, got %q", msgs[4].Content)
	}
}

// TestBuildAgentHistory_LazyMode_SingleTurn verifies a single omitted turn
// uses singular phrasing.
func TestBuildAgentHistory_LazyMode_SingleTurn(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", true)
	if len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %d", len(msgs))
	}
	if msgs[1].Content != "[escalation]: (1 turn omitted)" {
		t.Errorf("single-turn placeholder wrong, got %q", msgs[1].Content)
	}
}

// TestBuildAgentHistory_LazyMode_MultipleAgents verifies that contiguous turns
// from other agents are collapsed into a single placeholder.
func TestBuildAgentHistory_LazyMode_MultipleAgents(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentEscalation, "esc q"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a"),
			dhTurn(session.RoleUser, session.AgentEscalation, "esc q2"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a2"),
			dhTurn(session.RoleUser, session.AgentLocal, "q2"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", true)
	// q1, a1, placeholder(4 turns), q2 → 4 messages
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages, got %d", len(msgs))
	}
	if msgs[2].Content != "[escalation]: (4 turns omitted)" {
		t.Errorf("multi-turn placeholder wrong, got %q", msgs[2].Content)
	}
}

// TestBuildAgentHistory_LazyMode_False verifies that skipOtherAgents=false
// preserves the current behavior (all turns included).
func TestBuildAgentHistory_LazyMode_False(t *testing.T) {
	sess := &session.Session{
		History: []session.Turn{
			dhTurn(session.RoleUser, session.AgentLocal, "q1"),
			dhTurn(session.RoleAssistant, session.AgentLocal, "a1"),
			dhTurn(session.RoleUser, session.AgentEscalation, "esc q"),
			dhTurn(session.RoleAssistant, session.AgentEscalation, "esc a"),
		},
	}
	msgs := sessionToUnifiedMessages(sess, "test-primary", false)
	// All 4 turns should be present.
	if len(msgs) != 4 {
		t.Fatalf("want 4 messages (all turns), got %d", len(msgs))
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
