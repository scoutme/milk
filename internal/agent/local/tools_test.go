package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

func TestChatRequest_NilHistorySerializesAsArray(t *testing.T) {
	// llama.cpp rejects {"messages":null}; must be {"messages":[...]}
	req := chatRequest{
		Model:    "test",
		Messages: []Message{{Role: "user", Content: "hi"}},
	}
	b, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), `"messages":null`) {
		t.Error("messages must not serialize as null")
	}
	if !strings.Contains(string(b), `"messages":[`) {
		t.Errorf("messages must serialize as array, got: %s", b)
	}
}

func TestRunBash_Success(t *testing.T) {
	result, escalate := dispatchTool(context.Background(), "bash", `{"command":"echo hello"}`, nil)
	if escalate {
		t.Fatal("unexpected escalation signal")
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected 'hello' in output, got %q", result)
	}
}

func TestRunBash_NonZeroExit(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "bash", `{"command":"exit 42"}`, nil)
	if !strings.Contains(result, "42") {
		t.Errorf("expected exit code 42 in result, got %q", result)
	}
}

func TestRunBash_InvalidJSON(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "bash", `not json`, nil)
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected error message, got %q", result)
	}
}

func TestRunGrep_FindsMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("hello world\ngoodbye world\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "grep", `{"pattern":"hello","path":"`+f+`"}`, nil)
	if !strings.Contains(result, "hello") {
		t.Errorf("expected match in output, got %q", result)
	}
}

func TestRunGrep_Recursive(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "a.txt"), []byte("needle\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "grep", `{"pattern":"needle","path":"`+dir+`","recursive":true}`, nil)
	if !strings.Contains(result, "needle") {
		t.Errorf("expected recursive match, got %q", result)
	}
}

func TestRunGrep_NoMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("nothing here\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "grep", `{"pattern":"xyzzy","path":"`+f+`"}`, nil)
	// grep exit code 1 = no match; should get a result, not an error from dispatchTool
	if strings.Contains(result, "invalid") {
		t.Errorf("unexpected error for no-match grep: %q", result)
	}
}

func TestReadFile_ReturnsNumberedLines(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sample.txt")
	os.WriteFile(f, []byte("line1\nline2\nline3\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "read_file", `{"path":"`+f+`"}`, nil)
	// result is JSON: {"output":"1\tline1\n..."}
	if !strings.Contains(result, `1\tline1`) {
		t.Errorf("expected numbered lines, got %q", result)
	}
	if !strings.Contains(result, `3\tline3`) {
		t.Errorf("expected line 3, got %q", result)
	}
}

func TestReadFile_OffsetAndLimit(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sample.txt")
	os.WriteFile(f, []byte("a\nb\nc\nd\ne\n"), 0o600)

	// offset=1 skips line index 0 ("a"); limit=2 returns lines at index 1,2 ("b","c")
	// line numbers are 1-based from offset: index 1 → number 2, index 2 → number 3
	result, _ := dispatchTool(context.Background(), "read_file", `{"path":"`+f+`","offset":1,"limit":2}`, nil)
	if strings.Contains(result, `1\ta`) {
		t.Error("offset=1 should skip first line")
	}
	if !strings.Contains(result, `2\tb`) {
		t.Errorf("expected line b at position 2, got %q", result)
	}
	if strings.Contains(result, `4\td`) {
		t.Error("limit=2 should stop before line d")
	}
}

func TestReadFile_MissingFile(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "read_file", `{"path":"/nonexistent/file.txt"}`, nil)
	if !strings.Contains(result, "error") && !strings.Contains(result, "no such file") {
		t.Errorf("expected error for missing file, got %q", result)
	}
}

func TestEscalateToClaudeReturnsSignal(t *testing.T) {
	_, escalate := dispatchTool(context.Background(), "escalate_to_claude", `{"reason":"too complex"}`, nil)
	if !escalate {
		t.Error("expected escalation signal")
	}
}

func TestGetSessionContext_Empty(t *testing.T) {
	result, escalate := dispatchTool(context.Background(), "get_session_context", `{}`, nil)
	if escalate {
		t.Error("unexpected escalation signal")
	}
	if !strings.Contains(result, "no session history") {
		t.Errorf("expected empty-history message, got %q", result)
	}
}

func TestGetSessionContext_WithHistory(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "hello"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "world"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{}`, sess)
	if !strings.Contains(result, "hello") {
		t.Errorf("expected user turn in context, got %q", result)
	}
	if !strings.Contains(result, "world") {
		t.Errorf("expected assistant turn in context, got %q", result)
	}
}

func TestGetSessionContext_LastN(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "first"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "second"})
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "third"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"last_n":1}`, sess)
	if strings.Contains(result, "first") {
		t.Error("last_n:1 should exclude earlier turns")
	}
	if !strings.Contains(result, "third") {
		t.Errorf("expected last turn in result, got %q", result)
	}
}

func TestGetSessionContext_Pattern(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "needle in a haystack"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "unrelated response"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"pattern":"needle"}`, sess)
	if !strings.Contains(result, "needle") {
		t.Errorf("expected matching turn, got %q", result)
	}
	if strings.Contains(result, "unrelated") {
		t.Error("non-matching turn should be excluded")
	}
}

func TestGetSessionContext_AgentFilter(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "question"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "local answer"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentClaude, Content: "claude answer"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"agent":"claude"}`, sess)
	if !strings.Contains(result, "claude answer") {
		t.Errorf("expected claude turn, got %q", result)
	}
	if strings.Contains(result, "local answer") {
		t.Error("local turn should be excluded when agent=claude")
	}
}

func TestGetSessionContext_NoMatch(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "something"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"pattern":"xyzzy"}`, sess)
	if !strings.Contains(result, "no matching turns") {
		t.Errorf("expected no-match message, got %q", result)
	}
}

func TestUnknownTool(t *testing.T) {
	result, escalate := dispatchTool(context.Background(), "nonexistent", `{}`, nil)
	if escalate {
		t.Error("unexpected escalation signal")
	}
	if !strings.Contains(result, "unknown tool") {
		t.Errorf("expected unknown tool error, got %q", result)
	}
}
