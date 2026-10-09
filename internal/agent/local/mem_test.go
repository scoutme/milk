package local

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/scoutme/milk/internal/session"
)

// --- capToolResult ---

func TestCapMemToolResult_NoLimit(t *testing.T) {
	result := `{"output":"fact1\nfact2\nfact3"}`
	got := capToolResult(result, 0)
	if got != result {
		t.Errorf("expected unchanged with limit 0, got %q", got)
	}
}

func TestCapMemToolResult_WithinLimit(t *testing.T) {
	result := `{"output":"short"}`
	got := capToolResult(result, 1000)
	if got != result {
		t.Errorf("expected unchanged when within limit, got %q", got)
	}
}

func TestCapMemToolResult_Truncated(t *testing.T) {
	long := strings.Repeat("x", 3000)
	result := `{"output":"` + long + `"}`
	got := capToolResult(result, 100)
	if len(got) == len(result) {
		t.Error("expected result to be truncated")
	}
	if !strings.Contains(got, "omitted") {
		t.Error("expected an omission notice in output")
	}
}

// TestCapMemToolResult_PreservesTail verifies that truncation keeps both the
// head and the tail of a long output — a build/test failure signal near the
// end must survive, unlike the previous head-only truncation.
func TestCapMemToolResult_PreservesTail(t *testing.T) {
	body := "BEGIN-MARKER" + strings.Repeat("filler ", 500) + "FAIL: TestSomething END-MARKER"
	result := `{"output":"` + body + `"}`
	got := capToolResult(result, 200)

	if !strings.Contains(got, "BEGIN-MARKER") {
		t.Error("expected the head marker to survive truncation")
	}
	if !strings.Contains(got, "FAIL: TestSomething END-MARKER") {
		t.Error("expected the tail marker (the actual failure signal) to survive truncation")
	}
}

func TestCapMemToolResult_InvalidJSON(t *testing.T) {
	result := "not json"
	got := capToolResult(result, 100)
	if got != result {
		t.Errorf("expected unchanged on invalid JSON, got %q", got)
	}
}

func TestCapMemToolResult_EmptyOutput(t *testing.T) {
	result := `{"error":"not found"}`
	got := capToolResult(result, 10)
	if got != result {
		t.Errorf("expected unchanged when output field is empty, got %q", got)
	}
}

// TestDispatchOneTool_CapsNonMemoryToolResult verifies that a tool with no
// cap of its own (bash) gets truncated via memCfg.ToolResultMaxBytes, the
// same as memory/session-context tools already were — a long shell output
// alone should not be able to dominate a turn's payload before the
// payload-size trim loop ever gets a chance to run.
func TestDispatchOneTool_CapsNonMemoryToolResult(t *testing.T) {
	a := (&Agent{}).WithMemConfig(MemConfig{ToolResultMaxBytes: 200})
	tc := toolCall{ID: "1", Function: toolCallFunction{
		Name:      "bash",
		Arguments: `{"command":"head -c 1000 /dev/zero | tr '\\0' 'a'"}`,
	}}
	outcome := a.dispatchOneTool(context.Background(), tc, 0, "", "", io.Discard, &session.Session{}, nil, nil)

	var r toolResult
	if err := json.Unmarshal([]byte(outcome.msg.Content), &r); err != nil {
		t.Fatalf("expected valid toolResult JSON, got %q: %v", outcome.msg.Content, err)
	}
	if len(r.Output) > 200 {
		t.Errorf("expected output capped to ~200 bytes, got %d bytes", len(r.Output))
	}
}

// --- truncateHeadAndTail ---

func TestTruncateHeadAndTail_WithinLimit(t *testing.T) {
	s := "short string"
	if got := truncateHeadAndTail(s, 100); got != s {
		t.Errorf("expected unchanged when within limit, got %q", got)
	}
}

func TestTruncateHeadAndTail_RuneSafe(t *testing.T) {
	// Multi-byte runes throughout so any naive byte-index cut would split one.
	s := strings.Repeat("日本語テスト", 200)
	got := truncateHeadAndTail(s, 100)
	if !utf8.ValidString(got) {
		t.Errorf("expected valid UTF-8 output, got invalid string of length %d", len(got))
	}
}

func TestTruncateHeadAndTail_TinyBudget(t *testing.T) {
	s := strings.Repeat("x", 1000)
	// Budget too small to fit head + marker + tail — must not panic and must
	// still return a bounded, valid result.
	got := truncateHeadAndTail(s, 5)
	if len(got) > 5 {
		t.Errorf("expected result to stay within the byte budget, got %d bytes", len(got))
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

func TestCapToolResultHint_TellsHowToGetTheRest(t *testing.T) {
	big := strings.Repeat("line of file text\n", 2000)
	result := toolResult{Output: big}.String()
	got := capToolResultHint(result, 4000, toolResultCutHint("read_file"))
	if !strings.Contains(got, "offset/limit") || !strings.Contains(got, "bytes omitted") {
		t.Errorf("capped read_file result lacks the read-back hint: %.300s", got)
	}
	if plain := capToolResultHint(result, 4000, ""); strings.Contains(plain, "offset/limit") {
		t.Error("no hint expected when none is given")
	}
	// A tiny budget must not be eaten by the hint.
	small := capToolResultHint(result, 200, toolResultCutHint("read_file"))
	var r toolResult
	if err := json.Unmarshal([]byte(small), &r); err != nil || len(r.Output) > 200 {
		t.Errorf("small budget exceeded or invalid: len=%d err=%v", len(r.Output), err)
	}
}
