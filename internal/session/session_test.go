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

func TestAddTokensFull_AccumulatesCacheTokens(t *testing.T) {
	s := &Session{}
	s.AddTokensFull("model-a", "escalation", 100, 20, 500, 80)
	s.AddTokensFull("model-a", "escalation", 50, 10, 200, 30)
	u := s.Tokens["model-a\x00escalation"]
	if u == nil {
		t.Fatal("expected token entry")
	}
	if u.Prompt != 150 {
		t.Errorf("prompt: got %d want 150", u.Prompt)
	}
	if u.CacheRead != 700 {
		t.Errorf("cache_read: got %d want 700", u.CacheRead)
	}
	if u.CacheCreation != 110 {
		t.Errorf("cache_creation: got %d want 110", u.CacheCreation)
	}
}

func TestAddTokens_DoesNotWriteCacheFields(t *testing.T) {
	s := &Session{}
	s.AddTokens("model-a", "primary", 100, 20)
	u := s.Tokens["model-a\x00primary"]
	if u == nil {
		t.Fatal("expected token entry")
	}
	if u.CacheRead != 0 || u.CacheCreation != 0 {
		t.Errorf("AddTokens should leave cache fields zero, got read=%d creation=%d", u.CacheRead, u.CacheCreation)
	}
}

func TestEscalationEverActive(t *testing.T) {
	mkTurn := func(role Role, agent Agent) Turn { return Turn{Role: role, Agent: agent} }

	t.Run("no history returns false", func(t *testing.T) {
		if EscalationEverActive(&Session{}) {
			t.Error("want false for empty history")
		}
	})
	t.Run("only local turns returns false", func(t *testing.T) {
		s := &Session{History: []Turn{mkTurn(RoleUser, AgentLocal), mkTurn(RoleAssistant, AgentLocal)}}
		if EscalationEverActive(s) {
			t.Error("want false when no escalation turns")
		}
	})
	t.Run("escalation turn present returns true", func(t *testing.T) {
		s := &Session{History: []Turn{
			mkTurn(RoleUser, AgentLocal),
			mkTurn(RoleAssistant, AgentEscalation),
		}}
		if !EscalationEverActive(s) {
			t.Error("want true when escalation turn present")
		}
	})
}

func TestLastEscalationBoundary(t *testing.T) {
	mkTurn := func(role Role, agent Agent) Turn { return Turn{Role: role, Agent: agent} }

	t.Run("no escalation returns 0", func(t *testing.T) {
		s := &Session{History: []Turn{mkTurn(RoleAssistant, AgentLocal)}}
		if got := LastEscalationBoundary(s); got != 0 {
			t.Errorf("want 0, got %d", got)
		}
	})
	t.Run("returns index after last escalation turn", func(t *testing.T) {
		s := &Session{History: []Turn{
			mkTurn(RoleUser, AgentEscalation),        // 0
			mkTurn(RoleAssistant, AgentEscalation),   // 1 ← last escalation
			mkTurn(RoleUser, AgentLocal),             // 2
			mkTurn(RoleAssistant, AgentLocal),        // 3
		}}
		if got := LastEscalationBoundary(s); got != 2 {
			t.Errorf("want 2, got %d", got)
		}
	})
}

func TestLocalTurnsSinceLastEscalation(t *testing.T) {
	mkTurn := func(role Role, agent Agent) Turn { return Turn{Role: role, Agent: agent} }

	t.Run("no escalation history returns 0", func(t *testing.T) {
		s := &Session{History: []Turn{
			mkTurn(RoleUser, AgentLocal),
			mkTurn(RoleAssistant, AgentLocal),
		}}
		if got := s.LocalTurnsSinceLastEscalation(); got != 0 {
			t.Errorf("want 0, got %d", got)
		}
	})

	t.Run("counts local turns after last escalation turn", func(t *testing.T) {
		s := &Session{History: []Turn{
			mkTurn(RoleUser, AgentEscalation),
			mkTurn(RoleAssistant, AgentEscalation), // boundary here
			mkTurn(RoleUser, AgentLocal),
			mkTurn(RoleAssistant, AgentLocal),
			mkTurn(RoleUser, AgentLocal),
			mkTurn(RoleAssistant, AgentLocal),
		}}
		if got := s.LocalTurnsSinceLastEscalation(); got != 2 {
			t.Errorf("want 2, got %d", got)
		}
	})

	t.Run("zero local turns after escalation", func(t *testing.T) {
		s := &Session{History: []Turn{
			mkTurn(RoleUser, AgentEscalation),
			mkTurn(RoleAssistant, AgentEscalation),
		}}
		if got := s.LocalTurnsSinceLastEscalation(); got != 0 {
			t.Errorf("want 0, got %d", got)
		}
	})
}

func TestNeedChangedSinceLastEscalation(t *testing.T) {
	mkTurn := func(role Role, agent Agent) Turn { return Turn{Role: role, Agent: agent} }

	t.Run("no need set returns false", func(t *testing.T) {
		s := &Session{EscalationSessionID: "x"}
		if s.NeedChangedSinceLastEscalation() {
			t.Error("want false when no need set")
		}
	})

	t.Run("no prior escalation session returns false", func(t *testing.T) {
		s := &Session{CurrentNeed: "something", CurrentNeedSetAt: 1}
		if s.NeedChangedSinceLastEscalation() {
			t.Error("want false when no escalation session")
		}
	})

	t.Run("need set after last escalation turn returns true", func(t *testing.T) {
		// History: esc turn at index 1; need set at history pos 3 (CurrentNeedSetAt=4)
		s := &Session{
			EscalationSessionID: "x",
			History: []Turn{
				mkTurn(RoleUser, AgentEscalation),
				mkTurn(RoleAssistant, AgentEscalation),
				mkTurn(RoleUser, AgentLocal),
				mkTurn(RoleAssistant, AgentLocal),
			},
			CurrentNeed:      "new goal",
			CurrentNeedSetAt: 4, // set at position 3 (0-based), i.e. after esc turn at index 1
		}
		if !s.NeedChangedSinceLastEscalation() {
			t.Error("want true when need set after last escalation turn")
		}
	})

	t.Run("need set before last escalation turn returns false", func(t *testing.T) {
		// History: need set before esc turn; esc turn follows
		s := &Session{
			EscalationSessionID: "x",
			History: []Turn{
				mkTurn(RoleUser, AgentLocal),
				mkTurn(RoleAssistant, AgentLocal), // need set here: CurrentNeedSetAt=2
				mkTurn(RoleUser, AgentEscalation),
				mkTurn(RoleAssistant, AgentEscalation),
			},
			CurrentNeed:      "old goal",
			CurrentNeedSetAt: 2,
		}
		if s.NeedChangedSinceLastEscalation() {
			t.Error("want false when need set before last escalation turn")
		}
	})
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
