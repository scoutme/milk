package local

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// duplicateToolCallServer returns a non-identical tool call on request 2 (so
// the doom-loop gate's *consecutive*-match counter never reaches its
// threshold), then repeats request 1's exact call on request 3 — triggering
// the duplicate-tool-call detector (which tracks "ever executed this turn",
// not just consecutive) in isolation from the doom-loop gate. Request 4
// returns a final response.
func duplicateToolCallServer() (*httptest.Server, *int32) {
	var requests int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := atomic.AddInt32(&requests, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		switch count {
		case 1, 3:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc","function":{"name":"create_task","arguments":"{\"title\":\"a\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		case 2:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc","function":{"name":"create_task","arguments":"{\"title\":\"b\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	return srv, &requests
}

// TestDuplicateToolCall_RecoversWithoutTerminating establishes the baseline
// behavior the analysis in docs/session-2026-10-01-primary-overload-analysis.md
// relied on: a single repeated tool call crops the looping tail, injects a
// recovery nudge, and lets the turn continue to completion — it must not
// terminate after only one recovery (duplicateToolMaxRecovery is 5).
func TestDuplicateToolCall_RecoversWithoutTerminating(t *testing.T) {
	srv, requests := duplicateToolCallServer()
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "done") {
		t.Errorf("expected the turn to recover and complete normally, got %q", last.Content)
	}
	if got := atomic.LoadInt32(requests); got != 4 {
		t.Errorf("expected exactly 4 requests (1 unique + 1 unique + 1 duplicate + 1 final), got %d", got)
	}
}

// TestDuplicateToolCall_SessionIDIsLoggingOnly guards the sessionID parameter
// threaded into loopRecoveryAction for observability (session-tagged
// recovery logs) — it must never change the recovery outcome itself. A real
// (non-empty) session ID must behave identically to the ""-ID baseline above.
func TestDuplicateToolCall_SessionIDIsLoggingOnly(t *testing.T) {
	srv, requests := duplicateToolCallServer()
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	sess := &session.Session{ID: "sess-with-a-real-id"}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "done") {
		t.Errorf("expected the turn to recover and complete normally, got %q", last.Content)
	}
	if got := atomic.LoadInt32(requests); got != 4 {
		t.Errorf("expected exactly 4 requests, got %d", got)
	}
}

// A duplicate call is detected before it is appended to msgs, so nothing in
// msgs is looping: the recovery nudge must not delete the turn's earlier work.
func TestDuplicateToolCall_RecoveryKeepsEarlierToolWork(t *testing.T) {
	srv, _ := duplicateToolCallServer()
	defer srv.Close()

	agent := New(srv.URL, "test-model").WithMemConfig(MemConfig{MaxToolIterations: 15})
	var out strings.Builder
	history, err := agent.Run(context.Background(), nil, "do the thing", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	var toolIterations, nudges int
	for _, m := range history {
		if m.Role == "assistant" && len(m.ToolCalls) > 0 {
			toolIterations++
		}
		if m.Role == "user" && strings.Contains(m.Content, recoveryDuplicateToolMild[:60]) {
			nudges++
		}
	}
	if toolIterations != 2 || nudges != 1 {
		t.Fatalf("want both executed iterations kept and one nudge, got iterations=%d nudges=%d", toolIterations, nudges)
	}
}

func TestResetExecutedKeysAfterMutation(t *testing.T) {
	call := func(name, args string) toolCall {
		return toolCall{Function: toolCallFunction{Name: name, Arguments: args}}
	}
	build := call("bash", `{"command":"go build ./..."}`)
	edit := call("edit_file", `{"path":"a.go"}`)
	key := func(c toolCall) string { return c.Function.Name + "\x00" + c.Function.Arguments }

	keys := map[string]bool{key(build): true}
	resetExecutedKeysAfterMutation(keys, []toolCall{call("bash", `{"command":"ls"}`)})
	if !keys[key(build)] {
		t.Fatal("a batch that changed no files must not forget earlier calls")
	}

	keys[key(edit)] = true
	resetExecutedKeysAfterMutation(keys, []toolCall{edit})
	if keys[key(build)] {
		t.Error("a verification command must be forgotten once files changed")
	}
	if !keys[key(edit)] {
		t.Error("the mutating batch's own call must stay, so an immediate identical edit is still a duplicate")
	}
}
