package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// TestPinnedEscalationHistoryParity pins the parity contract for sessions that
// start pinned to the escalation agent: the history the escalation agent gets
// must render exactly like the primary path renders its own conversation —
// same message count, same roles, same first-person replay and labels modulo
// agent names. No hand-off narration, no summary bricks, no external-document
// shape (ADR-0093 follow-up).
func TestPinnedEscalationHistoryParity(t *testing.T) {
	mk := func(agent session.Agent, name string) *session.Session {
		s := &session.Session{CWD: "/tmp"}
		s.History = append(s.History,
			session.Turn{Role: session.RoleUser, Agent: agent, AgentName: name, Content: "design a retry mechanism"},
			session.Turn{Role: session.RoleAssistant, Agent: agent, AgentName: name, Content: "Here is the design: ..."},
			session.Turn{Role: session.RoleUser, Agent: agent, AgentName: name, Content: "list the files"},
			session.Turn{Role: session.RoleAssistant, Agent: agent, AgentName: name, Content: "a.go\nb.go"},
		)
		return s
	}

	escMsgs := escalationLocalHistory(mk(session.AgentEscalation, "esc-agent"), "esc-agent", false)
	priMsgs := sessionToUnifiedMessages(mk(session.AgentLocal, "pri-agent"), "pri-agent", false)

	if len(escMsgs) != len(priMsgs) {
		t.Fatalf("message count differs: esc=%d pri=%d", len(escMsgs), len(priMsgs))
	}
	for i := range escMsgs {
		if escMsgs[i].Role != priMsgs[i].Role {
			t.Errorf("msg %d role differs: esc=%s pri=%s", i, escMsgs[i].Role, priMsgs[i].Role)
		}
		e := strings.ReplaceAll(escMsgs[i].Content, "esc-agent", "SELF")
		p := strings.ReplaceAll(priMsgs[i].Content, "pri-agent", "SELF")
		if e != p {
			t.Errorf("msg %d content differs:\n esc=%q\n pri=%q", i, e, p)
		}
	}
}
