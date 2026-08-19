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

// TestRun_ReasoningOnlyCompletion_FallsBackToReasoningText verifies that when
// the model streams only reasoning_content (no content, no tool calls) before
// the stream ends cleanly, Run() does not terminate the turn with an empty
// assistant message. Instead it falls back to the reasoning text so the turn
// persists to session history instead of vanishing (see dispatch.go's
// `res.Text != ""` guard, which would otherwise silently drop it).
func TestRun_ReasoningOnlyCompletion_FallsBackToReasoningText(t *testing.T) {
	const reasoning = "Now let me update the exchange_list.php template."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", reasoning)
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "hi", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if len(history) == 0 {
		t.Fatal("expected non-empty updated history")
	}
	last := history[len(history)-1]
	if last.Role != "assistant" {
		t.Fatalf("expected last message role=assistant, got %q", last.Role)
	}
	if last.Content != reasoning {
		t.Errorf("expected assistant content to fall back to reasoning text %q, got %q", reasoning, last.Content)
	}
}

// TestRun_ToolCallsThenEmptyCompletion_PreservesToolTrail verifies the
// reported scenario: a turn makes real tool calls (edits, etc.) across
// several loop iterations, then the final completion comes back empty
// (reasoning-only, no content, no tool calls). The fallback must not just
// carry the trailing reasoning — it must also preserve a record of what the
// turn actually did (the tool calls and their results), since that is the
// "main content" a later "resume" turn needs, not just the last thought.
func TestRun_ToolCallsThenEmptyCompletion_PreservesToolTrail(t *testing.T) {
	const reasoning = "Now let me update the exchange_list.php template."
	var calls atomic.Int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if calls.Add(1) == 1 {
			// First completion: a native tool call (e.g. an edit).
			fmt.Fprint(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc1","function":{"name":"read_file","arguments":"{\"path\":\"/nonexistent/style.css\"}"}}]}}]}`+"\n\n")
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			return
		}
		// Second completion: reasoning-only, empty content, no tool calls.
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":%q}}]}\n\n", reasoning)
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model")
	sess := &session.Session{}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "resume", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if last.Role != "assistant" {
		t.Fatalf("expected last message role=assistant, got %q", last.Role)
	}
	if !strings.Contains(last.Content, "read_file") {
		t.Errorf("expected fallback content to mention the tool call made this turn, got %q", last.Content)
	}
	if !strings.Contains(last.Content, reasoning) {
		t.Errorf("expected fallback content to also keep the reasoning text, got %q", last.Content)
	}
}
