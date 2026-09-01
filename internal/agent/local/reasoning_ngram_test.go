package local

import (
	"fmt"
	"strings"
	"testing"
)

func TestReasoningNgramMonitor_DetectsPeriodicRepetition(t *testing.T) {
	m := newReasoningNgramMonitor()
	// Simulate the "I'm done → let me check → I'm done → let me check" pattern.
	// Each cycle has slightly different content after the common prefix, but
	// the token-level periodic structure is the same.
	cycle := "OK I'm done let me write up my findings actually wait let me check one more thing the acceptance criteria says "
	// Feed enough cycles to exceed threshold (default 10).
	for i := 0; i < 12; i++ {
		if m.Feed(cycle) {
			// Detected at cycle i+1 — that's fine, as long as it triggers.
			return
		}
	}
	t.Error("expected n-gram monitor to detect periodic repetition after 12 cycles")
}

func TestReasoningNgramMonitor_NoFalsePositiveOnUniqueText(t *testing.T) {
	m := newReasoningNgramMonitor()
	// Feed varied text that should NOT trigger.
	sentences := []string{
		"Let me read the file to understand the current state.",
		"I see the issue now. The function returns early when the condition is met.",
		"Let me fix this by adding a check for nil before dereferencing.",
		"The test should pass now. Let me run it to verify.",
		"Great, all tests pass. Let me also check the linter.",
	}
	for _, s := range sentences {
		if m.Feed(s) {
			t.Error("should not trigger on varied text")
		}
	}
}

func TestReasoningNgramMonitor_Reset(t *testing.T) {
	m := newReasoningNgramMonitor()
	cycle := "I'm done let me write findings actually wait check again "
	for i := 0; i < 12; i++ {
		m.Feed(cycle)
	}
	m.Reset()
	// After reset, feeding the same text should not immediately trigger.
	if m.Feed(cycle) {
		t.Error("should not trigger immediately after reset")
	}
}

func TestDetectConsecutiveRepeat_BasicPattern(t *testing.T) {
	// A B C D A B C D A B C D — period=4, threshold=3
	tokens := []string{"a", "b", "c", "d", "a", "b", "c", "d", "a", "b", "c", "d"}
	if !detectConsecutiveRepeat(tokens, 4, 3, 3) {
		t.Error("expected detection of A B C D repeated 3 times")
	}
}

func TestDetectConsecutiveRepeat_NoRepeat(t *testing.T) {
	tokens := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i", "j", "k", "l"}
	if detectConsecutiveRepeat(tokens, 4, 3, 3) {
		t.Error("should not detect repetition in unique sequence")
	}
}

func TestDetectConsecutiveRepeat_TooFewRepetitions(t *testing.T) {
	// A B C D A B C D — only 2 repetitions, threshold=3
	tokens := []string{"a", "b", "c", "d", "a", "b", "c", "d"}
	if detectConsecutiveRepeat(tokens, 4, 3, 3) {
		t.Error("should not trigger with only 2 repetitions when threshold=3")
	}
}

func TestDetectConsecutiveRepeat_MinDistinctFilter(t *testing.T) {
	// "a a a a a a a a a a a a" — period=1, but only 1 distinct token (< minDistinct=3)
	tokens := make([]string, 12)
	for i := range tokens {
		tokens[i] = "a"
	}
	if detectConsecutiveRepeat(tokens, 1, 3, 3) {
		t.Error("should not trigger when block has fewer than minDistinct tokens")
	}
}

func TestDetectConsecutiveRepeat_LargeBlock(t *testing.T) {
	// Simulate a 200-token block repeating 3 times in a 600-token window.
	// This is the pattern from real reasoning loops where the model repeats
	// a large block of analysis verbatim.
	block := make([]string, 200)
	for i := range block {
		block[i] = fmt.Sprintf("token_%d", i%20) // 20 distinct tokens
	}
	tokens := make([]string, 0, 600)
	for i := 0; i < 3; i++ {
		tokens = append(tokens, block...)
	}
	if !detectConsecutiveRepeat(tokens, 4, 3, 3) {
		t.Error("expected detection of 200-token block repeated 3 times")
	}
}

