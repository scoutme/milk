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

// leakedStream reproduces the failure seen live (2026-10-08 02:09): the server
// streams nameless argument fragments, flushes the call as XML-ish text in
// content, and finishes with finish_reason=tool_calls.
func leakedStream(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"command\": \"ls\""}}]}}]}`+"\n\n")
	fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"<tool_call>=no_such_tool><parameter=command>ls</parameter></function></tool_call>"}}]}`+"\n\n")
	fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func okStream(w http.ResponseWriter, text string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, `data: {"choices":[{"delta":{"content":%q}}]}`+"\n\n", text)
	fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	fmt.Fprint(w, "data: [DONE]\n\n")
}

func TestRun_UndeliveredToolCall_RetriesWithNudge(t *testing.T) {
	var reqs atomic.Int32
	var secondBody atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := reqs.Add(1)
		if n == 1 {
			leakedStream(w)
			return
		}
		b, _ := io.ReadAll(r.Body)
		secondBody.Store(string(b))
		okStream(w, "all done")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var out strings.Builder
	msgs, err := agent.Run(context.Background(), nil, "list files", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reqs.Load(); got != 2 {
		t.Fatalf("want 2 requests (original + retry), got %d", got)
	}
	if last := msgs[len(msgs)-1]; last.Role != "assistant" || last.Content != "all done" {
		t.Fatalf("final message = %+v", last)
	}
	if strings.Contains(out.String(), "<tool_call>") {
		t.Errorf("leaked markup reached the user: %q", out.String())
	}
	body, _ := secondBody.Load().(string)
	if !strings.Contains(body, "NOT executed") {
		t.Errorf("retry request lacks the nudge: %s", body)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "<tool_call>") {
			t.Errorf("leaked markup kept in messages: %+v", m)
		}
	}
}

func TestRun_UndeliveredToolCall_GivesUpAfterBoundedRetries(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reqs.Add(1)
		leakedStream(w)
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var out strings.Builder
	msgs, err := agent.Run(context.Background(), nil, "list files", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reqs.Load(); got != int32(maxMalformedToolCallRetries)+1 {
		t.Fatalf("want %d requests, got %d", maxMalformedToolCallRetries+1, got)
	}
	last := msgs[len(msgs)-1]
	if last.Role != "assistant" || strings.Contains(last.Content, "<tool_call>") || last.Content == "" {
		t.Fatalf("persisted final message must be a clean, non-empty note, got %q", last.Content)
	}
}

func TestRun_WellFormedToolCall_NoRetry(t *testing.T) {
	var reqs atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if reqs.Add(1) == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"list_tasks","arguments":"{}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		okStream(w, "finished")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	var out strings.Builder
	msgs, err := agent.Run(context.Background(), nil, "tasks?", &out, &session.Session{}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := reqs.Load(); got != 2 {
		t.Fatalf("want exactly 2 requests (call + follow-up), got %d", got)
	}
	for _, m := range msgs {
		if strings.Contains(m.Content, "NOT executed") {
			t.Fatalf("nudge injected for a healthy tool call: %+v", m)
		}
	}
}
