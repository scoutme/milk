package main

import (
	"testing"

	"github.com/scoutme/milk/internal/agent/local"
)

func msgs(pairs ...string) []local.Message {
	var out []local.Message
	for i := 0; i+1 < len(pairs); i += 2 {
		out = append(out, local.Message{Role: pairs[i], Content: pairs[i+1]})
	}
	return out
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

func TestTrimLocalMessages_DropsOldestPair(t *testing.T) {
	// Two pairs: first is large, second is small. Budget fits only second.
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
	if len(got) != 2 {
		t.Errorf("expected 2 messages after trim, got %d", len(got))
	}
	if got[0].Content != "second" {
		t.Errorf("expected oldest pair dropped, got first msg: %q", got[0].Content)
	}
}

func TestTrimLocalMessages_DropsToolResults(t *testing.T) {
	// user + assistant (tool call) + tool result + user + assistant
	// First group should be dropped together.
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
	// q1 + big assistant + tool result should all be dropped
	for _, m := range got {
		if m.Content == big || m.Content == "result" || m.Content == "q1" {
			t.Errorf("expected dropped content to be gone, still found %q", m.Content)
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
// clamped to 1 (forcing all history dropped) rather than left at the original
// budget value (the pre-fix bug that caused unbounded context growth).
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
		t.Error("expected all history trimmed when overhead exceeds budget")
	}
	if len(got) != 0 {
		t.Errorf("expected 0 messages remaining, got %d", len(got))
	}
}
