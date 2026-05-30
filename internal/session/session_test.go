package session

import (
	"strings"
	"testing"
)

func TestTransition_ValidPaths(t *testing.T) {
	cases := []struct {
		from State
		to   State
		want bool
	}{
		{StateRouting, StateLocal, true},
		{StateRouting, StateEscalation, true},
		{StateLocal, StateEscalation, true},
		{StateLocal, StateRouting, true},
		{StateEscalation, StateEscalationWaiting, true},
		{StateEscalation, StateRouting, true},
		{StateEscalationWaiting, StateEscalation, true},
		{StateEscalationWaiting, StateRouting, true},
	}
	for _, tc := range cases {
		s := &Session{State: tc.from}
		got := s.Transition(tc.to)
		if got != tc.want {
			t.Errorf("Transition(%s → %s): want %v got %v", tc.from, tc.to, tc.want, got)
		}
	}
}

func TestTransition_InvalidPaths(t *testing.T) {
	cases := []struct{ from, to State }{
		{StateRouting, StateEscalationWaiting},
		{StateLocal, StateEscalationWaiting},
		{StateEscalationWaiting, StateLocal},
	}
	for _, tc := range cases {
		s := &Session{State: tc.from}
		if s.Transition(tc.to) {
			t.Errorf("Transition(%s → %s) should be invalid", tc.from, tc.to)
		}
	}
}

func mkTurn(role Role, agent Agent, content string) Turn {
	return Turn{Role: role, Agent: agent, Content: content}
}

func TestRebuildSummaryBricks_LocalOnly(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "fix typo"),
		mkTurn(RoleAssistant, AgentLocal, "done"),
	}}
	s.RebuildSummaryBricks(12000)
	if !contains(s.LastLocalSummary, "fix typo") {
		t.Errorf("expected local turn in LastLocalSummary, got %q", s.LastLocalSummary)
	}
	if s.LastEscalationSummary != "" {
		t.Errorf("expected empty LastEscalationSummary with no Claude turns, got %q", s.LastEscalationSummary)
	}
}

func TestRebuildSummaryBricks_LocalSinceLastClaude(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "old local turn"),
		mkTurn(RoleAssistant, AgentLocal, "old answer"),
		mkTurn(RoleUser, AgentEscalation, "escalated"),
		mkTurn(RoleAssistant, AgentEscalation, "claude reply with enough text to count"),
		mkTurn(RoleUser, AgentLocal, "new local turn"),
		mkTurn(RoleAssistant, AgentLocal, "new answer"),
	}}
	s.RebuildSummaryBricks(12000)
	if contains(s.LastLocalSummary, "old local turn") {
		t.Error("LastLocalSummary should not contain turns before last Claude boundary")
	}
	if !contains(s.LastLocalSummary, "new local turn") {
		t.Errorf("expected post-Claude local turn in LastLocalSummary, got %q", s.LastLocalSummary)
	}
}

func TestRebuildSummaryBricks_ClaudeEmptySessionNotUpdated(t *testing.T) {
	s := &Session{
		LastEscalationSummary: "previous summary",
		History: []Turn{
			mkTurn(RoleUser, AgentEscalation, "hi"),
			mkTurn(RoleAssistant, AgentEscalation, "ok"), // < 200 chars, no tool calls
		},
	}
	s.RebuildSummaryBricks(12000)
	if s.LastEscalationSummary != "previous summary" {
		t.Errorf("empty Claude session should not update LastEscalationSummary, got %q", s.LastEscalationSummary)
	}
}

func TestRebuildSummaryBricks_BudgetTrimsOldest(t *testing.T) {
	// Each turn renders to ~30 chars; budget of 60 should keep ~2 turns.
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "turn one"),
		mkTurn(RoleAssistant, AgentLocal, "answer one"),
		mkTurn(RoleUser, AgentLocal, "turn two"),
		mkTurn(RoleAssistant, AgentLocal, "answer two"),
		mkTurn(RoleUser, AgentLocal, "turn three"),
		mkTurn(RoleAssistant, AgentLocal, "answer three"),
	}}
	s.RebuildSummaryBricks(60)
	// Should not contain the oldest turn.
	if contains(s.LastLocalSummary, "turn one") {
		t.Errorf("oldest turn should be trimmed under budget, got %q", s.LastLocalSummary)
	}
}

func TestRebuildSummaryBricks_ToolCallCollapse(t *testing.T) {
	s := &Session{History: []Turn{
		{Role: RoleUser, Agent: AgentLocal, Content: "search"},
		{Role: RoleAssistant, Agent: AgentLocal, ToolCalls: []ToolCall{{Name: "grep", Arguments: `{"pattern":"foo"}`}}},
		{Role: RoleToolResult, Content: "result1"},
		{Role: RoleAssistant, Agent: AgentLocal, ToolCalls: []ToolCall{{Name: "grep", Arguments: `{"pattern":"foo"}`}}},
		{Role: RoleToolResult, Content: "result2"},
		{Role: RoleAssistant, Agent: AgentLocal, ToolCalls: []ToolCall{{Name: "grep", Arguments: `{"pattern":"foo"}`}}},
		{Role: RoleToolResult, Content: "result3"},
	}}
	s.RebuildSummaryBricks(12000)
	if !contains(s.LastLocalSummary, "collapsed") {
		t.Errorf("expected consecutive identical tool calls to be collapsed, got %q", s.LastLocalSummary)
	}
}

