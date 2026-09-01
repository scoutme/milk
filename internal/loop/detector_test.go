package loop

import (
	"fmt"
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

// ── Reasoning Chunk Flood ───────────────────────────────────────────────────

func TestReasoningChunkFlood_FiresAtThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkFloodThreshold = 10
	d := New(cfg)
	// Feed 10 different short reasoning chunks — each unique so consecutive
	// and scattered detectors don't fire, but the flood detector should.
	for i := 0; i < 9; i++ {
		d.FeedReasoningChunk(fmt.Sprintf("thinking about step %d of the problem", i))
	}
	verdicts := d.FeedReasoningChunk("thinking about step 9 of the problem")
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkFlood {
			found = true
			if v.ShouldInterrupt {
				t.Error("reasoning chunk flood should warn only, not auto-interrupt")
			}
		}
	}
	if !found {
		t.Error("expected SignalReasoningChunkFlood after exceeding threshold")
	}
}

func TestReasoningChunkFlood_NoFireBelowThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkFloodThreshold = 10
	d := New(cfg)
	for i := 0; i < 8; i++ {
		d.FeedReasoningChunk(fmt.Sprintf("thinking about step %d of the problem", i))
	}
	verdicts := d.FeedReasoningChunk("thinking about step 8 of the problem")
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkFlood {
			t.Error("should not fire below the flood threshold")
		}
	}
}

func TestReasoningChunkFlood_FiresOnce(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkFloodThreshold = 5
	d := New(cfg)
	for i := 0; i < 4; i++ {
		d.FeedReasoningChunk(fmt.Sprintf("chunk %d", i))
	}
	d.FeedReasoningChunk("chunk 4") // fires here
	// Continue feeding — should NOT re-fire.
	verdicts := d.FeedReasoningChunk("chunk 5")
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkFlood {
			t.Error("should not re-fire within the same turn")
		}
	}
}

func TestReasoningChunkFlood_ResetTurnResetsCounter(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkFloodThreshold = 5
	d := New(cfg)
	for i := 0; i < 4; i++ {
		d.FeedReasoningChunk(fmt.Sprintf("chunk %d", i))
	}
	d.FeedReasoningChunk("chunk 4") // fires in first turn
	d.ResetTurn()
	// After reset, counter should be back to 0 — 3 more chunks should not fire.
	for i := 0; i < 3; i++ {
		d.FeedReasoningChunk(fmt.Sprintf("new chunk %d", i))
	}
	verdicts := d.FeedReasoningChunk("new chunk 3")
	for _, v := range verdicts {
		if v.Signal == SignalReasoningChunkFlood {
			t.Error("ResetTurn should reset the flood counter")
		}
	}
}

// ── Scattered Chunk Repetition (intra-turn, non-adjacent) ───────────────────

func TestScatteredChunkRepetition_FiresOnLongPhraseWithGaps(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 20
	d := New(cfg)
	phrase := "I need to check the DNS records again to be sure"
	d.FeedChunk(phrase)
	d.FeedChunk("some unrelated content in between the repeats")
	d.FeedChunk(phrase)
	d.FeedChunk("more unrelated content here as well")
	verdicts := d.FeedChunk(phrase)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			found = true
			if !v.ShouldInterrupt {
				t.Error("scattered chunk repetition should auto-interrupt")
			}
		}
	}
	if !found {
		t.Error("expected SignalScatteredChunkRepetition after 3 non-adjacent identical long chunks")
	}
}

func TestScatteredChunkRepetition_NoFireForShortPhraseWithGaps(t *testing.T) {
	// Below ChunkRepetitionMinScatteredLength — must not fire even with
	// enough non-adjacent repeats, since short phrases recur legitimately.
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 40
	d := New(cfg)
	phrase := "Let me read the file"
	d.FeedChunk(phrase)
	d.FeedChunk("totally different content here")
	d.FeedChunk(phrase)
	d.FeedChunk("more different content here")
	verdicts := d.FeedChunk(phrase)
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			t.Error("should not fire for short phrases below the min scattered length")
		}
	}
}

