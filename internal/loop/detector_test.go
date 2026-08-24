package loop

import (
	"testing"
	"time"
)

func testConfig() Config {
	cfg := Config{Enabled: true}
	cfg.defaults()
	return cfg
}

func TestTrigramSimilarity_Identical(t *testing.T) {
	if got := trigramSimilarity("hello world", "hello world"); got != 1.0 {
		t.Errorf("identical strings: got %f, want 1.0", got)
	}
}

func TestTrigramSimilarity_Different(t *testing.T) {
	if got := trigramSimilarity("hello", "zzzzz"); got > 0.3 {
		t.Errorf("different strings: got %f, want <0.3", got)
	}
}

func TestTrigramSimilarity_Similar(t *testing.T) {
	a := "The quick brown fox jumps over the lazy dog"
	b := "The quick brown fox leaps over the lazy dog"
	got := trigramSimilarity(a, b)
	if got < 0.7 || got > 0.95 {
		t.Errorf("similar strings: got %f, want 0.7-0.95", got)
	}
}

func TestTrigramSimilarity_Empty(t *testing.T) {
	if got := trigramSimilarity("", ""); got != 1.0 {
		t.Errorf("empty strings: got %f, want 1.0", got)
	}
}

// ── Response Repetition ──────────────────────────────────────────────────────

func TestResponseRepetition_NoRepeat(t *testing.T) {
	d := New(testConfig())
	now := time.Now()
	d.Feed(TurnSummary{Text: "first response", Timestamp: now})
	d.Feed(TurnSummary{Text: "second response", Timestamp: now})
	d.Feed(TurnSummary{Text: "third response completely different", Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: "fourth response", Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalResponseRepetition {
			t.Error("should not fire for different responses")
		}
	}
}

func TestResponseRepetition_FiresAfter3(t *testing.T) {
	d := New(testConfig())
	now := time.Now()
	resp := "I will now run the same command again to check the output"
	d.Feed(TurnSummary{Text: resp, Timestamp: now})
	d.Feed(TurnSummary{Text: resp, Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: resp, Timestamp: now})
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalResponseRepetition {
			found = true
			if v.Confidence < 0.7 {
				t.Errorf("confidence too low: %f", v.Confidence)
			}
		}
	}
	if !found {
		t.Error("expected SignalResponseRepetition after 3 identical responses")
	}
}

func TestResponseRepetition_BreaksOnDifferent(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConsecutiveSimilarResponses = 3
	d := New(cfg)
	now := time.Now()
	resp := "repeated response"
	d.Feed(TurnSummary{Text: resp, Timestamp: now})
	d.Feed(TurnSummary{Text: resp, Timestamp: now})
	d.Feed(TurnSummary{Text: "something completely different", Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: resp, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalResponseRepetition {
			t.Error("should not fire after a different response broke the streak")
		}
	}
}

// ── Token Velocity ───────────────────────────────────────────────────────────

func TestTokenVelocity_NoFireBelowThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.TokenVelocityThreshold = 300000
	d := New(cfg)
	now := time.Now()
	d.Feed(TurnSummary{InputTokens: 1000, OutputTokens: 500, Timestamp: now})
	verdicts := d.Feed(TurnSummary{InputTokens: 1000, OutputTokens: 500, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalTokenVelocity {
			t.Error("should not fire below threshold")
		}
	}
}

func TestTokenVelocity_FiresAboveThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.TokenVelocityThreshold = 10000
	d := New(cfg)
	now := time.Now()
	d.Feed(TurnSummary{InputTokens: 5000, OutputTokens: 3000, Timestamp: now})
	d.Feed(TurnSummary{InputTokens: 5000, OutputTokens: 3000, Timestamp: now})
	verdicts := d.Feed(TurnSummary{InputTokens: 5000, OutputTokens: 3000, Timestamp: now})
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalTokenVelocity {
			found = true
			if v.ShouldInterrupt {
				t.Error("token velocity should never auto-interrupt (warn only)")
			}
		}
	}
	if !found {
		t.Error("expected SignalTokenVelocity above threshold")
	}
}

