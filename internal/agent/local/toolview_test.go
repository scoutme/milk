package local

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// toolViewTestServer emits one grep tool call, then a final answer — the
// minimal scripted turn for observing the pre-disprint view callbacks.
func toolViewTestServer() *httptest.Server {
	var requests int32
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if count == 1 {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tv1","function":{"name":"grep","arguments":"{\"pattern\":\"needle\",\"path\":\".\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"found it"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
}

// TestToolCallView_CarriesFullInputAndGatesHint pins the host-takeover
// contract at the dispatch loop: a view callback receives the call's full
// input (untruncated summary, raw argument JSON, the pre-rendered hint bytes)
// and its true return suppresses the streamed hint + diff; a false return (or
// no callback) keeps the stream exactly as it was.
func TestToolCallView_CarriesFullInputAndGatesHint(t *testing.T) {
	srv := toolViewTestServer()
	defer srv.Close()

	type seen struct {
		outArg            any // the writer passed to the callback (identity)
		id, name, summary string
		argsJSON, diff    string
		hintText          string
	}
	var start seen
	var doneID, doneResult string
	var doneOut io.Writer
	var doneErr bool
	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 5}).
		WithOnToolCallView(func(out io.Writer, id, name, summary, argsJSON, diff, hintText string) bool {
			start = seen{outArg: out, id: id, name: name, summary: summary,
				argsJSON: argsJSON, diff: diff, hintText: hintText}
			return true // main-turn takeover: suppress the streamed hint
		}).
		WithOnToolCallDone(func(out io.Writer, id, result string, isError bool) bool {
			doneID, doneResult, doneOut, doneErr = id, result, out, isError
			return true
		})

	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "find it", &out, &session.Session{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	// The callback got the full input: exact id/name, the untruncated
	// summary, the raw argument JSON, and the hint bytes the stream would
	// have carried.
	if start.id != "tv1" || start.name != "grep" {
		t.Errorf("start identity = %q/%q, want tv1/grep", start.id, start.name)
	}
	// The summary is toolArgSummary's key-argument summary (the same string
	// the hint would carry) — untruncated, and always present.
	if start.summary == "" {
		t.Errorf("summary must carry the key-argument summary")
	}
	if !strings.Contains(start.argsJSON, `"pattern":"needle"`) {
		t.Errorf("argsJSON = %q, want the raw argument JSON", start.argsJSON)
	}
	if !strings.Contains(start.hintText, "» grep") || !strings.HasPrefix(start.hintText, "\n") {
		t.Errorf("hintText = %q, want the framed hint line", start.hintText)
	}
	// Suppressed: the run's writer carries no tool hint at all (grep has no
	// file diff, so nothing else either).
	if strings.Contains(out.String(), "» grep") {
		t.Errorf("hint must be suppressed when the view takes over, out = %q", out.String())
	}
	// The outcome twin fired with the same id and the run's writer.
	if doneID != "tv1" || doneOut == nil {
		t.Fatalf("done id = %q out = %v, want id tv1 with the run's writer", doneID, doneOut)
	}
	if doneResult == "" {
		t.Errorf("done result must carry the tool outcome (isError=%v)", doneErr)
	}
}

// TestToolCallView_RejectedKeepsStream is the other half of the gate: a
// false return leaves the streamed hint byte-for-byte as the default path
// prints it (the job/buffer case), and the callback still ran.
func TestToolCallView_RejectedKeepsStream(t *testing.T) {
	srv := toolViewTestServer()
	defer srv.Close()

	var called int
	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 5}).
		WithOnToolCallView(func(out io.Writer, id, name, summary, argsJSON, diff, hintText string) bool {
			called++
			return false // not my stream: keep the hint
		})

	var out strings.Builder
	if _, err := agent.Run(context.Background(), nil, "find it", &out, &session.Session{}, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if called == 0 {
		t.Fatal("view callback must run even when it declines")
	}
	if !strings.Contains(out.String(), "» grep") {
		t.Errorf("declined view must keep the streamed hint, out = %q", out.String())
	}
}

// TestToolHintText_MatchesPrintedLine pins the refactor invariant: the hint
// bytes handed to a view callback are exactly what printToolLine streams
// (framing, dim escape, truncation included).
func TestToolHintText_MatchesPrintedLine(t *testing.T) {
	tc := toolCall{Function: toolCallFunction{Name: "bash", Arguments: `{"command":"git status --porcelain"}`}}
	for _, width := range []int{0, 40, 200} {
		var printed strings.Builder
		printToolLine(&printed, tc, width)
		if got, want := toolHintText(tc, width), printed.String(); got != want {
			t.Errorf("width %d: toolHintText %q != printed %q", width, got, want)
		}
	}
}
