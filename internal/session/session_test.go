package session

import (
	"testing"
)

func TestTransition_ValidPaths(t *testing.T) {
	cases := []struct {
		from State
		to   State
		want bool
	}{
		{StateRouting, StateLocal, true},
		{StateRouting, StateClaude, true},
		{StateLocal, StateClaude, true},
		{StateLocal, StateRouting, true},
		{StateClaude, StateClaudeWaiting, true},
		{StateClaude, StateRouting, true},
		{StateClaudeWaiting, StateClaude, true},
		{StateClaudeWaiting, StateRouting, true},
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
		{StateRouting, StateClaudeWaiting},
		{StateLocal, StateClaudeWaiting},
		{StateClaudeWaiting, StateLocal},
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
	if s.LastClaudeSummary != "" {
		t.Errorf("expected empty LastClaudeSummary with no Claude turns, got %q", s.LastClaudeSummary)
	}
}

func TestRebuildSummaryBricks_LocalSinceLastClaude(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "old local turn"),
		mkTurn(RoleAssistant, AgentLocal, "old answer"),
		mkTurn(RoleUser, AgentClaude, "escalated"),
		mkTurn(RoleAssistant, AgentClaude, "claude reply with enough text to count"),
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
		LastClaudeSummary: "previous summary",
		History: []Turn{
			mkTurn(RoleUser, AgentClaude, "hi"),
			mkTurn(RoleAssistant, AgentClaude, "ok"), // < 200 chars, no tool calls
		},
	}
	s.RebuildSummaryBricks(12000)
	if s.LastClaudeSummary != "previous summary" {
		t.Errorf("empty Claude session should not update LastClaudeSummary, got %q", s.LastClaudeSummary)
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
