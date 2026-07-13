package local

// Contract tests for the history-strip invariant:
// history passed to isRepeatedPrompt must not contain the current user turn.
// These tests document what happens when that contract is met vs. violated,
// and will remain correct after the ADR-0038 refactor (which makes the
// contract automatic by moving AddTurn to after Execute).

import "testing"

// TestIsRepeatedPrompt_NoPendingTurn — when history contains only answered
// turns, isRepeatedPrompt must not fire for a novel prompt.
func TestIsRepeatedPrompt_NoPendingTurn(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "write a unit test for the auth module"},
		{Role: "assistant", Content: "here is the test"},
		{Role: "user", Content: "now refactor the session store"},
		{Role: "assistant", Content: "done"},
	}
	if isRepeatedPrompt(history, "a brand new question about deployment") {
		t.Error("isRepeatedPrompt fired for a novel prompt with no pending user turn in history")
	}
}

// TestIsRepeatedPrompt_PendingTurnWouldCauseFalsePositive — demonstrates
// the bug the strip sites prevent: if the current user turn were left in
// history, isRepeatedPrompt would immediately fire on the very next turn
// with the same prompt, even though it hasn't been answered yet.
func TestIsRepeatedPrompt_PendingTurnWouldCauseFalsePositive(t *testing.T) {
	const prompt = "write a unit test for the auth module"
	// Simulate what would happen WITHOUT the strip: history already contains
	// the current prompt as a trailing unanswered user turn.
	historyWithPending := []Message{
		{Role: "user", Content: prompt},
		{Role: "assistant", Content: "first attempt"},
		{Role: "user", Content: prompt}, // ← trailing unanswered turn (no strip applied)
	}
	// isRepeatedPrompt would fire because the prompt appears at distance 1
	// (the trailing turn) with score 1.0.
	if !isRepeatedPrompt(historyWithPending, prompt) {
		t.Error("expected isRepeatedPrompt to fire when pending turn is not stripped — this test documents the bug the strip sites prevent")
	}
}
