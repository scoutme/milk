package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

func testTools() []map[string]any {
	return []map[string]any{
		{"type": "function", "function": map[string]any{"name": "bash", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{
				"command": map[string]any{"type": "string"}, "timeout_seconds": map[string]any{"type": "integer"}},
			"required": []string{"command"}}}},
		{"type": "function", "function": map[string]any{"name": "get_session_context", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{
				"agent": map[string]any{"type": "string"}, "last_n": map[string]any{"type": "integer"}, "pattern": map[string]any{"type": "string"}},
			"required": []string{}}}},
		{"type": "function", "function": map[string]any{"name": "search_signals", "parameters": map[string]any{
			"type": "object", "properties": map[string]any{
				"max_results": map[string]any{"type": "integer"}, "pattern": map[string]any{"type": "string"}, "signals": map[string]any{"type": "array"}},
			"required": []string{"pattern"}}}},
	}
}

// Verbatim shape of the 2026-10-08 02:09 stream (values shortened): four calls
// leaked as text, their arguments streamed as nameless fragments.
const (
	leaked0209 = `<tool_call>=get_session_context><parameter=agent>escalation</parameter><parameter=last_n>10</parameter></function></tool_call>` +
		`<tool_call>=bash><parameter=command>cd /repo && git status --short && git log --oneline -3</parameter></function></tool_call>` +
		`<tool_call>=bash><parameter=command>grep -n "handleUpdateCmd\|pendingUpdate" cmd/milk/*.go | grep -v _test</parameter></function></tool_call>`
	orphans0209 = `{"agent": "escalation", "last_n": "10"` +
		`{"command": "cd /repo && git status --short && git log --oneline -3"` +
		`{"command": "grep -n \"handleUpdateCmd\\|pendingUpdate\" cmd/milk/*.go | grep -v _test"`
)

func TestRecoverLeakedToolCalls_RealShape(t *testing.T) {
	got := recoverLeakedToolCalls(testTools(), nil, leaked0209, orphans0209)
	if len(got) != 3 {
		t.Fatalf("want 3 recovered calls, got %d: %+v", len(got), got)
	}
	var a map[string]any
	if err := json.Unmarshal([]byte(got[0].Function.Arguments), &a); err != nil {
		t.Fatal(err)
	}
	if got[0].Function.Name != "get_session_context" || a["agent"] != "escalation" || a["last_n"] != float64(10) {
		t.Errorf("call 0 = %s %v", got[0].Function.Name, a)
	}
	if got[1].Function.Name != "bash" || !strings.Contains(got[1].Function.Arguments, "git log --oneline -3") {
		t.Errorf("call 1 = %+v", got[1])
	}
	if !strings.Contains(got[2].Function.Arguments, `handleUpdateCmd\\|pendingUpdate`) {
		t.Errorf("call 2 lost its escaped backslashes: %s", got[2].Function.Arguments)
	}
	for _, g := range got {
		if g.ID == "" || g.Type != "function" {
			t.Errorf("malformed recovered call: %+v", g)
		}
	}
}

func TestRecoverLeakedToolCalls_Variants(t *testing.T) {
	for name, leaked := range map[string]string{
		"missing-function-prefix": `<tool_call>=bash><parameter=command>ls</parameter></function></tool_call>`,
		"angle-equals":            `<tool_call><=bash><parameter=command>ls</parameter></function></tool_call>`,
		"function-tag-newlines":   "<tool_call>\n<function=bash>\n<parameter=command>\nls\n</parameter>\n</function>\n</tool_call>",
	} {
		got := recoverLeakedToolCalls(testTools(), nil, leaked, `{"command": "ls"`)
		if len(got) != 1 || got[0].Function.Arguments != `{"command":"ls"}` {
			t.Errorf("%s: got %+v", name, got)
		}
	}
}

