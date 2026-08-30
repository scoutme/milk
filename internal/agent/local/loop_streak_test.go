package local

import (
	"testing"
)

func TestStepKeyFromIteration_ReasoningHash(t *testing.T) {
	// Two calls with the same reasoning text should produce the same key.
	k1 := stepKeyFromIteration("I need to read the file first", nil)
	k2 := stepKeyFromIteration("I need to read the file first", nil)
	if k1 != k2 {
		t.Fatalf("expected identical keys, got %q vs %q", k1, k2)
	}
	if k1 == "" {
		t.Fatal("expected non-empty key")
	}
	// Different reasoning should produce a different key.
	k3 := stepKeyFromIteration("Let me check the imports instead", nil)
	if k3 == k1 {
		t.Fatalf("expected different keys for different reasoning, got %q", k3)
	}
}

func TestStepKeyFromIteration_ReasoningNormalization(t *testing.T) {
	// Normalization should make case/whitespace differences invisible.
	k1 := stepKeyFromIteration("I need to read the file", nil)
	k2 := stepKeyFromIteration("  I  NEED  to  Read  the  file  ", nil)
	if k1 != k2 {
		t.Fatalf("normalization should collapse differences, got %q vs %q", k1, k2)
	}
}

func TestStepKeyFromIteration_ToolSignature(t *testing.T) {
	// Without reasoning, the key should be based on tool calls.
	tcs := []toolCall{
		{Function: toolCallFunction{Name: "read_file", Arguments: `{"path":"foo.go"}`}},
	}
	k1 := stepKeyFromIteration("", tcs)
	if k1 == "" {
		t.Fatal("expected non-empty key from tool calls")
	}
	// Same tool call should produce the same key.
	k2 := stepKeyFromIteration("", tcs)
	if k1 != k2 {
		t.Fatalf("expected identical keys, got %q vs %q", k1, k2)
	}
	// Different tool call should produce a different key.
	k3 := stepKeyFromIteration("", []toolCall{
		{Function: toolCallFunction{Name: "grep", Arguments: `{"pattern":"TODO"}`}},
	})
	if k3 == k1 {
		t.Fatalf("expected different keys, got %q", k3)
	}
}

func TestStepKeyFromIteration_ToolSignatureKeyOrder(t *testing.T) {
	// Key order in JSON arguments should not affect the signature.
	tcs1 := []toolCall{
		{Function: toolCallFunction{Name: "bash", Arguments: `{"command":"ls","timeout":30}`}},
	}
	tcs2 := []toolCall{
		{Function: toolCallFunction{Name: "bash", Arguments: `{"timeout":30,"command":"ls"}`}},
	}
	k1 := stepKeyFromIteration("", tcs1)
	k2 := stepKeyFromIteration("", tcs2)
	if k1 != k2 {
		t.Fatalf("key order should not matter, got %q vs %q", k1, k2)
	}
}

func TestStepKeyFromIteration_ReasoningTakesPriority(t *testing.T) {
	// When reasoning is present, it should be used even if tool calls differ.
	tcs1 := []toolCall{
		{Function: toolCallFunction{Name: "read_file", Arguments: `{"path":"a.go"}`}},
	}
	tcs2 := []toolCall{
		{Function: toolCallFunction{Name: "read_file", Arguments: `{"path":"b.go"}`}},
	}
	k1 := stepKeyFromIteration("same thinking", tcs1)
	k2 := stepKeyFromIteration("same thinking", tcs2)
	if k1 != k2 {
		t.Fatalf("reasoning hash should dominate, got %q vs %q", k1, k2)
	}
}

func TestLoopStreakTracker_DetectsStreak(t *testing.T) {
	tr := &loopStreakTracker{}
	if tr.recordStep("A") {
		t.Error("should not trigger on 1st step")
	}
	if tr.recordStep("A") {
		t.Error("should not trigger on 2nd step")
	}
	if !tr.recordStep("A") {
		t.Error("should trigger on 3rd identical step")
	}
}

func TestLoopStreakTracker_BreaksOnDifferentKey(t *testing.T) {
	tr := &loopStreakTracker{}
	tr.recordStep("A")
	tr.recordStep("A")
	tr.recordStep("B") // different key breaks the streak
	if tr.recordStep("B") {
		t.Error("should not trigger after streak was broken")
	}
}

func TestLoopStreakTracker_IgnoresEmptyKey(t *testing.T) {
	tr := &loopStreakTracker{}
	if tr.recordStep("") {
		t.Error("empty key should not trigger")
	}
	tr.recordStep("A")
	tr.recordStep("A")
	if tr.recordStep("") {
		t.Error("empty key should not count toward streak")
	}
}

func TestLoopStreakTracker_Reset(t *testing.T) {
	tr := &loopStreakTracker{}
	tr.recordStep("A")
	tr.recordStep("A")
	tr.reset()
	if tr.recordStep("A") {
		t.Error("should not trigger after reset")
	}
}

func TestCropLoopingMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do something"},
		{Role: "assistant", Content: "ok", ToolCalls: []toolCall{{Function: toolCallFunction{Name: "bash"}}}},
		{Role: "tool", Content: "result"},
		{Role: "assistant", Content: "again", ToolCalls: []toolCall{{Function: toolCallFunction{Name: "bash"}}}},
		{Role: "tool", Content: "result2"},
	}
	cropped := cropLoopingMessages(msgs, 1)
	if len(cropped) != 2 {
		t.Fatalf("expected 2 messages (system+user), got %d", len(cropped))
	}
	if cropped[0].Role != "system" || cropped[1].Role != "user" {
		t.Fatalf("expected system+user, got %s+%s", cropped[0].Role, cropped[1].Role)
	}
}

func TestCropLoopingMessages_PreservesNonLoopMessages(t *testing.T) {
	msgs := []Message{
		{Role: "system", Content: "sys"},
		{Role: "user", Content: "do something"},
		{Role: "assistant", Content: "thinking"},
		{Role: "assistant", Content: "ok", ToolCalls: []toolCall{{Function: toolCallFunction{Name: "bash"}}}},
		{Role: "tool", Content: "result"},
	}
	cropped := cropLoopingMessages(msgs, 1)
	if len(cropped) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(cropped))
	}
}

func TestStableStringify(t *testing.T) {
	s1 := stableStringify(`{"b":2,"a":1}`)
	s2 := stableStringify(`{"a":1,"b":2}`)
	if s1 != s2 {
		t.Fatalf("stableStringify should be order-independent, got %q vs %q", s1, s2)
	}
	if s1 != `{"a":1,"b":2}` {
		t.Fatalf("expected sorted keys, got %q", s1)
	}
}

func TestNormalisedHash_Deterministic(t *testing.T) {
	h1 := normalisedHash("Hello World")
	h2 := normalisedHash("hello world")
	h3 := normalisedHash("  HELLO   WORLD  ")
	if h1 != h2 || h2 != h3 {
		t.Fatalf("expected identical hashes, got %q %q %q", h1, h2, h3)
	}
}
