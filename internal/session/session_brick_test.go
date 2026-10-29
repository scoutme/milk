package session

import (
	"strings"
	"testing"
)

// TestRenderTurns_ResultTruncationIsExplicit: a tool result over the per-result
// cap is truncated with a pointer to where the full result lives — never a
// silent cut that turns evidence into a claim.
func TestRenderTurns_ResultTruncationIsExplicit(t *testing.T) {
	long := strings.Repeat("x", brickResultMaxChars+100)
	out := renderTurns([]Turn{
		{Role: RoleAssistant, Agent: AgentLocal, ToolCalls: []ToolCall{{Name: "bash", Arguments: `{"command":"test"}`}}},
		{Role: RoleToolResult, Content: long},
	})
	if !strings.Contains(out, "truncated — full result in the session history") {
		t.Errorf("truncation must carry an explicit pointer, got %q", out)
	}
	if strings.Contains(out, "... (truncated)") && !strings.Contains(out, "full result") {
		t.Errorf("silent truncation marker must be gone, got %q", out)
	}
}

// TestRenderTurns_TrailEvidenceRendered: trail-era turns (tool calls and
// results stored on the assistant turn) carry their evidence in the brick,
// not just the final text.
func TestRenderTurns_TrailEvidenceRendered(t *testing.T) {
	out := renderTurns([]Turn{
		{Role: RoleUser, Agent: AgentLocal, Content: "check the tests"},
		{Role: RoleAssistant, Agent: AgentLocal, Content: "all green",
			Trail: []TrailStep{{Calls: []TrailCall{
				{ID: "c1", Name: "bash", Args: `{"command":"go test ./..."}`, Result: "ok 42/42"},
			}}}},
	})
	for _, want := range []string{"[Tool: bash]", "go test ./...", "[Tool result] ok 42/42", "Assistant (primary): all green"} {
		if !strings.Contains(out, want) {
			t.Errorf("brick should render %q, got %q", want, out)
		}
	}
}

// TestRenderTurns_TrailClearedResultMarker: a call whose result was dropped at
// capture says so instead of pretending to carry it.
func TestRenderTurns_TrailClearedResultMarker(t *testing.T) {
	out := renderTurns([]Turn{
		{Role: RoleAssistant, Agent: AgentLocal, Content: "done",
			Trail: []TrailStep{{Calls: []TrailCall{
				{ID: "c1", Name: "grep", Args: `{"pattern":"foo"}`, Cleared: true},
			}}}},
	})
	if !strings.Contains(out, "(cleared — full result in the session history)") {
		t.Errorf("cleared result must carry an explicit marker, got %q", out)
	}
}

// TestRebuildSummaryBricks_BudgetTrimIsExplicit: when the brick drops older
// turns it says how many — a summary must never silently look complete.
func TestRebuildSummaryBricks_BudgetTrimIsExplicit(t *testing.T) {
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "turn one"),
		mkTurn(RoleAssistant, AgentLocal, "answer one"),
		mkTurn(RoleUser, AgentLocal, "turn two"),
		mkTurn(RoleAssistant, AgentLocal, "answer two"),
		mkTurn(RoleUser, AgentLocal, "turn three"),
		mkTurn(RoleAssistant, AgentLocal, "answer three"),
	}}
	s.RebuildSummaryBricks(60)
	if !strings.Contains(s.LastLocalSummary, "earlier activity omitted") {
		t.Errorf("trimmed brick must carry an omission marker, got %q", s.LastLocalSummary)
	}
	if strings.Contains(s.LastLocalSummary, "turn one") {
		t.Errorf("oldest turn should be trimmed under budget, got %q", s.LastLocalSummary)
	}
}

// TestRebuildSummaryBricks_SingleTurnOverBudgetKeepsHeadTail: when even the
// newest turn alone exceeds the budget, a head+tail of it survives (via
// textbudget) instead of the brick going empty.
func TestRebuildSummaryBricks_SingleTurnOverBudgetKeepsHeadTail(t *testing.T) {
	long := strings.Repeat("a", 4000)
	s := &Session{History: []Turn{
		mkTurn(RoleUser, AgentLocal, "q"),
		mkTurn(RoleAssistant, AgentLocal, long),
	}}
	s.RebuildSummaryBricks(1000)
	if s.LastLocalSummary == "" {
		t.Fatal("brick must not go empty when a single turn is over budget")
	}
	if !strings.Contains(s.LastLocalSummary, "chars omitted") {
		t.Errorf("over-budget turn should be head+tail truncated with a marker, got %q", s.LastLocalSummary[:60])
	}
}