func TestTokenVelocity_IgnoresOldTurns(t *testing.T) {
	cfg := testConfig()
	cfg.TokenVelocityThreshold = 10000
	cfg.TokenVelocityWindow = 30 * time.Second
	d := New(cfg)
	old := time.Now().Add(-60 * time.Second)
	now := time.Now()
	d.Feed(TurnSummary{InputTokens: 50000, OutputTokens: 50000, Timestamp: old})
	verdicts := d.Feed(TurnSummary{InputTokens: 100, OutputTokens: 100, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalTokenVelocity {
			t.Error("should ignore turns outside the velocity window")
		}
	}
}

func TestTokenVelocity_NeverInterruptsEvenAtHighConfidence(t *testing.T) {
	cfg := testConfig()
	cfg.TokenVelocityThreshold = 1000
	d := New(cfg)
	now := time.Now()
	// Way above threshold — should still warn-only
	for i := 0; i < 5; i++ {
		d.Feed(TurnSummary{InputTokens: 50000, OutputTokens: 50000, Timestamp: now})
	}
	verdicts := d.Feed(TurnSummary{InputTokens: 50000, OutputTokens: 50000, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalTokenVelocity {
			if v.ShouldInterrupt {
				t.Error("token velocity must never auto-interrupt, even at high confidence")
			}
			if v.Confidence < 0.8 {
				t.Errorf("expected high confidence for large burn, got %f", v.Confidence)
			}
		}
	}
}

// ── Silent Burn ──────────────────────────────────────────────────────────────

func TestSilentBurn_FiresOnHighInputLowOutput(t *testing.T) {
	d := New(testConfig())
	verdicts := d.Feed(TurnSummary{
		InputTokens:  50000,
		OutputTokens: 10,
		Timestamp:    time.Now(),
	})
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalSilentBurn {
			found = true
			if v.ShouldInterrupt {
				t.Error("silent burn should not auto-interrupt (medium confidence)")
			}
		}
	}
	if !found {
		t.Error("expected SignalSilentBurn for high input + low output")
	}
}

func TestSilentBurn_NoFireOnNormalOutput(t *testing.T) {
	d := New(testConfig())
	verdicts := d.Feed(TurnSummary{
		InputTokens:  50000,
		OutputTokens: 5000,
		Timestamp:    time.Now(),
	})
	for _, v := range verdicts {
		if v.Signal == SignalSilentBurn {
			t.Error("should not fire when output is substantial")
		}
	}
}

func TestSilentBurn_NoFireOnLowInput(t *testing.T) {
	d := New(testConfig())
	verdicts := d.Feed(TurnSummary{
		InputTokens:  100,
		OutputTokens: 10,
		Timestamp:    time.Now(),
	})
	for _, v := range verdicts {
		if v.Signal == SignalSilentBurn {
			t.Error("should not fire when input is low")
		}
	}
}

// ── Tool Call Echo ───────────────────────────────────────────────────────────

func TestToolCallEcho_FiresAfter3(t *testing.T) {
	cfg := testConfig()
	cfg.ToolEchoThreshold = 3
	d := New(cfg)
	now := time.Now()
	tool := "bash\x00ls -la"
	d.Feed(TurnSummary{ToolCalls: []string{tool}, Timestamp: now})
	d.Feed(TurnSummary{ToolCalls: []string{tool}, Timestamp: now})
	verdicts := d.Feed(TurnSummary{ToolCalls: []string{tool}, Timestamp: now})
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalToolCallEcho {
			found = true
			if !v.ShouldInterrupt {
				t.Error("tool echo should auto-interrupt")
			}
		}
	}
	if !found {
		t.Error("expected SignalToolCallEcho after 3 identical tool calls")
	}
}

func TestToolCallEcho_NoFireOnDifferent(t *testing.T) {
	cfg := testConfig()
	cfg.ToolEchoThreshold = 3
	d := New(cfg)
	now := time.Now()
	d.Feed(TurnSummary{ToolCalls: []string{"bash\x00ls"}, Timestamp: now})
	d.Feed(TurnSummary{ToolCalls: []string{"bash\x00cat foo"}, Timestamp: now})
	verdicts := d.Feed(TurnSummary{ToolCalls: []string{"bash\x00ls"}, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalToolCallEcho {
			t.Error("should not fire for different tool calls")
		}
	}
}

func TestToolCallEcho_NoFireWithoutToolCalls(t *testing.T) {
	cfg := testConfig()
	cfg.ToolEchoThreshold = 3
	d := New(cfg)
	now := time.Now()
	d.Feed(TurnSummary{Text: "hello", Timestamp: now})
	d.Feed(TurnSummary{Text: "hello", Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: "hello", Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalToolCallEcho {
			t.Error("should not fire without tool calls")
		}
	}
}

// ── Turn Flood ───────────────────────────────────────────────────────────────

func TestTurnFlood_FiresAfterThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConsecutiveTurnsWithoutUser = 3
	d := New(cfg)
	now := time.Now()
	d.Feed(TurnSummary{Text: "turn 1", IsUserTurn: false, Timestamp: now})
	d.Feed(TurnSummary{Text: "turn 2", IsUserTurn: false, Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: "turn 3", IsUserTurn: false, Timestamp: now})
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalTurnFlood {
			found = true
		}
	}
	if !found {
		t.Error("expected SignalTurnFlood after 3 non-user turns")
	}
}

