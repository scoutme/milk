package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
	"github.com/scoutme/milk/internal/session"
)

func msgs(pairs ...string) []local.Message {
	var out []local.Message
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, local.Message{Role: pairs[i], Content: pairs[i+1]})
	}
	return out
}

func joined(msgs []local.Message) string {
	var parts []string
	for _, m := range msgs {
		parts = append(parts, m.Content)
	}
	return strings.Join(parts, "\n")
}

func TestTrimLocalMessages_NoBudget(t *testing.T) {
	in := msgs("user", "hello", "assistant", "world")
	got, trimmed := trimLocalMessages(in, 0)
	if trimmed {
		t.Error("budget=0 should never trim")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 messages, got %d", len(got))
	}
}

func TestTrimLocalMessages_WithinBudget(t *testing.T) {
	in := msgs("user", "hi", "assistant", "hey")
	got, trimmed := trimLocalMessages(in, 10000)
	if trimmed {
		t.Error("should not trim when under budget")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 messages, got %d", len(got))
	}
}

// TestTrimLocalMessages_KeepsHeadAndTail pins head+tail retention: the
// original request (head) and the recent turns (tail) survive, the middle is
// dropped with an explicit marker — never a silent gap, never the task
// definition first (the old drop-oldest behavior).
func TestTrimLocalMessages_KeepsHeadAndTail(t *testing.T) {
	in := msgs(
		"user", "first long message that pushes over budget",
		"assistant", "first reply",
		"user", "second",
		"assistant", "ok",
	)
	budget := len("second") + len("ok") + 5
	got, trimmed := trimLocalMessages(in, budget)
	if !trimmed {
		t.Error("expected trimming to occur")
	}
	text := joined(got)
	if !strings.Contains(text, "first long message that pushes over budget") {
		t.Errorf("head (the original request) must survive, got %+v", got)
	}
	if !strings.Contains(text, "second") || !strings.Contains(text, "ok") {
		t.Errorf("tail (recent turns) must survive, got %+v", got)
	}
	if strings.Contains(text, "first reply") {
		t.Errorf("middle should be dropped, got %+v", got)
	}
	if !strings.Contains(text, "[milk] middle of the conversation omitted (1 message)") {
		t.Errorf("dropped middle must carry an explicit marker, got %+v", got)
	}
}

// TestTrimLocalMessages_DropsToolResults: an assistant turn is dropped
// together with the tool results that follow it (segment integrity), while
// head and tail survive.
func TestTrimLocalMessages_DropsToolResults(t *testing.T) {
	big := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" // 52 chars
	in := []local.Message{
		{Role: "user", Content: "q1"},
		{Role: "assistant", Content: big},
		{Role: "tool", Content: "result"},
		{Role: "user", Content: "q2"},
		{Role: "assistant", Content: "a2"},
	}
	budget := len("q2") + len("a2") + 5
	got, trimmed := trimLocalMessages(in, budget)
	if !trimmed {
		t.Error("expected trimming")
	}
	text := joined(got)
	for _, want := range []string{"q1", "q2", "a2"} {
		if !strings.Contains(text, want) {
			t.Errorf("expected %q to survive head+tail trim, got %+v", want, got)
		}
	}
	// The middle assistant turn and its tool result go together.
	for _, gone := range []string{big, "result"} {
		if strings.Contains(text, gone) {
			t.Errorf("expected middle content %q to be gone, got %+v", gone, got)
		}
	}
}

func TestTrimLocalMessages_EmptyInput(t *testing.T) {
	got, trimmed := trimLocalMessages(nil, 1000)
	if trimmed {
		t.Error("empty input should not trim")
	}
	if len(got) != 0 {
		t.Errorf("expected empty output, got %d", len(got))
	}
}

func TestMessagesCharCount(t *testing.T) {
	in := msgs("user", "hello", "assistant", "world")
	if got := messagesCharCount(in); got != 10 {
		t.Errorf("expected 10, got %d", got)
	}
}

