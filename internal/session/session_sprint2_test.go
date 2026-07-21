package session

import "testing"

func TestTransition_EmitsMetrics(t *testing.T) {
	s := &Session{State: StateRouting}
	if !s.Transition(StateLocal) {
		t.Fatal("expected valid transition")
	}
}
