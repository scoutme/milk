package escalation

import (
	"strings"
	"testing"
	"time"

	"github.com/scoutme/milk/internal/session"
)

func turn(role session.Role, agent session.Agent, content string) session.Turn {
	return session.Turn{Role: role, Agent: agent, Content: content, Timestamp: time.Now()}
}

func TestBuildContext_Empty(t *testing.T) {
	sess := &session.Session{}
	if got := BuildContext(sess); got != "" {
		t.Errorf("empty history should return empty string, got %q", got)
	}
}

func TestBuildContext_ContainsHeader(t *testing.T) {
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleUser, session.AgentLocal, "hello"),
	}}
	got := BuildContext(sess)
	if !strings.Contains(got, "[Context from local agent session]") {
		t.Error("missing header")
	}
	if !strings.Contains(got, "[End of local context") {
		t.Error("missing footer")
	}
}

func TestBuildContext_UserAndAssistantTurns(t *testing.T) {
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleUser, session.AgentLocal, "what files are here?"),
		turn(session.RoleAssistant, session.AgentLocal, "I can check that."),
		turn(session.RoleToolResult, "", `{"output":"main.go\n","exit_code":0}`),
	}}
	got := BuildContext(sess)
	if !strings.Contains(got, "User: what files are here?") {
		t.Error("missing user turn")
	}
	if !strings.Contains(got, "Assistant (local): I can check that.") {
		t.Error("missing assistant turn")
	}
	if !strings.Contains(got, "[Tool result]") {
		t.Error("missing tool result")
	}
}

func TestBuildContext_ToolResultTruncation(t *testing.T) {
	long := strings.Repeat("x", 600)
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleToolResult, "", long),
	}}
	got := BuildContext(sess)
	if !strings.Contains(got, "... (truncated)") {
		t.Error("expected truncation marker")
	}
	if strings.Contains(got, long) {
		t.Error("expected content to be truncated, found full string")
	}
}

func TestBuildContext_ToolResultNoTruncation(t *testing.T) {
	short := strings.Repeat("x", 499)
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleToolResult, "", short),
	}}
	got := BuildContext(sess)
	if strings.Contains(got, "... (truncated)") {
		t.Error("short content should not be truncated")
	}
}
