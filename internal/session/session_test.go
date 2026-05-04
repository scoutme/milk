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
