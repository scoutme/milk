package main

import (
	"fmt"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
)

func trailTurns(agent session.Agent, name string, n int) []session.Turn {
	var out []session.Turn
	for i := 0; i < n; i++ {
		out = append(out,
			session.Turn{Role: session.RoleUser, Agent: agent, AgentName: name, Content: fmt.Sprintf("ask %d", i)},
			session.Turn{Role: session.RoleAssistant, Agent: agent, AgentName: name, Content: fmt.Sprintf("answer %d", i),
				Trail: []session.TrailStep{{Calls: []session.TrailCall{{ID: fmt.Sprintf("c%d", i), Name: "read_file", Args: `{"path":"a.go"}`, Result: "R"}}}}})
	}
	return out
}

func TestBuildAgentHistory_ReplaysOwnToolTrail(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		sess := &session.Session{History: trailTurns(session.AgentLocal, "mimo", 3)}
		msgs := buildAgentHistory(sess, 0, session.AgentLocal, "mimo", lazy)

		var roles []string
		for _, m := range msgs {
			roles = append(roles, m.Role)
		}
		// turn 0 is old: manifest only; turns 1 and 2 expand to
		// user, assistant(tool_calls), tool, assistant.
		want := "user,assistant,user,assistant,tool,assistant,user,assistant,tool,assistant"
		if got := strings.Join(roles, ","); got != want {
			t.Fatalf("lazy=%v roles = %s, want %s", lazy, got, want)
		}
		if !strings.Contains(msgs[1].Content, "answer 0") || !strings.Contains(msgs[1].Content, "read_file ×1") {
			t.Errorf("lazy=%v oldest turn should carry a manifest note, got %q", lazy, msgs[1].Content)
		}
		if msgs[5].Content != "answer 1" {
			t.Errorf("lazy=%v expanded turn keeps its plain final text, got %q", lazy, msgs[5].Content)
		}
	}
}

// TestBuildAgentHistory_NarratesOtherAgentsTrail pins the core parity fix:
// another agent's tool evidence is narrated into the receiving agent's
// history — calls, arguments and results — instead of being dropped, and it
// is never shown as first-person tool calls the receiving agent never issued.
func TestBuildAgentHistory_NarratesOtherAgentsTrail(t *testing.T) {
	sess := &session.Session{History: trailTurns(session.AgentLocal, "someone-else", 1)}
	msgs := buildAgentHistory(sess, 0, session.AgentEscalation, "claude", false)

	var joined string
	for _, m := range msgs {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("another agent's tool calls must not be replayed first-person: %+v", m)
		}
		joined += m.Content + "\n"
	}
	for _, want := range []string{"read_file", `{"path":"a.go"}`, "[result: R]", "answer 0"} {
		if !strings.Contains(joined, want) {
			t.Errorf("narration must carry %q, got:\n%s", want, joined)
		}
	}
}

// TestBuildAgentHistory_NarratesEmptyTextTrailTurn: a tool-using turn whose
// final text is empty still contributes its evidence (it used to be skipped
// wholesale for everyone).
func TestBuildAgentHistory_NarratesEmptyTextTrailTurn(t *testing.T) {
	history := []session.Turn{
		{Role: session.RoleUser, Agent: session.AgentLocal, AgentName: "mimo", Content: "go"},
		{Role: session.RoleAssistant, Agent: session.AgentLocal, AgentName: "mimo",
			Trail: []session.TrailStep{{Calls: []session.TrailCall{{ID: "c1", Name: "bash", Args: `{"command":"make"}`, Result: "done"}}}}},
	}
	sess := &session.Session{History: history}
	for _, self := range []struct {
		agent session.Agent
		name  string
	}{{session.AgentLocal, "mimo"}, {session.AgentEscalation, "claude"}} {
		var joined string
		for _, m := range buildAgentHistory(sess, 0, self.agent, self.name, false) {
			joined += m.Content + "\n"
		}
		if !strings.Contains(joined, "done") {
			t.Errorf("self=%s: empty-text turn's tool result must survive, got:\n%s", self.name, joined)
		}
	}
}

// TestBuildAgentHistory_NarratesOldTurnsAsManifest: retention parity — the
// other agent's oldest tool-using turns shrink to the same one-line manifest
// the agent itself gets, its newest turns stay expanded.
func TestBuildAgentHistory_NarratesOldTurnsAsManifest(t *testing.T) {
	sess := &session.Session{History: trailTurns(session.AgentLocal, "someone-else", 3)}
	msgs := buildAgentHistory(sess, 0, session.AgentEscalation, "claude", false)

	var manifestOnAnswer0, manifestOnAnswer1, manifestOnAnswer2, expanded bool
	var expandCount int
	for _, m := range msgs {
		hasManifest := strings.Contains(m.Content, "tool activity:")
		switch {
		case strings.Contains(m.Content, "answer 0"):
			manifestOnAnswer0 = hasManifest
		case strings.Contains(m.Content, "answer 1"):
			manifestOnAnswer1 = hasManifest
		case strings.Contains(m.Content, "answer 2"):
			manifestOnAnswer2 = hasManifest
		}
		if strings.Contains(m.Content, "[result: R]") {
			expandCount++
		}
	}
	expanded = expandCount == 2
	if !manifestOnAnswer0 {
		t.Errorf("oldest turn should carry a manifest note")
	}
	if manifestOnAnswer1 || manifestOnAnswer2 {
		t.Errorf("newest turns should expand, not manifest")
	}
	if !expanded {
		t.Errorf("expected 2 expanded turns with call/result narration, got %d", expandCount)
	}
}

