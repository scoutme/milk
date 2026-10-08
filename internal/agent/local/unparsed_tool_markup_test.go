package local

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

func TestStripUnparsedToolMarkup(t *testing.T) {
	// Verbatim samples leaked into content by the upstream server.
	leaked := []string{
		`<tool_call>=get_session_context><parameter=agent>escalation</parameter><parameter=last_n>10</parameter></function></tool_call><tool_call>=get_session_context><parameter=last_n>10</parameter><parameter=pattern>wizard</parameter></function></tool_call>`,
		`<tool_call><=get_session_context><parameter=pattern>background</parameter></function></tool_call>`,
		"<tool_call>\n<function=search_signals>\n<parameter=pattern>denied</parameter>\n</tool_call>",
	}
	for _, s := range leaked {
		clean, found := stripUnparsedToolMarkup(s)
		if !found || strings.Contains(clean, "<tool_call>") || strings.Contains(clean, "<parameter=") {
			t.Errorf("not stripped: found=%v clean=%q", found, clean)
		}
	}
}

func TestStripUnparsedToolMarkup_LeavesNormalTextAlone(t *testing.T) {
	for _, s := range []string{
		"Done — all green.",
		"The server leaks `<tool_call>` tags and `<parameter=x>` fragments into content.",
		`<tool_call>{"name":"bash","arguments":{"command":"ls"}}</tool_call>`,
	} {
		if clean, found := stripUnparsedToolMarkup(s); found || clean != s {
			t.Errorf("modified %q -> %q (found=%v)", s, clean, found)
		}
	}
}

func TestSummarizeToolTrail_CapsLongTurns(t *testing.T) {
	var msgs []Message
	for i := 0; i < 200; i++ {
		msgs = append(msgs,
			Message{Role: "assistant", ToolCalls: []toolCall{{Function: toolCallFunction{Name: "bash", Arguments: `{"command":"echo step"}`}}}},
			Message{Role: "tool", Content: strings.Repeat("x", 250)})
	}
	out := summarizeToolTrailWithHeader(msgs, "", unparsedToolCallTrailHeader)
	if len(out) > maxToolTrailChars+500 {
		t.Fatalf("trail not capped: %d chars", len(out))
	}
	if !strings.HasPrefix(out, unparsedToolCallTrailHeader) || !strings.Contains(out, "earlier entries omitted") {
		t.Fatalf("missing header or omission note: %q", out[:200])
	}
	if got := summarizeToolTrail(nil, "keep"); got != "keep" {
		t.Fatalf("no tool activity must return resp unchanged, got %q", got)
	}
}

func TestRun_UnparsedMarkupTurn_RetractsStreamedText(t *testing.T) {
	const markup = `<tool_call>=bash><parameter=command>ls</parameter></function></tool_call>`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		okStream(w, markup)
	}))
	defer srv.Close()

	var from, to string
	agent := New(srv.URL, "test-model").WithOnRetract(func(f, t string) { from, to = f, t })
	var out strings.Builder
	msgs, err := agent.Run(context.Background(), nil, "list files", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if from == "" || !strings.Contains(out.String(), from) {
		t.Fatalf("retraction must name text that actually streamed: from=%q streamed=%q", from, out.String())
	}
	if last := msgs[len(msgs)-1]; last.Content != to || strings.Contains(to, "<tool_call>") || to == "" {
		t.Fatalf("persisted %q, retracted-to %q", last.Content, to)
	}
}