func TestDetectConsecutiveRepeat_200TokenBlock4x(t *testing.T) {
	// Real-world case: model repeats a ~200-token planning block 4 times.
	// With window=1000 and threshold=3, this should trigger.
	block := make([]string, 200)
	for i := range block {
		block[i] = fmt.Sprintf("word_%d", i%30)
	}
	tokens := make([]string, 0, 800)
	for i := 0; i < 4; i++ {
		tokens = append(tokens, block...)
	}
	if !detectConsecutiveRepeat(tokens, 4, 3, 3) {
		t.Error("expected detection of 200-token block repeated 4 times")
	}
}

func TestNormalisedHash_Truncation(t *testing.T) {
	// Two long texts that share the first 500+ chars but differ after should
	// hash identically because normalisedHash truncates to streakHashPrefixLen.
	prefix := strings.Repeat("the quick brown fox jumps over the lazy dog ", 15) // 660 chars
	suffix1 := strings.Repeat("x", 1000)
	suffix2 := strings.Repeat("y", 1000)
	h1 := normalisedHash(prefix + suffix1)
	h2 := normalisedHash(prefix + suffix2)
	if h1 != h2 {
		t.Fatalf("truncation should make long texts with same prefix hash identically:\n  h1=%s\n  h2=%s", h1, h2)
	}
}

func TestNormalisedHash_LeadingPhraseStripping(t *testing.T) {
	// "Let me check the file" and "I'll check the file" should hash identically.
	h1 := normalisedHash("Let me check the file")
	h2 := normalisedHash("I'll check the file")
	if h1 != h2 {
		t.Fatalf("leading phrase stripping should normalize these to the same hash:\n  h1=%s\n  h2=%s", h1, h2)
	}
}

func TestNormalisedHash_ShortTextUnaffected(t *testing.T) {
	// Short texts that differ should still hash differently.
	h1 := normalisedHash("hello")
	h2 := normalisedHash("world")
	if h1 == h2 {
		t.Fatal("different short texts should hash differently")
	}
}

func TestTextLoopTracker_DetectsStreak(t *testing.T) {
	tr := &textLoopTracker{}
	if tr.recordStep("The answer is 42") {
		t.Error("should not trigger on 1st step")
	}
	if tr.recordStep("The answer is 42") {
		t.Error("should not trigger on 2nd step")
	}
	if !tr.recordStep("The answer is 42") {
		t.Error("should trigger on 3rd identical step")
	}
}

func TestTextLoopTracker_BreaksOnDifferentText(t *testing.T) {
	tr := &textLoopTracker{}
	tr.recordStep("The answer is 42")
	tr.recordStep("The answer is 42")
	tr.recordStep("The answer is 43") // different text breaks the streak
	if tr.recordStep("The answer is 43") {
		t.Error("should not trigger after streak was broken")
	}
}

func TestTextLoopTracker_NormalizesCase(t *testing.T) {
	tr := &textLoopTracker{}
	tr.recordStep("The Answer Is 42")
	tr.recordStep("the answer is 42")
	if !tr.recordStep("THE ANSWER IS 42") {
		t.Error("should trigger after normalizing case")
	}
}

func TestTextLoopTracker_NormalizesLeadingPhrases(t *testing.T) {
	tr := &textLoopTracker{}
	tr.recordStep("Let me check the file")
	tr.recordStep("I'll check the file")
	if !tr.recordStep("I will check the file") {
		t.Error("should trigger after normalizing leading phrases")
	}
}

func TestTextLoopTracker_Reset(t *testing.T) {
	tr := &textLoopTracker{}
	tr.recordStep("The answer is 42")
	tr.recordStep("The answer is 42")
	tr.reset()
	if tr.recordStep("The answer is 42") {
		t.Error("should not trigger after reset")
	}
}
