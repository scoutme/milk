package local

import "testing"

func TestIsRepeatedPrompt_DetectsRepeat(t *testing.T) {
	history := []Message{
		{Role: "user", Content: "write a test"},
		{Role: "assistant", Content: "done"},
	}
	if !isRepeatedPrompt(history, "write a test") {
		t.Error("want true for exact repeat")
	}
}

func TestIsRepeatedPrompt_CaseAndSpaceInsensitive(t *testing.T) {
	history := []Message{{Role: "user", Content: "Write  a Test"}}
	if !isRepeatedPrompt(history, "write a test") {
		t.Error("want true for case/space-normalised repeat")
	}
}

func TestIsRepeatedPrompt_NoRepeat(t *testing.T) {
	history := []Message{{Role: "user", Content: "do X"}}
	if isRepeatedPrompt(history, "do Y") {
		t.Error("want false for different prompt")
	}
}

func TestIsRepeatedPrompt_EmptyPrompt(t *testing.T) {
	history := []Message{{Role: "user", Content: ""}}
	if isRepeatedPrompt(history, "") {
		t.Error("want false for empty prompt")
	}
}

func TestIsRepeatedPrompt_EmptyHistory(t *testing.T) {
	if isRepeatedPrompt(nil, "hello") {
		t.Error("want false for empty history")
	}
}

func TestIsRepeatedPrompt_IgnoresAssistantTurns(t *testing.T) {
	history := []Message{{Role: "assistant", Content: "hello"}}
	if isRepeatedPrompt(history, "hello") {
		t.Error("want false when only assistant has the content")
	}
}
