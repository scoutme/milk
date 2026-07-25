package local

import "testing"

const longPrompt = "write a unit test for the auth module"

func TestIsRepeatedPrompt_DetectsRepeat(t *testing.T) {
	// Immediate repeat (distance 1): score = 1.0 → escalate.
	history := []Message{
		{Role: "user", Content: longPrompt},
		{Role: "assistant", Content: "done"},
	}
	if !isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want true for immediate repeat (score 1.0)")
	}
}

func TestIsRepeatedPrompt_CaseAndSpaceInsensitive(t *testing.T) {
	history := []Message{{Role: "user", Content: "Write  a Unit Test For The Auth Module"}}
	if !isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want true for case/space-normalised repeat")
	}
}

func TestIsRepeatedPrompt_NoRepeat(t *testing.T) {
	history := []Message{{Role: "user", Content: longPrompt}}
	if isRepeatedPrompt(history, "write a unit test for the config module", 0) {
		t.Error("want false for different prompt")
	}
}

func TestIsRepeatedPrompt_EmptyPrompt(t *testing.T) {
	history := []Message{{Role: "user", Content: ""}}
	if isRepeatedPrompt(history, "", 0) {
		t.Error("want false for empty prompt")
	}
}

func TestIsRepeatedPrompt_ShortPromptSkipped(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "hi"},
		{Role: "user", Content: "hello"},
		{Role: "user", Content: "bye"},
	}
	for _, short := range []string{"hi", "hello", "bye", "ok", "yes"} {
		if isRepeatedPrompt(history, short, 0) {
			t.Errorf("want false for short prompt %q (under minRepeatCheckLen)", short)
		}
	}
}

func TestIsRepeatedPrompt_EmptyHistory(t *testing.T) {
	if isRepeatedPrompt(nil, "hello", 0) {
		t.Error("want false for empty history")
	}
}

func TestIsRepeatedPrompt_IgnoresAssistantTurns(t *testing.T) {
	history := []Message{{Role: "assistant", Content: longPrompt}}
	if isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want false when only assistant has the content")
	}
}

// Scoring tests — verify the weighted sum logic.

func TestIsRepeatedPrompt_OneIntermediateTurn_NoEscalate(t *testing.T) {
	// Pattern: other · match → current
	// User turns in history: [other, match]. Distance of match = 1, score = 1.0.
	// Wait — distance counts from the end: match is at index 1 (0-based from end),
	// so distance = 1, score = 1.0. This WOULD escalate.
	// To get score 0.5 we need: match · other → current (match at distance 2).
	history := []Message{
		{Role: "user", Content: longPrompt}, // distance 2 → weight 0.5
		{Role: "assistant", Content: "here"},
		{Role: "user", Content: "something else"}, // distance 1 → not a match
		{Role: "assistant", Content: "ok"},
	}
	// score = 0.5 < 1.0 → no escalation
	if isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want false: single match at distance 2 gives score 0.5, below threshold")
	}
}

func TestIsRepeatedPrompt_TwoMatches_Escalate(t *testing.T) {
	// Matches at distance 1 and 3: score = 1.0 + 1/3 ≥ 1.0 → escalate.
	history := []Message{
		{Role: "user", Content: "unrelated"}, // distance 4
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: longPrompt}, // distance 3 → weight 1/3
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: "unrelated"}, // distance 2
		{Role: "assistant", Content: "c"},
		{Role: "user", Content: longPrompt}, // distance 1 → weight 1.0
		{Role: "assistant", Content: "d"},
	}
	// score = 1.0 + 1/3 = 1.33 ≥ 1.0 → escalate
	if !isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want true: matches at distance 1 and 3, score 1.33")
	}
}

