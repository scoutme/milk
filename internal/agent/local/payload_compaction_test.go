package local

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/scoutme/milk/internal/session"
)

// summarizeServer returns a canned non-streaming completion for Summarize's
// one-shot call. summaryText is echoed back as the "assistant" content.
func summarizeServer(summaryText string, usagePrompt, usageCompletion int64) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}],"usage":{"prompt_tokens":%d,"completion_tokens":%d}}`,
			summaryText, usagePrompt, usageCompletion)
	}))
}

func TestCompactForPayloadSize_CollapsesWholePriorTurnsInOneShot(t *testing.T) {
	srv := summarizeServer("[summary]", 10, 5)
	defer srv.Close()
	agent := New(srv.URL, "test-model")
	sess := &session.Session{ID: "sess-1"}

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first task"},
		{Role: "assistant", Content: "did the first task"},
		{Role: "user", Content: "second task"},
		{Role: "assistant", Content: "did the second task"},
		{Role: "user", Content: "third task"},
		{Role: "assistant", Content: "did the third task"},
		{Role: "user", Content: "current task"}, // protectedIdx
		{Role: "assistant", Content: "working on it"},
	}
	userMsgIdx := 7

	got, _, ok := agent.compactForPayloadSize(context.Background(), sess, msgs, userMsgIdx)
	if !ok {
		t.Fatal("expected compaction to succeed")
	}
	if got[0].Role != "system" || got[0].Content != "sys" {
		t.Fatalf("expected system prompt preserved at index 0, got %+v", got[0])
	}
	if got[1].Role != "system" || !strings.Contains(got[1].Content, "[summary]") {
		t.Fatalf("expected a summary message spliced in at index 1, got %+v", got[1])
	}
	if got[2].Role != "user" || got[2].Content != "current task" {
		t.Fatalf("expected the current turn's user message preserved right after the summary, got %+v", got[2])
	}
	if got[3].Content != "working on it" {
		t.Fatalf("expected the rest of the current turn preserved, got %+v", got[3])
	}
	if len(got) != 4 {
		t.Fatalf("expected all 3 prior turns collapsed into one summary message, got %d messages: %+v", len(got), got)
	}

	// Token usage for the Summarize call must be attributed under a
	// ":compaction" role, distinct from the turn's own usage.
	key := "test-model\x00primary:compaction"
	if sess.Tokens[key] == nil || sess.Tokens[key].Prompt != 10 || sess.Tokens[key].Completion != 5 {
		t.Fatalf("expected compaction token usage recorded under %q, got %+v", key, sess.Tokens)
	}
}

func TestCompactForPayloadSize_NoPriorTurns_CollapsesOlderHalfOfCurrentTurnTail(t *testing.T) {
	srv := summarizeServer("[summary]", 1, 1)
	defer srv.Close()
	agent := New(srv.URL, "test-model")
	sess := &session.Session{ID: "sess-2"}

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "current task"}, // protectedIdx, no prior turns before it
	}
	for i := 0; i < 10; i++ {
		msgs = append(msgs,
			Message{Role: "assistant", Content: fmt.Sprintf("step %d", i), ToolCalls: []toolCall{{ID: fmt.Sprintf("tc%d", i), Function: toolCallFunction{Name: "read_file"}}}},
			Message{Role: "tool", Content: fmt.Sprintf("result %d", i), ToolCallID: fmt.Sprintf("tc%d", i)},
		)
	}
	userMsgIdx := 1

	got, _, ok := agent.compactForPayloadSize(context.Background(), sess, msgs, userMsgIdx)
	if !ok {
		t.Fatal("expected compaction to succeed")
	}
	if got[0].Content != "sys" || got[1].Content != "current task" {
		t.Fatalf("expected system+user preserved at the head, got %+v / %+v", got[0], got[1])
	}
	if !strings.Contains(got[2].Content, "[summary]") {
		t.Fatalf("expected a summary message at index 2, got %+v", got[2])
	}
	// The most recent half of the tail must survive untouched — the model
	// needs recent tool output, not ancient history.
	tail := got[len(got)-2:]
	if tail[0].Content != "step 9" && tail[1].Content != "result 9" {
		t.Fatalf("expected the most recent step/result to survive at the tail, got %+v", tail)
	}
	if len(got) >= len(msgs) {
		t.Fatalf("expected the compacted result to be meaningfully shorter than the original %d messages, got %d", len(msgs), len(got))
	}
	// Never orphan a tool message right after the summary or at the head.
	for i, m := range got {
		if m.Role == "tool" && (i == 0 || got[i-1].Role != "assistant") {
			t.Fatalf("orphaned tool message at index %d: %+v", i, got)
		}
	}
}

func TestCompactForPayloadSize_SpanTooSmall_ReturnsUnchanged(t *testing.T) {
	srv := summarizeServer("[summary]", 1, 1)
	defer srv.Close()
	agent := New(srv.URL, "test-model")
	sess := &session.Session{ID: "sess-3"}

	// Only one small assistant+tool round past the protected user message —
	// below payloadCompactionMinSpan, not worth summarizing.
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "current task"},
		{Role: "assistant", Content: "step 0", ToolCalls: []toolCall{{ID: "tc0", Function: toolCallFunction{Name: "read_file"}}}},
		{Role: "tool", Content: "result 0", ToolCallID: "tc0"},
	}
	got, _, ok := agent.compactForPayloadSize(context.Background(), sess, msgs, 1)
	if ok {
		t.Fatalf("expected no compaction for a span below payloadCompactionMinSpan, got %+v", got)
	}
	if len(got) != len(msgs) {
		t.Fatalf("expected msgs returned unchanged, got %d messages (wanted %d)", len(got), len(msgs))
	}
}

func TestCompactForPayloadSize_SummarizeFails_ReturnsUnchanged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	agent := New(srv.URL, "test-model")
	sess := &session.Session{ID: "sess-4"}

	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "first task"},
		{Role: "assistant", Content: "did it"},
		{Role: "user", Content: "current task"},
		{Role: "assistant", Content: "working"},
	}
	got, _, ok := agent.compactForPayloadSize(context.Background(), sess, msgs, 3)
	if ok {
		t.Fatalf("expected compaction to fail gracefully on a Summarize error, got %+v", got)
	}
	if len(got) != len(msgs) {
		t.Fatalf("expected msgs returned unchanged on failure, got %d messages (wanted %d)", len(got), len(msgs))
	}
}

// TestRunToolLoop_PayloadCompaction_Fires drives a real multi-iteration turn
// through Run (not a direct compactForPayloadSize call) with a small
// maxPayloadBytes and compaction threshold, and asserts the server actually
// received at least one non-streaming request — the one-shot Summarize call
// compactForPayloadSize makes — proving runToolLoop's proactive check really
// invokes it, not just that the function works in isolation. Each tool call
// uses a unique, deliberately long "unknown tool" name so its dispatch
// error (echoing the name back) grows the tool-result Content enough to
// cross maxPayloadBytes within a few iterations, without needing real file
// or process I/O.
func TestRunToolLoop_PayloadCompaction_Fires(t *testing.T) {
	const bigNamePrefix = "unknown-tool-" // dispatch falls through to "unknown tool: <name>"
	bigSuffix := strings.Repeat("x", 1500)
	var streamReqs, nonStreamReqs int32

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(body, &req) //nolint:errcheck

		if !req.Stream {
			atomic.AddInt32(&nonStreamReqs, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"[compacted summary]"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}

		n := atomic.AddInt32(&streamReqs, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 8 {
			toolName := fmt.Sprintf("%s%d-%s", bigNamePrefix, n, bigSuffix)
			chunk := map[string]any{
				"choices": []map[string]any{{
					"delta": map[string]any{
						"tool_calls": []map[string]any{{
							"index":    0,
							"id":       fmt.Sprintf("tc%d", n),
							"function": map[string]any{"name": toolName, "arguments": "{}"},
						}},
					},
				}},
			}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\n", data)
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 15}).
		WithMaxPayloadBytes(3000).
		WithPayloadCompactionThreshold(2)
	sess := &session.Session{ID: "compaction-live-wiring"}
	var out strings.Builder

	history, err := agent.Run(context.Background(), nil, "do the thing", &out, sess, nil)
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	last := history[len(history)-1]
	if !strings.Contains(last.Content, "done") {
		t.Errorf("expected the turn to complete normally, got %q", last.Content)
	}
	if got := atomic.LoadInt32(&nonStreamReqs); got == 0 {
		t.Error("expected at least one non-streaming Summarize call — compactForPayloadSize was never invoked by runToolLoop's proactive check")
	}
}

// The provider's own prompt-size report — not a byte estimate — triggers
// compaction, and the turn keeps a valid user-message index afterwards.
func TestRunToolLoop_TokenTriggeredCompaction_Fires(t *testing.T) {
	var streamReqs, nonStreamReqs int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Stream bool `json:"stream"`
		}
		json.Unmarshal(body, &req) //nolint:errcheck
		if !req.Stream {
			atomic.AddInt32(&nonStreamReqs, 1)
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"choices":[{"message":{"content":"[compacted summary]"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			return
		}
		n := atomic.AddInt32(&streamReqs, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		if n <= 8 {
			fmt.Fprintf(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"tc%d","function":{"name":"unknown-tool-%d","arguments":"{}"}}]}}]}`+"\n\n", n, n)
			fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}]}`+"\n\n")
		}
		fmt.Fprintf(w, `data: {"choices":[],"usage":{"prompt_tokens":%d,"completion_tokens":5}}`+"\n\n", 1000*n)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	agent := New(srv.URL, "test-model").
		WithMemConfig(MemConfig{MaxToolIterations: 15}).
		WithMaxPayloadBytes(0). // byte trigger off: only the token one can fire
		WithCompactionTrigger(4000)
	var out strings.Builder
	history, err := agent.Run(context.Background(), nil, "do the thing", &out, &session.Session{ID: "token-compaction"}, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := atomic.LoadInt32(&nonStreamReqs); got == 0 {
		t.Fatal("prompt_tokens crossed the trigger but no Summarize call was made")
	}
	var summaries int
	for _, m := range history {
		if m.Role == "system" && strings.Contains(m.Content, "[compacted summary]") {
			summaries++
		}
	}
	if summaries == 0 {
		t.Error("compacted summary was not spliced into the turn's history")
	}
	if last := history[len(history)-1]; !strings.Contains(last.Content, "done") {
		t.Errorf("turn should finish normally, got %q", last.Content)
	}
}

func TestFilesTouchedAndRenderForSummary(t *testing.T) {
	call := func(name, args string) toolCall {
		return toolCall{Function: toolCallFunction{Name: name, Arguments: args}}
	}
	msgs := []Message{
		{Role: "assistant", ToolCalls: []toolCall{call("read_file", `{"path":"a.go"}`), call("read_file", `{"path":"b.go"}`)}},
		{Role: "tool", Content: strings.Repeat("x", 5000)},
		{Role: "assistant", ToolCalls: []toolCall{call("edit_file", `{"path":"a.go","old_string":"o","new_string":"n"}`), call("bash", `{"command":"go build ./..."}`)}},
		{Role: "assistant", ToolCalls: []toolCall{call("read_file", `{"path":"a.go"}`)}},
	}
	got := FilesTouched(msgs, 8)
	if !strings.Contains(got, "- b.go (read)") || !strings.Contains(got, "- a.go (edited)") {
		t.Errorf("manifest wrong (a.go stays 'edited' after a later read):\n%s", got)
	}
	if FilesTouched(msgs[:0], 8) != "" {
		t.Error("no tool calls, no manifest")
	}
	if limited := FilesTouched(msgs, 1); strings.Contains(limited, "b.go") {
		t.Errorf("limit keeps only the most recent file:\n%s", limited)
	}
	r := RenderForSummary(msgs)
	if !strings.Contains(r, "read_file(a.go)") || !strings.Contains(r, "bash(go build ./...)") {
		t.Errorf("tool calls missing from the summarizer input:\n%s", r)
	}
	if len(r) > 3000 {
		t.Errorf("a 5000-byte tool result must be cut for the summarizer, input is %d bytes", len(r))
	}
}
