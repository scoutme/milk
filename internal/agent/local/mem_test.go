package local

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// --- capMemToolResult ---

func TestCapMemToolResult_NoLimit(t *testing.T) {
	result := `{"output":"fact1\nfact2\nfact3"}`
	got := capMemToolResult(result, 0)
	if got != result {
		t.Errorf("expected unchanged with limit 0, got %q", got)
	}
}

func TestCapMemToolResult_WithinLimit(t *testing.T) {
	result := `{"output":"short"}`
	got := capMemToolResult(result, 1000)
	if got != result {
		t.Errorf("expected unchanged when within limit, got %q", got)
	}
}

func TestCapMemToolResult_Truncated(t *testing.T) {
	long := strings.Repeat("x", 3000)
	result := `{"output":"` + long + `"}`
	got := capMemToolResult(result, 100)
	if len(got) == len(result) {
		t.Error("expected result to be truncated")
	}
	if !strings.Contains(got, "truncated") {
		t.Error("expected truncation notice in output")
	}
}

func TestCapMemToolResult_InvalidJSON(t *testing.T) {
	result := "not json"
	got := capMemToolResult(result, 100)
	if got != result {
		t.Errorf("expected unchanged on invalid JSON, got %q", got)
	}
}

func TestCapMemToolResult_EmptyOutput(t *testing.T) {
	result := `{"error":"not found"}`
	got := capMemToolResult(result, 10)
	if got != result {
		t.Errorf("expected unchanged when output field is empty, got %q", got)
	}
}

// --- isMemoryReadTool ---

func TestIsMemoryReadTool(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"get_memory", true},
		{"list_memory", true},
		{"record_memory", false},
		{"forget_memory", false},
		{"bash", false},
		{"escalate", false},
	}
	for _, tc := range cases {
		got := isMemoryReadTool(tc.name)
		if got != tc.want {
			t.Errorf("isMemoryReadTool(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// --- shouldInjectMemoryInstruction ---

func makeAgentWithMemCfg(turnThreshold, byteThreshold int) *Agent {
	return &Agent{
		tagNonce: "test",
		memCfg: MemConfig{
			ReinjectionTurns: turnThreshold,
			ReinjectionBytes: byteThreshold,
		},
	}
}

func TestShouldInjectMemoryInstruction_FirstTime(t *testing.T) {
	a := makeAgentWithMemCfg(20, 0)
	sess := &session.Session{}
	if !a.shouldInjectMemoryInstruction(sess) {
		t.Error("expected injection on first turn (injectedAt == 0)")
	}
}

func TestShouldInjectMemoryInstruction_NilSession(t *testing.T) {
	a := makeAgentWithMemCfg(20, 0)
	if !a.shouldInjectMemoryInstruction(nil) {
		t.Error("expected injection when session is nil")
	}
}

func TestShouldInjectMemoryInstruction_BothThresholdsDisabled(t *testing.T) {
	a := makeAgentWithMemCfg(0, 0)
	// injectedAt=1 (1-based): injected at turn-0, non-zero sentinel → already injected
	sess := &session.Session{LocalMemoryInstructionInjectedAt: 1}
	if a.shouldInjectMemoryInstruction(sess) {
		t.Error("expected no injection when both thresholds disabled and already injected")
	}
}

func TestShouldInjectMemoryInstruction_TurnThresholdMet(t *testing.T) {
	a := makeAgentWithMemCfg(3, 0)
	// injectedAt=2 (1-based): injected when LocalTurnCount()=1.
	// turnsSince = LocalTurnCount() - (injectedAt-1) = count - 1.
	sess := &session.Session{LocalMemoryInstructionInjectedAt: 2}
	// Add 3 local turns: turnsSince = 3-1 = 2 — not yet
	for range 3 {
		sess.History = append(sess.History, session.Turn{
			Role:  session.RoleAssistant,
			Agent: session.AgentLocal,
		})
	}
	// LocalTurnCount = 3, turnsSince = 3-1 = 2 — not yet
	if a.shouldInjectMemoryInstruction(sess) {
		t.Error("threshold not yet met (turnsSince=2, threshold=3)")
	}
	// Add one more turn: turnsSince = 4-1 = 3 ≥ 3 → inject
	sess.History = append(sess.History, session.Turn{
		Role:  session.RoleAssistant,
		Agent: session.AgentLocal,
	})
	if !a.shouldInjectMemoryInstruction(sess) {
		t.Error("expected injection when turn threshold met (turnsSince=3)")
	}
}

func TestShouldInjectMemoryInstruction_ByteThresholdMet(t *testing.T) {
	a := makeAgentWithMemCfg(0, 100)
	// injectedAt=2 (1-based): injected when LocalTurnCount()=1.
	// LocalOutputBytesSince(injectedAt-1 = 1) skips turn[0] (the seed).
	sess := &session.Session{LocalMemoryInstructionInjectedAt: 2}
	// Add 1 local turn before injection point (the seed, skipped by bytesSince)
	sess.History = append(sess.History, session.Turn{
		Role: session.RoleAssistant, Agent: session.AgentLocal,
		Content: "seed",
	})
	// Turns after injection point accumulate toward the threshold
	sess.History = append(sess.History, session.Turn{
		Role: session.RoleAssistant, Agent: session.AgentLocal,
		Content: strings.Repeat("a", 50),
	})
	// bytesSince = 50 < 100 → no inject
	if a.shouldInjectMemoryInstruction(sess) {
		t.Error("byte threshold not yet met (50 < 100)")
	}
	sess.History = append(sess.History, session.Turn{
		Role: session.RoleAssistant, Agent: session.AgentLocal,
		Content: strings.Repeat("b", 60),
	})
	// bytesSince = 50+60 = 110 ≥ 100 → inject
	if !a.shouldInjectMemoryInstruction(sess) {
		t.Error("expected injection when byte threshold met (110 ≥ 100)")
	}
}