func TestIsRepeatedPrompt_TwoMatchesBelowThreshold_NoEscalate(t *testing.T) {
	// Matches at distance 2 and 3: score = 0.5 + 1/3 = 0.83 < 0.9 → no escalation.
	history := []Message{
		{Role: "user", Content: "unrelated"}, // distance 4
		{Role: "assistant", Content: "a"},
		{Role: "user", Content: longPrompt}, // distance 3 → weight 1/3
		{Role: "assistant", Content: "b"},
		{Role: "user", Content: longPrompt}, // distance 2 → weight 0.5
		{Role: "assistant", Content: "c"},
		{Role: "user", Content: "unrelated"}, // distance 1
		{Role: "assistant", Content: "d"},
	}
	// score = 0.5 + 1/3 = 0.83 < 0.9 → no escalation
	if isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want false: matches at distance 2 and 3, score 0.83 below threshold 0.9")
	}
}

func TestIsRepeatedPrompt_SingleMatchAtDistance2_NoEscalate(t *testing.T) {
	// Single match at distance 2: score = 0.5, not > 0.6 → no escalation.
	history := []Message{
		{Role: "user", Content: longPrompt}, // distance 2 → weight 0.5
		{Role: "assistant", Content: "here"},
		{Role: "user", Content: "something else"}, // distance 1 → not a match
		{Role: "assistant", Content: "ok"},
	}
	// score = 0.5, not > 0.6 → no escalation
	if isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want false: single match at distance 2, score 0.5 not > threshold 0.6")
	}
}

func TestIsRepeatedPrompt_Window_OldMatchesIgnored(t *testing.T) {
	// 12 user turns, only last 10 counted. Match only at position 11 (outside window).
	msgs := make([]Message, 0, 24)
	for i := 0; i < 11; i++ {
		msgs = append(msgs, Message{Role: "user", Content: "unrelated turn"})
		msgs = append(msgs, Message{Role: "assistant", Content: "ok"})
	}
	msgs[0] = Message{Role: "user", Content: longPrompt} // 12th user turn from end — outside window
	// Within the window (last 10 user turns) there are no matches → score = 0
	if isRepeatedPrompt(msgs, longPrompt, 0) {
		t.Error("want false: only match is outside the 10-turn window")
	}
}

// Tests for skipFirstUserTurns — the /primary baseline mechanism.

func TestIsRepeatedPrompt_SkipFirst_NoFire(t *testing.T) {
	// User asked longPrompt before /primary (2 prior user turns), then asks it again.
	// skipFirstUserTurns=2 should exclude both pre-/primary turns, so no repeat detected.
	history := []Message{
		{Role: "user", Content: longPrompt}, // pre-/primary turn 1 — skipped
		{Role: "assistant", Content: "tried"},
		{Role: "user", Content: longPrompt}, // pre-/primary turn 2 — skipped
		{Role: "assistant", Content: "tried again"},
	}
	if isRepeatedPrompt(history, longPrompt, 2) {
		t.Error("want false: both matches are in the skipped pre-/primary window")
	}
}

func TestIsRepeatedPrompt_SkipFirst_StillFiresOnPostBaselineRepeat(t *testing.T) {
	// 1 pre-/primary turn skipped; 1 post-/primary repeat → should still escalate.
	history := []Message{
		{Role: "user", Content: longPrompt}, // pre-/primary — skipped
		{Role: "assistant", Content: "old"},
		{Role: "user", Content: longPrompt}, // post-/primary (distance 1) — counted
		{Role: "assistant", Content: "new"},
	}
	if !isRepeatedPrompt(history, longPrompt, 1) {
		t.Error("want true: post-baseline match at distance 1 scores 1.0 → escalate")
	}
}

func TestIsRepeatedPrompt_SkipFirst_ZeroIsIdentical(t *testing.T) {
	// skipFirstUserTurns=0 must behave identically to the original (no-skip) path.
	history := []Message{
		{Role: "user", Content: longPrompt},
		{Role: "assistant", Content: "done"},
	}
	if !isRepeatedPrompt(history, longPrompt, 0) {
		t.Error("want true: immediate repeat with skip=0 should fire")
	}
}