func TestTurnFlood_ResetsOnUserTurn(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConsecutiveTurnsWithoutUser = 3
	d := New(cfg)
	now := time.Now()
	d.Feed(TurnSummary{Text: "turn 1", IsUserTurn: false, Timestamp: now})
	d.Feed(TurnSummary{Text: "user input", IsUserTurn: true, Timestamp: now})
	d.Feed(TurnSummary{Text: "turn 2", IsUserTurn: false, Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: "turn 3", IsUserTurn: false, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalTurnFlood {
			t.Error("should not fire — user turn reset the streak, only 2 non-user turns since")
		}
	}
}

// ── Disabled ─────────────────────────────────────────────────────────────────

func TestDisabled_NoVerdicts(t *testing.T) {
	cfg := Config{Enabled: false}
	d := New(cfg)
	now := time.Now()
	for i := 0; i < 10; i++ {
		verdicts := d.Feed(TurnSummary{Text: "same", InputTokens: 100000, Timestamp: now})
		if len(verdicts) != 0 {
			t.Errorf("disabled detector should return no verdicts, got %d", len(verdicts))
		}
	}
}

// ── Reset ────────────────────────────────────────────────────────────────────

func TestReset_ClearsHistory(t *testing.T) {
	d := New(testConfig())
	now := time.Now()
	d.Feed(TurnSummary{Text: "repeat me", Timestamp: now})
	d.Feed(TurnSummary{Text: "repeat me", Timestamp: now})
	d.Reset()
	verdicts := d.Feed(TurnSummary{Text: "repeat me", Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalResponseRepetition {
			t.Error("reset should clear history, preventing repetition detection")
		}
	}
}

// ── Multiple signals ─────────────────────────────────────────────────────────

func TestMultipleSignals_FireTogether(t *testing.T) {
	cfg := testConfig()
	cfg.MaxConsecutiveSimilarResponses = 2
	cfg.TokenVelocityThreshold = 1000
	cfg.ToolEchoThreshold = 2
	d := New(cfg)
	now := time.Now()
	tool := "bash\x00loop"
	d.Feed(TurnSummary{
		Text:         "I will try again",
		InputTokens:  10000,
		OutputTokens: 5000,
		ToolCalls:    []string{tool},
		Timestamp:    now,
	})
	verdicts := d.Feed(TurnSummary{
		Text:         "I will try again",
		InputTokens:  10000,
		OutputTokens: 5000,
		ToolCalls:    []string{tool},
		Timestamp:    now,
	})
	signals := map[Signal]bool{}
	for _, v := range verdicts {
		signals[v.Signal] = true
	}
	if !signals[SignalResponseRepetition] {
		t.Error("expected response repetition")
	}
	if !signals[SignalTokenVelocity] {
		t.Error("expected token velocity")
	}
	if !signals[SignalToolCallEcho] {
		t.Error("expected tool call echo")
	}
}

// ── Chunk Repetition (intra-turn) ────────────────────────────────────────────

func TestChunkRepetition_FiresAfterConsecutiveRepeats(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 5
	d := New(cfg)
	chunk := "I will now run the same command again to verify the output"
	for i := 0; i < 4; i++ {
		d.FeedChunk(chunk)
	}
	verdicts := d.FeedChunk(chunk)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			found = true
			if !v.ShouldInterrupt {
				t.Error("chunk repetition should auto-interrupt")
			}
			if v.Confidence < 0.8 {
				t.Errorf("confidence too low: %f", v.Confidence)
			}
		}
	}
	if !found {
		t.Error("expected SignalChunkRepetition after 5 consecutive identical chunks")
	}
}

func TestChunkRepetition_NoFireForDifferentChunks(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 5
	d := New(cfg)
	d.FeedChunk("first chunk of output from the agent")
	d.FeedChunk("second chunk with different content here")
	verdicts := d.FeedChunk("third chunk also different from the others")
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should not fire for different chunks")
		}
	}
}

func TestChunkRepetition_NoFireForScatteredRepeats(t *testing.T) {
	// The key test: same chunk appears 3 times but NOT consecutively.
	// This should NOT fire — it's normal for multi-file reads to share
	// common phrases like "Let me read" across different contexts.
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	d := New(cfg)
	d.FeedChunk("Let me read the config file first")               // 1st occurrence
	d.FeedChunk("Found 3 sections in the config")                  // different
	d.FeedChunk("Now let me read the tools implementation")        // different
	d.FeedChunk("The tools file defines runBash and runGrep")      // different
	d.FeedChunk("Let me read the status bar code")                 // 2nd occurrence (different text)
	d.FeedChunk("The status bar uses dim() for styling")           // different
	d.FeedChunk("Let me read the header implementation")           // 3rd occurrence (different text)
	verdicts := d.FeedChunk("The header calls styleStatusBarPerm") // 4th
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should not fire for non-consecutive repeats across different contexts")
		}
	}
}