func contains(s, substr string) bool {
	return len(s) > 0 && len(substr) > 0 && (s == substr || len(s) >= len(substr) && containsStr(s, substr))
}

func containsStr(s, sub string) bool {
	for i := range s {
		if len(s[i:]) >= len(sub) && s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func TestLocalTurnCount(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "hi"),
		mkTurn(RoleAssistant, AgentLocal, "hello"),
		mkTurn(RoleUser, AgentEscalation, "escalate"),
		mkTurn(RoleAssistant, AgentEscalation, "escalation reply"),
		mkTurn(RoleAssistant, AgentLocal, "back"),
	}}
	if got := s.LocalTurnCount(); got != 2 {
		t.Errorf("expected 2 local turns, got %d", got)
	}
}

func TestLocalOutputBytesSince(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleAssistant, AgentLocal, "aaa"),    // local turn index 0
		mkTurn(RoleAssistant, AgentLocal, "bbbbbb"), // local turn index 1
		mkTurn(RoleAssistant, AgentLocal, "cc"),     // local turn index 2
	}}
	// afterTurnIndex=0 means count turns with count>=0, i.e. all: 3+6+2=11
	if got := s.LocalOutputBytesSince(0); got != 11 {
		t.Errorf("LocalOutputBytesSince(0): want 11, got %d", got)
	}
	// afterTurnIndex=1: skip index 0 (count 0), include index 1+ → 6+2=8
	if got := s.LocalOutputBytesSince(1); got != 8 {
		t.Errorf("LocalOutputBytesSince(1): want 8, got %d", got)
	}
	// afterTurnIndex=3: nothing → 0
	if got := s.LocalOutputBytesSince(3); got != 0 {
		t.Errorf("LocalOutputBytesSince(3): want 0, got %d", got)
	}
}

func TestRebuildSummaryBricks_EscalationSinceLastLocalOnly(t *testing.T) {
	// Escalation turns followed by local turns: escalation brick should only
	// cover the escalation turns before the last local boundary.
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentEscalation, "old question"),
		mkTurn(RoleAssistant, AgentEscalation, "old escalation reply that is definitely long enough to count"),
		mkTurn(RoleUser, AgentLocal, "now local"),
		mkTurn(RoleAssistant, AgentLocal, "local answer"),
	}}
	s.RebuildSummaryBricks(12000)
	// After local work, escalation brick should be empty (local was most recent).
	if s.LastEscalationSummary != "" {
		t.Errorf("LastEscalationSummary should be empty when local was most recent, got %q", s.LastEscalationSummary)
	}
}

func TestRebuildSummaryBricks_EscalationBrickCoversOnlyRecentEscalation(t *testing.T) {
	// Local turns, then escalation turns: escalation brick should only cover
	// the escalation turns since the last local turn.
	longReply := "new escalation reply: " + strings.Repeat("x", 200) // > 200 chars to pass emptyEscalationSession
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "old local"),
		mkTurn(RoleAssistant, AgentLocal, "old local answer"),
		mkTurn(RoleUser, AgentEscalation, "escalated"),
		mkTurn(RoleAssistant, AgentEscalation, longReply),
	}}
	s.RebuildSummaryBricks(12000)
	if !contains(s.LastEscalationSummary, "new escalation reply") {
		t.Errorf("expected recent escalation turn in LastEscalationSummary, got %q", s.LastEscalationSummary)
	}
}

func TestEscalationMostRecent_EscalationLast(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleAssistant, AgentLocal, "local"),
		mkTurn(RoleAssistant, AgentEscalation, "escalation"),
	}}
	if !EscalationMostRecent(s) {
		t.Error("expected EscalationMostRecent=true when escalation turn is last")
	}
}

func TestEscalationMostRecent_LocalLast(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleAssistant, AgentEscalation, "escalation"),
		mkTurn(RoleAssistant, AgentLocal, "local"),
	}}
	if EscalationMostRecent(s) {
		t.Error("expected EscalationMostRecent=false when local turn is last")
	}
}

func TestEscalationMostRecent_NoHistory(t *testing.T) {
	s := &Session{}
	if EscalationMostRecent(s) {
		t.Error("expected EscalationMostRecent=false for empty history")
	}
}

func TestAddTurn_SetsTimestampAndUpdatesLastUsed(t *testing.T) {
	s := &Session{}
	s.AddTurn(Turn{Role: RoleUser, Content: "hello"})
	if len(s.History) != 1 {
		t.Fatalf("expected 1 turn, got %d", len(s.History))
	}
	if s.History[0].Timestamp.IsZero() {
		t.Error("timestamp not set")
	}
	if s.LastUsed.IsZero() {
		t.Error("last_used not updated")
	}
}
