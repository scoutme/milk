package main

import (
	"fmt"
	"strings"
	"testing"

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

func TestBuildAgentHistory_NeverReplaysAnotherAgentsTrail(t *testing.T) {
	sess := &session.Session{History: trailTurns(session.AgentLocal, "someone-else", 1)}
	msgs := buildAgentHistory(sess, 0, session.AgentLocal, "mimo", false)
	for _, m := range msgs {
		if m.Role == "tool" || len(m.ToolCalls) > 0 {
			t.Fatalf("another agent's tool calls must not be replayed: %+v", m)
		}
		if strings.Contains(m.Content, "Tool activity") {
			t.Fatalf("another agent's manifest must not leak: %q", m.Content)
		}
	}
}