func TestChunkRepetition_NoFireForConsecutiveShortPrefixes(t *testing.T) {
	// Different chunks that share a common prefix but are NOT the same text.
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	d := New(cfg)
	d.FeedChunk("Let me check the config.go file for defaults")
	d.FeedChunk("Let me check the tools.go file for the dispatch")
	d.FeedChunk("Let me check the repl.go file for the handler")
	verdicts := d.FeedChunk("Let me check the status.go file for formatting")
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should not fire — chunks share a prefix but are different text")
		}
	}
}

func TestChunkRepetition_SkipsShortChunks(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	d := New(cfg)
	// Chunks shorter than 5 runes are skipped.
	d.FeedChunk("\n")
	d.FeedChunk("\n")
	verdicts := d.FeedChunk("\n")
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should skip short chunks")
		}
	}
}

func TestChunkRepetition_ResetTurnClearsBuffer(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	d := New(cfg)
	chunk := "repeating phrase that should trigger detection"
	d.FeedChunk(chunk)
	d.FeedChunk(chunk)
	d.FeedChunk(chunk) // fires in first turn
	d.ResetTurn()      // new turn starts — buffer cleared, firedChunk reset
	// After reset, the same chunk can trigger again in the new turn.
	d.FeedChunk(chunk)
	d.FeedChunk(chunk)
	verdicts := d.FeedChunk(chunk)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("ResetTurn should allow detection again in the new turn")
	}
}

func TestChunkRepetition_FiresOnce(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	d := New(cfg)
	chunk := "stuck in a loop repeating this phrase forever and ever"
	d.FeedChunk(chunk)
	d.FeedChunk(chunk)
	d.FeedChunk(chunk) // fires here
	verdicts := d.FeedChunk(chunk)
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should not re-fire within the same turn")
		}
	}
}

func TestChunkRepetition_ConsecutiveCounterResetsOnDifferent(t *testing.T) {
	// 4 identical chunks, then 1 different, then 2 more identical.
	// Should NOT fire — the streak was broken.
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	d := New(cfg)
	chunk := "this is a long enough phrase to not be trimmed away"
	d.FeedChunk(chunk)
	d.FeedChunk(chunk)
	d.FeedChunk(chunk)
	verdicts := d.FeedChunk("something completely different breaks the streak here")
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should not fire — consecutive streak was broken by different chunk")
		}
	}
	// Now continue with the same chunk — only 2 in a row since the break.
	d.FeedChunk(chunk)
	verdicts = d.FeedChunk(chunk)
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			t.Error("should not fire — only 2 consecutive since streak break")
		}
	}
	// One more to make 3 consecutive — should fire.
	verdicts = d.FeedChunk(chunk)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("expected chunk repetition after 3 consecutive (post-break)")
	}
}

// ── Reasoning Chunk Repetition (intra-turn) ──────────────────────────────────

func TestReasoningChunkRepetition_FiresAfterConsecutiveRepeats(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkRepetitionThreshold = 5
	d := New(cfg)
	chunk := "hmm let me reconsider this approach once more carefully"
	for i := 0; i < 4; i++ {
		d.FeedReasoningChunk(chunk)
	}
	verdicts := d.FeedReasoningChunk(chunk)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkRepetition {
			found = true
			if !v.ShouldInterrupt {
				t.Error("reasoning chunk repetition should auto-interrupt")
			}
		}
	}
	if !found {
		t.Error("expected SignalReasoningChunkRepetition after 5 consecutive identical reasoning chunks")
	}
}

func TestReasoningChunkRepetition_NoFireForDifferentChunks(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkRepetitionThreshold = 5
	d := New(cfg)
	d.FeedReasoningChunk("first thought about the problem")
	d.FeedReasoningChunk("second thought, a different angle")
	verdicts := d.FeedReasoningChunk("third thought, yet another angle")
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkRepetition {
			t.Error("should not fire for different reasoning chunks")
		}
	}
}

func TestReasoningChunkRepetition_DefaultIsHigherThanChunkRepetition(t *testing.T) {
	cfg := testConfig()
	if cfg.ReasoningChunkRepetitionThreshold <= cfg.ChunkRepetitionThreshold {
		t.Errorf("expected reasoning threshold (%d) > chunk threshold (%d)",
			cfg.ReasoningChunkRepetitionThreshold, cfg.ChunkRepetitionThreshold)
	}
}