func TestScatteredChunkRepetition_NoFireBelowThreshold(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 4
	cfg.ChunkRepetitionMinScatteredLength = 20
	d := New(cfg)
	phrase := "I need to check the DNS records again to be sure"
	d.FeedChunk(phrase)
	d.FeedChunk("some unrelated content in between the repeats")
	verdicts := d.FeedChunk(phrase)
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			t.Error("should not fire below the occurrence threshold")
		}
	}
}

func TestScatteredChunkRepetition_FiresOnce(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 20
	d := New(cfg)
	phrase := "I need to check the DNS records again to be sure"
	d.FeedChunk(phrase)
	d.FeedChunk("filler")
	d.FeedChunk(phrase)
	d.FeedChunk("filler")
	d.FeedChunk(phrase) // fires here
	verdicts := d.FeedChunk(phrase)
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			t.Error("should not re-fire for the same text within the same turn")
		}
	}
}

func TestScatteredChunkRepetition_DistinctTextsBothFire(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 20
	d := New(cfg)
	phraseA := "I need to check the DNS records again to be sure"
	phraseB := "The Cloudflare tunnel configuration looks correct"
	d.FeedChunk(phraseA)
	d.FeedChunk(phraseB)
	d.FeedChunk(phraseA)
	d.FeedChunk(phraseB)
	d.FeedChunk(phraseA) // phraseA fires here
	verdicts := d.FeedChunk(phraseB)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("expected phraseB to independently fire scattered repetition")
	}
}

func TestScatteredChunkRepetition_ResetTurnClearsState(t *testing.T) {
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 20
	d := New(cfg)
	phrase := "I need to check the DNS records again to be sure"
	d.FeedChunk(phrase)
	d.FeedChunk("filler")
	d.FeedChunk(phrase)
	d.FeedChunk("filler")
	d.FeedChunk(phrase) // fires
	d.ResetTurn()
	d.FeedChunk(phrase)
	d.FeedChunk("filler")
	d.FeedChunk(phrase)
	d.FeedChunk("filler")
	verdicts := d.FeedChunk(phrase)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("ResetTurn should allow scattered detection again in the new turn")
	}
}

func TestScatteredChunkRepetition_EvictsOldEntriesFromWindow(t *testing.T) {
	// Small window: once the repeated phrase's earlier occurrences are
	// overwritten by unrelated content, the count must drop accordingly.
	cfg := testConfig()
	cfg.ChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 20
	cfg.ChunkWindowSize = 4
	d := New(cfg)
	phrase := "I need to check the DNS records again to be sure"
	d.FeedChunk(phrase)                                               // slot 0
	d.FeedChunk("filler one two three four five six seven eight")     // slot 1
	d.FeedChunk(phrase)                                               // slot 2 — count 2
	d.FeedChunk("filler nine ten eleven twelve thirteen fourteen")    // slot 3 — wraps next write to slot 0
	d.FeedChunk("filler fifteen sixteen seventeen eighteen nineteen") // slot 0 — evicts first phrase occurrence, count back to 1
	verdicts := d.FeedChunk(phrase)                                   // slot 1 — count 2, still below threshold 3
	for _, v := range verdicts {
		if v.Signal == SignalScatteredChunkRepetition {
			t.Error("should not fire — window eviction dropped the count below threshold")
		}
	}
}

func TestReasoningScatteredChunkRepetition_FiresWithGaps(t *testing.T) {
	cfg := testConfig()
	cfg.ReasoningChunkRepetitionThreshold = 3
	cfg.ChunkRepetitionMinScatteredLength = 20
	d := New(cfg)
	phrase := "hmm let me reconsider this approach once more carefully"
	d.FeedReasoningChunk(phrase)
	d.FeedReasoningChunk("a different thought entirely about the problem")
	d.FeedReasoningChunk(phrase)
	d.FeedReasoningChunk("another different thought about the problem")
	verdicts := d.FeedReasoningChunk(phrase)
	found := false
	for _, v := range verdicts {
		if v.Signal == SignalScatteredReasoningChunkRepetition {
			found = true
		}
	}
	if !found {
		t.Error("expected SignalScatteredReasoningChunkRepetition after 3 non-adjacent identical long reasoning chunks")
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