// TestTrimLocalMessages_OverheadExceedsBudget verifies that when system
// overhead is larger than the full message budget the remaining budget is
// clamped to 1 (forcing an aggressive trim) rather than left at the original
// budget value (the pre-fix bug that caused unbounded context growth). Under
// head+tail retention even the aggressive trim keeps the original request and
// the latest turn — the middle is what gives way.
func TestTrimLocalMessages_OverheadExceedsBudget(t *testing.T) {
	in := msgs(
		"user", "first question",
		"assistant", "first answer",
		"user", "second question",
		"assistant", "second answer",
	)
	msgBudget := 100
	overhead := 150 // exceeds budget

	// Replicate the fixed runner logic: clamp to 1 when overhead >= budget so
	// trimLocalMessages doesn't interpret 0 as "no limit".
	remaining := msgBudget - overhead
	if remaining < 1 {
		remaining = 1
	}
	got, trimmed := trimLocalMessages(in, remaining)
	if !trimmed {
		t.Error("expected trimming when overhead exceeds budget")
	}
	text := joined(got)
	if !strings.Contains(text, "first question") || !strings.Contains(text, "second answer") {
		t.Errorf("task definition and latest turn must survive even the aggressive trim, got %+v", got)
	}
	if strings.Contains(text, "first answer") || strings.Contains(text, "second question") {
		t.Errorf("middle should be dropped, got %+v", got)
	}
}

// --- trimLocalMessagesWithCompaction ---

func TestTrimLocalMessagesWithCompaction_NilAgentFallsBackToHardDrop(t *testing.T) {
	in := msgs(
		"user", "first long message that pushes over budget",
		"assistant", "first reply",
		"user", "second",
		"assistant", "ok",
	)
	budget := len("second") + len("ok") + 5
	got, trimmed := trimLocalMessagesWithCompaction(context.Background(), nil, nil, in, budget)
	if !trimmed {
		t.Error("expected trimming to occur")
	}
	text := joined(got)
	if !strings.Contains(text, "first long message that pushes over budget") || !strings.Contains(text, "ok") {
		t.Errorf("expected head+tail kept in hard-drop fallback, got %+v", got)
	}
	if strings.Contains(text, "first reply") || !strings.Contains(text, "middle of the conversation omitted") {
		t.Errorf("expected middle dropped with marker in hard-drop fallback, got %+v", got)
	}
}

func TestTrimLocalMessagesWithCompaction_WithinBudgetNoOp(t *testing.T) {
	in := msgs("user", "hi", "assistant", "hey")
	got, trimmed := trimLocalMessagesWithCompaction(context.Background(), nil, nil, in, 10000)
	if trimmed {
		t.Error("should not trim when under budget")
	}
	if len(got) != 2 {
		t.Errorf("expected 2 messages, got %d", len(got))
	}
}

func TestTrimLocalMessagesWithCompaction_SplicesSummaryInsteadOfDropping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"choices":[{"message":{"content":"condensed summary of the dropped turns"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	}))
	defer srv.Close()
	agent := local.New(srv.URL, "test-model")

	in := msgs(
		"user", "first long message that pushes over budget",
		"assistant", "first reply",
		"user", "second",
		"assistant", "ok",
	)
	budget := len("second") + len("ok") + 5
	sess := &session.Session{}
	got, trimmed := trimLocalMessagesWithCompaction(context.Background(), agent, sess, in, budget)
	if !trimmed {
		t.Error("expected trimming to occur")
	}
	if len(got) != 4 {
		t.Fatalf("expected head + summary + tail = 4 messages, got %d: %+v", len(got), got)
	}
	if got[0].Content != "first long message that pushes over budget" {
		t.Errorf("expected the head (original request) first, got %q", got[0].Content)
	}
	if !strings.Contains(got[1].Content, "condensed summary of the dropped turns") {
		t.Errorf("expected summary of the middle spliced between head and tail, got %q", got[1].Content)
	}
	if got[2].Content != "second" || got[3].Content != "ok" {
		t.Errorf("expected tail preserved after the summary, got %+v", got[2:])
	}
	usage := sess.Tokens["test-model\x00primary:compaction"]
	if usage == nil || usage.Prompt != 10 || usage.Completion != 5 {
		t.Errorf("expected compaction token usage recorded on the session, got %+v", usage)
	}
}

func TestTrimLocalMessagesWithCompaction_SummarizeFailureFallsBackToHardDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	agent := local.New(srv.URL, "test-model")

	in := msgs(
		"user", "first long message that pushes over budget",
		"assistant", "first reply",
		"user", "second",
		"assistant", "ok",
	)
	budget := len("second") + len("ok") + 5
	got, trimmed := trimLocalMessagesWithCompaction(context.Background(), agent, nil, in, budget)
	if !trimmed {
		t.Error("expected trimming to occur")
	}
	text := joined(got)
	if !strings.Contains(text, "first long message that pushes over budget") || !strings.Contains(text, "second") {
		t.Errorf("expected head+tail kept on summarize failure, got %+v", got)
	}
	if strings.Contains(text, "first reply") {
		t.Errorf("expected middle dropped on summarize failure, got %+v", got)
	}
}
