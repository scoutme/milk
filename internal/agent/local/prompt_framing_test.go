package local

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// TestBuildSystemPromptOpts_EscalationOwnedFromStart: when the session started
// pinned to the escalation agent there is no hand-off to narrate — the prompt
// must not claim "the primary agent has handed off this task" or point the
// model at the primary's work. That false hand-off framing is what makes the
// model present its own earlier turns as received material to recap.
func TestBuildSystemPromptOpts_EscalationOwnedFromStart(t *testing.T) {
	owned := buildSystemPromptOpts("", "mimo-pro", "mimo-pro", false, "standard", false, 0, true)
	for _, banned := range []string{
		"has handed off",
		"shared session history",
		"Prefer agent: \"primary\" to see what the primary agent did",
		"check whether the primary agent performed it",
	} {
		if strings.Contains(owned, banned) {
			t.Errorf("owned framing must not contain %q", banned)
		}
	}
	for _, want := range []string{"yours from the start", "continue as the owner", "don't recap unless asked"} {
		if !strings.Contains(owned, want) {
			t.Errorf("owned framing missing %q", want)
		}
	}

	// The hand-off variant keeps its narrative (regression guard).
	handoff := buildSystemPromptOpts("", "mimo-pro", "mimo-pro", false, "standard", false, 0, false)
	if !strings.Contains(handoff, "The primary agent has handed off this task") {
		t.Error("hand-off framing must keep the hand-off narrative")
	}
	if !strings.Contains(handoff, "check whether the primary agent performed it") {
		t.Error("hand-off framing must keep the primary-attribution guidance")
	}
}

// TestBuildSystemPromptOpts_EscalationOwnedMinimal: the minimal tier drops the
// hand-off claim too, and keeps the role framing.
func TestBuildSystemPromptOpts_EscalationOwnedMinimal(t *testing.T) {
	owned := buildSystemPromptOpts("", "mimo-pro", "mimo-pro", false, "minimal", false, 0, true)
	if strings.Contains(owned, "has handed off") {
		t.Error("owned minimal must not claim a hand-off")
	}
	if !strings.Contains(owned, "escalation agent") {
		t.Error("owned minimal must keep the role framing")
	}
	if !strings.Contains(owned, "yours from the start") {
		t.Error("owned minimal must state ownership of the conversation")
	}
}

// TestBuildSystemPromptOpts_OwnedIgnoredForPrimaryRole: the flag only shapes
// the escalation branch; the primary prompt is unchanged by it.
func TestBuildSystemPromptOpts_OwnedIgnoredForPrimaryRole(t *testing.T) {
	a := buildSystemPromptOpts("", "mimo-local", "", false, "standard", false, 0, true)
	b := buildSystemPromptOpts("", "mimo-local", "", false, "standard", false, 0, false)
	if a != b {
		t.Error("primary-role prompt must not vary with ownedFromStart")
	}
}

func TestOwnsConversationFromStart(t *testing.T) {
	escTurn := session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, AgentName: "mimo-pro"}
	priTurn := session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, AgentName: "mimo-local"}
	mk := func(turns ...session.Turn) *session.Session { return &session.Session{History: turns} }

	esc := &Agent{selfName: "mimo-pro", escalationName: "mimo-pro"}

	if !esc.ownsConversationFromStart(mk(escTurn, escTurn)) {
		t.Error("escalation-only session is owned from start")
	}
	if esc.ownsConversationFromStart(mk(escTurn, priTurn)) {
		t.Error("session with primary work must keep hand-off framing")
	}
	if esc.ownsConversationFromStart(&session.Session{History: []session.Turn{escTurn}, EscalationBrief: "needs vision"}) {
		t.Error("a hand-off brief must keep hand-off framing")
	}
	if esc.ownsConversationFromStart(nil) {
		t.Error("nil session defaults to hand-off framing")
	}
	if (&Agent{selfName: "mimo-local"}).ownsConversationFromStart(mk(escTurn)) {
		t.Error("primary role must never use the owned escalation framing")
	}
}
