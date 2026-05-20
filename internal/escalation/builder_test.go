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
	got := BuildContext(sess, "testnonce", nil)
	if !strings.Contains(got, "milk:percept:testnonce") {
		t.Errorf("empty history should still include memory instruction with nonce, got %q", got)
	}
}

func TestBuildContext_ContainsHeader(t *testing.T) {
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleUser, session.AgentLocal, "hello"),
	}}
	got := BuildContext(sess, "testnonce", nil)
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
	got := BuildContext(sess, "testnonce", nil)
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
	got := BuildContext(sess, "testnonce", nil)
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
	got := BuildContext(sess, "testnonce", nil)
	if strings.Contains(got, "... (truncated)") {
		t.Error("short content should not be truncated")
	}
}

func TestMemoryInstruction_NonceInTag(t *testing.T) {
	nonce := "abc123"
	got := MemoryInstruction(nonce)
	if !strings.Contains(got, "<milk:percept:abc123>") {
		t.Errorf("MemoryInstruction should contain nonce open tag, got %q", got)
	}
	if !strings.Contains(got, "</milk:percept:abc123>") {
		t.Errorf("MemoryInstruction should contain nonce close tag, got %q", got)
	}
	// Verify plain <milk:percept> (without nonce) does NOT appear
	if strings.Contains(got, "<milk:percept>") {
		t.Errorf("MemoryInstruction must not contain legacy tag without nonce, got %q", got)
	}
}

func TestBuildContext_WithPercepts(t *testing.T) {
	sess := &session.Session{}
	percepts := []string{"user prefers Go", "escalate to claude for architecture"}
	got := BuildContext(sess, "testnonce", percepts)

	if !strings.Contains(got, "[Remembered facts]") {
		t.Error("expected [Remembered facts] header when percepts are provided")
	}
	for _, p := range percepts {
		if !strings.Contains(got, p) {
			t.Errorf("expected percept %q in output, got %q", p, got)
		}
	}
}

func TestBuildContext_NilPerceptsOmitsBlock(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "testnonce", nil)
	if strings.Contains(got, "[Remembered facts]") {
		t.Error("nil percepts should not produce [Remembered facts] block")
	}
}

func TestBuildContext_EmptyPerceptsOmitsBlock(t *testing.T) {
	sess := &session.Session{}
	got := BuildContext(sess, "testnonce", []string{})
	if strings.Contains(got, "[Remembered facts]") {
		t.Error("empty percepts slice should not produce [Remembered facts] block")
	}
}

func TestBuildContext_WithHistoryAndPercepts(t *testing.T) {
	sess := &session.Session{History: []session.Turn{
		turn(session.RoleUser, session.AgentLocal, "what is the plan?"),
	}}
	got := BuildContext(sess, "testnonce", []string{"use flat files"})

	if !strings.Contains(got, "[Context from local agent session]") {
		t.Error("missing context header")
	}
	if !strings.Contains(got, "[Remembered facts]") {
		t.Error("expected [Remembered facts] after history")
	}
	if !strings.Contains(got, "use flat files") {
		t.Error("expected percept content in output")
	}
}
