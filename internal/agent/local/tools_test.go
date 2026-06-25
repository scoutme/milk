package local

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
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
	result, escalate := dispatchTool(context.Background(), "bash", `{"command":"echo hello"}`, nil, nil, "")
	if escalate {
		t.Fatal("unexpected escalation signal")
	}
	if !strings.Contains(result, "hello") {
		t.Errorf("expected 'hello' in output, got %q", result)
	}
}

func TestRunBash_NonZeroExit(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "bash", `{"command":"exit 42"}`, nil, nil, "")
	if !strings.Contains(result, "42") {
		t.Errorf("expected exit code 42 in result, got %q", result)
	}
}

func TestRunBash_InvalidJSON(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "bash", `not json`, nil, nil, "")
	if !strings.Contains(result, "invalid arguments") {
		t.Errorf("expected error message, got %q", result)
	}
}

func TestRunGrep_FindsMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("hello world\ngoodbye world\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "grep", `{"pattern":"hello","path":"`+f+`"}`, nil, nil, "")
	if !strings.Contains(result, "hello") {
		t.Errorf("expected match in output, got %q", result)
	}
}

func TestRunGrep_Recursive(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "sub")
	os.MkdirAll(sub, 0o700)
	os.WriteFile(filepath.Join(sub, "a.txt"), []byte("needle\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "grep", `{"pattern":"needle","path":"`+dir+`","recursive":true}`, nil, nil, "")
	if !strings.Contains(result, "needle") {
		t.Errorf("expected recursive match, got %q", result)
	}
}

func TestRunGrep_NoMatch(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "test.txt")
	os.WriteFile(f, []byte("nothing here\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "grep", `{"pattern":"xyzzy","path":"`+f+`"}`, nil, nil, "")
	// grep exit code 1 = no match; should get a result, not an error from dispatchTool
	if strings.Contains(result, "invalid") {
		t.Errorf("unexpected error for no-match grep: %q", result)
	}
}

func TestReadFile_ReturnsNumberedLines(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "sample.txt")
	os.WriteFile(f, []byte("line1\nline2\nline3\n"), 0o600)

	result, _ := dispatchTool(context.Background(), "read_file", `{"path":"`+f+`"}`, nil, nil, "")
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
	result, _ := dispatchTool(context.Background(), "read_file", `{"path":"`+f+`","offset":1,"limit":2}`, nil, nil, "")
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
	result, _ := dispatchTool(context.Background(), "read_file", `{"path":"/nonexistent/file.txt"}`, nil, nil, "")
	if !strings.Contains(result, "error") && !strings.Contains(result, "no such file") {
		t.Errorf("expected error for missing file, got %q", result)
	}
}

func TestEscalateReturnsSignal(t *testing.T) {
	_, escalate := dispatchTool(context.Background(), "escalate", `{"reason":"too complex"}`, nil, nil, "")
	if !escalate {
		t.Error("expected escalation signal")
	}
}

func TestGetSessionContext_Empty(t *testing.T) {
	result, escalate := dispatchTool(context.Background(), "get_session_context", `{}`, nil, nil, "")
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

	result, _ := dispatchTool(context.Background(), "get_session_context", `{}`, sess, nil, "")
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

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"last_n":1}`, sess, nil, "")
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

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"pattern":"needle"}`, sess, nil, "")
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
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentEscalation, Content: "claude answer"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"agent":"escalation"}`, sess, nil, "")
	if !strings.Contains(result, "claude answer") {
		t.Errorf("expected escalation turn, got %q", result)
	}
	if strings.Contains(result, "local answer") {
		t.Error("local turn should be excluded when agent=escalation")
	}
}

func TestGetSessionContext_NoMatch(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Content: "something"})

	result, _ := dispatchTool(context.Background(), "get_session_context", `{"pattern":"xyzzy"}`, sess, nil, "")
	if !strings.Contains(result, "no matching turns") {
		t.Errorf("expected no-match message, got %q", result)
	}
}

// makeSess builds a session with n user+assistant turn pairs.
func makeSess(n int) *session.Session {
	sess := &session.Session{}
	for i := 1; i <= n; i++ {
		sess.AddTurn(session.Turn{Role: session.RoleUser, Content: strings.Repeat("u", i)})
		sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: strings.Repeat("a", i)})
	}
	return sess
}

func TestGetSessionContext_CompactOlderHasIndices(t *testing.T) {
	// 8 pairs = 16 turns; split = 16-10 = 6 older turns → compact with indices
	sess := makeSess(8)
	result, _ := dispatchTool(context.Background(), "get_session_context", `{}`, sess, nil, "")
	if !strings.Contains(result, "[1]") {
		t.Errorf("expected compact index [1] in output, got %q", result)
	}
	if !strings.Contains(result, "older history") {
		t.Errorf("expected older history header, got %q", result)
	}
}

func TestGetSessionContext_SmallOlderVerbatimNoHeader(t *testing.T) {
	// 6 pairs = 12 turns; split = 12-10 = 2 older turns → ≤5, verbatim, no header
	sess := makeSess(6)
	result, _ := dispatchTool(context.Background(), "get_session_context", `{}`, sess, nil, "")
	if strings.Contains(result, "older history") {
		t.Errorf("small older portion should be verbatim without header, got %q", result)
	}
}

func TestGetSessionContext_RangeVerbatim(t *testing.T) {
	// 8 pairs → 16 turns; request turns 2-3 verbatim
	sess := makeSess(8)
	result, _ := dispatchTool(context.Background(), "get_session_context", `{"turn_from":2,"turn_to":3}`, sess, nil, "")
	if !strings.Contains(result, "turns 2") {
		t.Errorf("expected verbatim range header, got %q", result)
	}
	// turn 2 is the first assistant turn: content "a" (i=1 in makeSess)
	if !strings.Contains(result, "primary: a") {
		t.Errorf("expected verbatim content of turn 2, got %q", result)
	}
}

func TestGetSessionContext_RangeClampedToMax(t *testing.T) {
	// request 10 turns — should be clamped to contextRangeMaxTurns (5)
	sess := makeSess(10)
	result, _ := dispatchTool(context.Background(), "get_session_context", `{"turn_from":1,"turn_to":10}`, sess, nil, "")
	if !strings.Contains(result, "turns 1") {
		t.Errorf("expected range header, got %q", result)
	}
	// turn 6 (content "aaaaaa") must not appear
	if strings.Contains(result, "aaaaaa") {
		t.Errorf("range should be clamped to %d turns, but turn 6 appears: %q", contextRangeMaxTurns, result)
	}
}

func TestGetSessionContext_RangeOutOfBounds(t *testing.T) {
	sess := makeSess(2) // 4 turns
	result, _ := dispatchTool(context.Background(), "get_session_context", `{"turn_from":99}`, sess, nil, "")
	if !strings.Contains(result, "out of range") {
		t.Errorf("expected out-of-range message, got %q", result)
	}
}

func TestGetSessionContext_RangeDefaultsToFromWhenToOmitted(t *testing.T) {
	sess := makeSess(8)
	result, _ := dispatchTool(context.Background(), "get_session_context", `{"turn_from":2}`, sess, nil, "")
	// header should say "turns 2–2"
	if !strings.Contains(result, "2") {
		t.Errorf("expected single-turn range, got %q", result)
	}
}

func TestUnknownTool(t *testing.T) {
	result, escalate := dispatchTool(context.Background(), "nonexistent", `{}`, nil, nil, "")
	if escalate {
		t.Error("unexpected escalation signal")
	}
	if !strings.Contains(result, "unknown tool") {
		t.Errorf("expected unknown tool error, got %q", result)
	}
}

func TestEditFile_ReplaceAll(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	os.WriteFile(f, []byte("foo bar foo"), 0o600)

	args := `{"path":"` + f + `","old_string":"foo","new_string":"baz","replace_all":true}`
	result, _ := dispatchTool(context.Background(), "edit_file", args, nil, nil, "")
	if strings.Contains(result, "error") {
		t.Fatalf("unexpected error: %q", result)
	}
	got, _ := os.ReadFile(f)
	if string(got) != "baz bar baz" {
		t.Errorf("expected all occurrences replaced, got %q", string(got))
	}
}

func TestEditFile_AmbiguousWithoutReplaceAll(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "f.txt")
	os.WriteFile(f, []byte("foo foo"), 0o600)

	args := `{"path":"` + f + `","old_string":"foo","new_string":"baz"}`
	result, _ := dispatchTool(context.Background(), "edit_file", args, nil, nil, "")
	if !strings.Contains(result, "ambiguous") {
		t.Errorf("expected ambiguous error, got %q", result)
	}
	if !strings.Contains(result, "replace_all") {
		t.Errorf("expected hint about replace_all, got %q", result)
	}
}

func TestDeleteFile(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "todelete.txt")
	os.WriteFile(f, []byte("bye"), 0o600)

	args := `{"path":"` + f + `"}`
	result, _ := dispatchTool(context.Background(), "delete_file", args, nil, nil, "")
	if strings.Contains(result, "error") {
		t.Fatalf("unexpected error: %q", result)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Error("file should have been deleted")
	}
}

func TestDeleteFile_Missing(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "delete_file", `{"path":"/nonexistent/file.txt"}`, nil, nil, "")
	if !strings.Contains(result, "error") && !strings.Contains(result, "no such file") {
		t.Errorf("expected error for missing file, got %q", result)
	}
}

func TestMoveFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.txt")
	dst := filepath.Join(dir, "sub", "dst.txt")
	os.WriteFile(src, []byte("content"), 0o600)

	args := `{"source":"` + src + `","destination":"` + dst + `"}`
	result, _ := dispatchTool(context.Background(), "move_file", args, nil, nil, "")
	if strings.Contains(result, "error") {
		t.Fatalf("unexpected error: %q", result)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Error("source file should be gone after move")
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("destination not found: %v", err)
	}
	if string(got) != "content" {
		t.Errorf("expected content preserved, got %q", string(got))
	}
}

func TestGetContextStats(t *testing.T) {
	sess := &session.Session{}
	sess.AddTurn(session.Turn{Role: session.RoleUser, Agent: session.AgentLocal, Content: "hello"})
	sess.AddTurn(session.Turn{Role: session.RoleAssistant, Agent: session.AgentLocal, Content: "world"})

	result, _ := dispatchTool(context.Background(), "get_context_stats", `{}`, sess, nil, "")
	if !strings.Contains(result, "local_turns=1") {
		t.Errorf("expected local_turns=1, got %q", result)
	}
	if !strings.Contains(result, "total_history_turns=2") {
		t.Errorf("expected total_history_turns=2, got %q", result)
	}
	if !strings.Contains(result, "total_history_chars=10") {
		t.Errorf("expected total_history_chars=10 (hello+world), got %q", result)
	}
}

func TestGetContextStats_NoSession(t *testing.T) {
	result, _ := dispatchTool(context.Background(), "get_context_stats", `{}`, nil, nil, "")
	if !strings.Contains(result, "error") {
		t.Errorf("expected error with nil session, got %q", result)
	}
}

// --- AgentToolSchemas tests ---

func TestAgentToolSchemas_Empty(t *testing.T) {
	result := AgentToolSchemas(nil)
	if result == nil {
		t.Error("expected non-nil empty slice")
	}
	if len(result) != 0 {
		t.Errorf("expected empty slice, got %d entries", len(result))
	}
}

func TestAgentToolSchemas_SingleEntry(t *testing.T) {
	entries := []config.AgentToolEntry{
		{Agent: "my-agent", Description: "A helpful agent"},
	}
	result := AgentToolSchemas(entries)
	if len(result) != 1 {
		t.Fatalf("expected 1 schema, got %d", len(result))
	}
	schema := result[0]
	if schema["type"] != "function" {
		t.Errorf("expected type=function, got %v", schema["type"])
	}
	fn, ok := schema["function"].(map[string]any)
	if !ok {
		t.Fatalf("expected function map, got %T", schema["function"])
	}
	if fn["name"] != "agent_my_agent" {
		t.Errorf("expected agent_my_agent, got %v", fn["name"])
	}
	if fn["description"] != "A helpful agent" {
		t.Errorf("expected description, got %v", fn["description"])
	}
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("expected parameters map, got %T", fn["parameters"])
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("expected properties map, got %T", params["properties"])
	}
	if _, hasRequest := props["request"]; !hasRequest {
		t.Error("expected 'request' property in schema")
	}
	required, _ := params["required"].([]string)
	if len(required) != 1 || required[0] != "request" {
		t.Errorf("expected required=[request], got %v", required)
	}
}

func TestSanitiseAgentToolName_Uppercase(t *testing.T) {
	got := sanitiseAgentToolName("MyAgent")
	if got != "agent_myagent" {
		t.Errorf("expected agent_myagent, got %q", got)
	}
}

func TestSanitiseAgentToolName_Hyphens(t *testing.T) {
	got := sanitiseAgentToolName("my-agent")
	if got != "agent_my_agent" {
		t.Errorf("expected agent_my_agent, got %q", got)
	}
}

func TestSanitiseAgentToolName_Spaces(t *testing.T) {
	got := sanitiseAgentToolName("my agent")
	if got != "agent_my_agent" {
		t.Errorf("expected agent_my_agent, got %q", got)
	}
}