// TestBuildAgentHistory_LegacyToolResultNarrated: sessions predating trail
// storage keep their evidence too — a legacy tool result of another agent is
// narrated instead of dropped.
func TestBuildAgentHistory_LegacyToolResultNarrated(t *testing.T) {
	sess := &session.Session{History: []session.Turn{
		{Role: session.RoleUser, Agent: session.AgentLocal, AgentName: "mimo", Content: "search"},
		{Role: session.RoleAssistant, Agent: session.AgentLocal, AgentName: "mimo", Content: "looking",
			ToolCalls: []session.ToolCall{{Name: "grep", Arguments: `{"pattern":"foo"}`}}},
		{Role: session.RoleToolResult, Agent: session.AgentLocal, Content: "found foo at 3 sites"},
	}}
	msgs := buildAgentHistory(sess, 0, session.AgentEscalation, "claude", false)

	var joined string
	for _, m := range msgs {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("legacy calls must not be replayed first-person: %+v", m)
		}
		joined += m.Content + "\n"
	}
	for _, want := range []string{"grep", "found foo at 3 sites"} {
		if !strings.Contains(joined, want) {
			t.Errorf("legacy evidence must be narrated, missing %q in:\n%s", want, joined)
		}
	}
}

// mixedSession returns a session where both agents worked with tool trails.
func mixedSession() *session.Session {
	return &session.Session{History: []session.Turn{
		{Role: session.RoleUser, Agent: session.AgentLocal, AgentName: "mimo", Content: "fix the off-by-one in pricing"},
		{Role: session.RoleAssistant, Agent: session.AgentLocal, AgentName: "mimo", Content: "Fixed the boundary check.",
			Trail: []session.TrailStep{{Text: "checking the boundary", Calls: []session.TrailCall{
				{ID: "c1", Name: "read_file", Args: `{"path":"pricing.go"}`, Result: "func Max(a, b int) int {"},
				{ID: "c2", Name: "bash", Args: `{"command":"go test ./..."}`, Result: "ok 3/3"},
			}}}},
		{Role: session.RoleUser, Agent: session.AgentEscalation, AgentName: "claude", Content: "also check the negative case"},
		{Role: session.RoleAssistant, Agent: session.AgentEscalation, AgentName: "claude", Content: "verified n=0",
			Trail: []session.TrailStep{{Calls: []session.TrailCall{
				{ID: "c3", Name: "grep", Args: `{"pattern":"Max"}`, Result: "pricing.go:10"},
			}}}},
	}}
}

func joinedContent(msgs []local.Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Content)
		for _, tc := range m.ToolCalls {
			b.WriteString("\n")
			b.WriteString(tc.Function.Name)
			b.WriteString("(")
			b.WriteString(tc.Function.Arguments)
			b.WriteString(")")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// TestBuildAgentHistory_EscalationRenderingSuperset pins the parity
// requirement: for the same session, everything the primary's rendering shows,
// the escalation agent's rendering contains too — and vice versa — modulo
// speaker labels and the narration-vs-first-person format (own turns replay
// as tool calls, other agents' turns are narrated). What one agent saw, the
// other can see: every text, tool name, argument and result.
func TestBuildAgentHistory_EscalationRenderingSuperset(t *testing.T) {
	sess := mixedSession()
	primary := buildAgentHistory(sess, 0, session.AgentLocal, "mimo", false)
	escalation := buildAgentHistory(sess, 0, session.AgentEscalation, "claude", false)

	primText, escText := joinedContent(primary), joinedContent(escalation)
	evidence := []string{
		"fix the off-by-one in pricing", "also check the negative case", // user texts
		"Fixed the boundary check.", "verified n=0", // final texts
		"checking the boundary", // tool-adjacent narration text
		"read_file", `{"path":"pricing.go"}`, "func Max(a, b int) int {",
		"bash", `{"command":"go test ./..."}`, "ok 3/3",
		"grep", `{"pattern":"Max"}`, "pricing.go:10",
	}
	for _, want := range evidence {
		if !strings.Contains(escText, want) {
			t.Errorf("escalation view misses evidence %q", want)
		}
		if !strings.Contains(primText, want) {
			t.Errorf("primary view misses evidence %q", want)
		}
	}
	// User turns are shared verbatim (labels included) on both sides.
	for _, m := range primary {
		if strings.HasPrefix(m.Content, "[user to ") && !strings.Contains(escText, m.Content) {
			t.Errorf("escalation view misses user turn %q", m.Content)
		}
	}
}