func TestReasoningChunkRepetition_IndependentFromOutputChunks(t *testing.T) {
	// Feeding the same text to FeedChunk and FeedReasoningChunk should track
	// independent buffers/streaks — one firing must not affect the other.
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ReasoningChunkRepetitionThreshold = 3
	d := New(cfg)
	text := "the same phrase fed to both channels for this test"
	d.FeedChunk(text)
	d.FeedChunk(text)
	verdicts := d.FeedReasoningChunk(text)
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkRepetition {
			t.Error("reasoning buffer should not have accumulated the output-channel feeds")
		}
	}
}

func TestReasoningChunkRepetition_ResetTurnClearsBuffer(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkRepetitionThreshold = 3
	d := New(cfg)
	chunk := "repeating reasoning phrase that should trigger detection"
	d.FeedReasoningChunk(chunk)
	d.FeedReasoningChunk(chunk)
	d.FeedReasoningChunk(chunk) // fires in first turn
	d.ResetTurn()
	d.FeedReasoningChunk(chunk)
	d.FeedReasoningChunk(chunk)
	verdicts := d.FeedReasoningChunk(chunk)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("ResetTurn should allow reasoning detection again in the new turn")
	}
}

// ── Reasoning Repetition (cross-turn) ────────────────────────────────────────

func TestReasoningRepetition_NoFireBelowThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningMaxConsecutiveSimilarResponses = 6
	d := New(cfg)
	now := time.Now()
	reasoning := "I should look at the config file to understand defaults"
	for range 4 {
		d.Feed(TurnSummary{ReasoningText: reasoning, Timestamp: now})
	}
	verdicts := d.Feed(TurnSummary{ReasoningText: reasoning, Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalReasoningRepetition {
			t.Error("should not fire below the reasoning repetition threshold")
		}
	}
}

func TestReasoningRepetition_FiresAtThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningMaxConsecutiveSimilarResponses = 3
	d := New(cfg)
	now := time.Now()
	reasoning := "I should reconsider this approach and try again from scratch"
	d.Feed(TurnSummary{ReasoningText: reasoning, Timestamp: now})
	d.Feed(TurnSummary{ReasoningText: reasoning, Timestamp: now})
	verdicts := d.Feed(TurnSummary{ReasoningText: reasoning, Timestamp: now})
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalReasoningRepetition {
			found = true
		}
	}
	if !found {
		t.Error("expected SignalReasoningRepetition after N identical reasoning turns")
	}
}

func TestReasoningRepetition_NoFireWithoutReasoningText(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningMaxConsecutiveSimilarResponses = 3
	d := New(cfg)
	now := time.Now()
	// Non-reasoning turns (e.g. a non-thinking model) must not spuriously fire.
	d.Feed(TurnSummary{Text: "final answer one", Timestamp: now})
	d.Feed(TurnSummary{Text: "final answer two", Timestamp: now})
	verdicts := d.Feed(TurnSummary{Text: "final answer three", Timestamp: now})
	for _, v := range verdicts {
		if v.Signal == SignalReasoningRepetition {
			t.Error("should not fire when turns carry no reasoning text")
		}
	}
}

func TestReasoningRepetition_DefaultIsHigherThanResponseRepetition(t *testing.T) {
	cfg := testConfig()
	if cfg.ReasoningMaxConsecutiveSimilarResponses <= cfg.MaxConsecutiveSimilarResponses {
		t.Errorf("expected reasoning threshold (%d) > response threshold (%d)",
			cfg.ReasoningMaxConsecutiveSimilarResponses, cfg.MaxConsecutiveSimilarResponses)
	}
}

func TestChunkRepetition_WrappingRingBuffer(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkWindowSize = 5 // small buffer to force wrapping
	d := New(cfg)
	// Fill the buffer with different chunks.
	d.FeedChunk("chunk A different from others here")
	d.FeedChunk("chunk B different from others here")
	d.FeedChunk("chunk C different from others here")
	d.FeedChunk("chunk D different from others here")
	d.FeedChunk("chunk E different from others here")
	// Now repeat the same chunk 3 times — it should fire even though
	// the buffer has wrapped and overwritten earlier entries.
	repeat := "this phrase repeats and triggers detection now"
	d.FeedChunk(repeat)
	d.FeedChunk(repeat)
	verdicts := d.FeedChunk(repeat)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("expected chunk repetition to fire after ring buffer wraps")
	}
}
