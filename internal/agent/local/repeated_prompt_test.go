package local

import "testing"

const longPrompt = "write a unit test for the auth module"

func TestIsRepeatedPrompt_DetectsRepeat(t *testing.T) {
	history := []Message{
		{Role: "user", Content: longPrompt},
		{Role: "assistant", Content: "done"},
	}
	if !isRepeatedPrompt(history, longPrompt) {
		t.Error("want true for exact repeat")
	}
}

func TestIsRepeatedPrompt_CaseAndSpaceInsensitive(t *testing.T) {
	history := []Message{{Role: "user", Content: "Write  a Unit Test For The Auth Module"}}
	if !isRepeatedPrompt(history, longPrompt) {
		t.Error("want true for case/space-normalised repeat")
	}
}

func TestIsRepeatedPrompt_NoRepeat(t *testing.T) {
	history := []Message{{Role: "user", Content: longPrompt}}
	if isRepeatedPrompt(history, "write a unit test for the config module") {
		t.Error("want false for different prompt")
	}
}

func TestIsRepeatedPrompt_EmptyPrompt(t *testing.T) {
	history := []Message{{Role: "user", Content: ""}}
	if isRepeatedPrompt(history, "") {
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
		if isRepeatedPrompt(history, short) {
			t.Errorf("want false for short prompt %q (under minRepeatCheckLen)", short)
		}
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