func TestRecoverLeakedToolCalls_Rejections(t *testing.T) {
	ok := `<tool_call>=bash><parameter=command>ls</parameter></function></tool_call>`
	cases := map[string]struct{ leaked, orphan string }{
		"not corroborated by streamed fragments": {ok, `{"command": "something else"`},
		"no streamed fragments at all":           {ok, ``},
		"unknown tool":                           {`<tool_call>=rm_rf><parameter=command>ls</parameter></function></tool_call>`, `{"command": "ls"`},
		"undeclared parameter":                   {`<tool_call>=bash><parameter=command>ls</parameter><parameter=evil>1</parameter></function></tool_call>`, `{"command": "ls", "evil": 1`},
		"missing required parameter":             {`<tool_call>=bash><parameter=timeout_seconds>5</parameter></function></tool_call>`, `{"timeout_seconds": 5`},
		"integer that is not an integer":         {`<tool_call>=bash><parameter=command>ls</parameter><parameter=timeout_seconds>soon</parameter></function></tool_call>`, `{"command": "ls", "timeout_seconds": "soon"`},
		"duplicate parameter":                    {`<tool_call>=bash><parameter=command>ls</parameter><parameter=command>pwd</parameter></function></tool_call>`, `{"command": "ls"}{"command": "pwd"`},
		"prose mentioning the markup":            {"The server leaks `<tool_call>` and `<parameter=command>` into content.", `{"command": "ls"`},
		"block without a tool name":              {`<tool_call>{"name":"bash"}</tool_call>`, `{"command": "ls"`},
	}
	for name, c := range cases {
		if got := recoverLeakedToolCalls(testTools(), nil, c.leaked, c.orphan); len(got) != 0 {
			t.Errorf("%s: recovered %+v, want nothing", name, got)
		}
	}
	if got := recoverLeakedToolCalls(nil, nil, ok, `{"command": "ls"`); len(got) != 0 {
		t.Errorf("no tools in request: recovered %+v", got)
	}
}

// 2026-09-13 shape: the call was announced natively (brace repaired by
// accumulateNativeToolCalls) AND leaked as text — must not run twice.
func TestRecoverLeakedToolCalls_SkipsCallAlreadyDeliveredNatively(t *testing.T) {
	leaked := "<tool_call>\n<function=search_signals>\n<parameter=max_results>20</parameter>\n<parameter=pattern>denied</parameter>\n<parameter=signals>[\"logs\"]</parameter>\n</tool_call>"
	native := []toolCall{{Function: toolCallFunction{Name: "search_signals", Arguments: `{"max_results": 20, "pattern": "denied", "signals": ["logs"]}`}}}
	if got := recoverLeakedToolCalls(testTools(), native, leaked, `{"max_results": 20, "pattern": "denied"`); len(got) != 0 {
		t.Fatalf("duplicate of a native call was recovered: %+v", got)
	}
}

func TestOrphanArgText_CollectsDroppedAndUnannounced(t *testing.T) {
	partial := map[int]*toolCall{}
	for _, c := range []toolCall{
		chunk(0, "", "", `{"command": "dropped"`),
		chunk(0, "call_b", "bash", ""),
		chunk(0, "", "", `{"command": "real"}`),
		chunk(1, "", "", `{"command": "unannounced"`),
	} {
		accumulateNativeToolCalls([]toolCall{c}, partial)
	}
	txt := orphanArgText(partial)
	if !strings.Contains(txt, "dropped") || !strings.Contains(txt, "unannounced") || strings.Contains(txt, "real") {
		t.Fatalf("orphan text = %q", txt)
	}
}

// End to end: the leaked call is recovered, executed (read_file on a real
// temp file), and the model gets the result — no retry nudge, no markup shown.
func TestRun_LeakedToolCall_IsRecoveredAndExecuted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(path, []byte("secret-contents-42\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var reqs atomic.Int32
	var followUp atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if reqs.Add(1) == 1 {
			frag, _ := json.Marshal(`{"path": ` + fmt.Sprintf("%q", path))
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":%s}}]}}]}`+"\n\n", frag)
			leak, _ := json.Marshal(`<tool_call>=read_file><parameter=path>` + path + `</parameter></function></tool_call>`)
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%s}}]}`+"\n\n", leak)
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		b, _ := io.ReadAll(r.Body)
		followUp.Store(string(b))
		fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"read it"}}]}`+"\n\n")
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var out strings.Builder
	msgs, err := agent.Run(context.Background(), nil, "read the note", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reqs.Load(); got != 2 {
		t.Fatalf("want 2 requests (leaked call + follow-up), got %d", got)
	}
	body, _ := followUp.Load().(string)
	if !strings.Contains(body, "secret-contents-42") {
		t.Errorf("tool result never reached the model: %s", body)
	}
	if strings.Contains(body, "NOT executed") || strings.Contains(out.String(), "<tool_call>") {
		t.Errorf("retry nudge or markup leaked: out=%q", out.String())
	}
	if last := msgs[len(msgs)-1]; last.Content != "read it" {
		t.Errorf("final message = %+v", last)
	}
}
