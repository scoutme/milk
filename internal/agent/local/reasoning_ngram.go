package local

import (
	"regexp"
	"strings"
)

// numRe matches standalone integers and floats so the tokenizer can
// normalise them to a placeholder.  This lets the detector catch
// patterns like "line 1517 … correct" / "line 1523 … correct" where
// the structure repeats but the numbers change each cycle.
var numRe = regexp.MustCompile(`\b\d+(?:\.\d+)?\b`)

// reasoningNgramMonitor detects periodic repetition in streaming reasoning
// content.  It accumulates tokens in a sliding window and checks for blocks
// of tokens that repeat consecutively (e.g. "I'm done → let me check → I'm
// done → let me check").  This catches the "conclude → doubt → re-conclude"
// loop that reasoning models like mimo-v2.5-pro fall into, even when each
// cycle has slightly different wording.
//
// Ported from MiMo-Code's TextNgramMonitor (text-ngram-detection.ts).
type reasoningNgramMonitor struct {
	tokens               []string
	windowSize           int
	blockSize            int // min period length in tokens
	consecutiveThreshold int // back-to-back repetitions to trigger detectConsecutiveRepeat
	spacedThreshold      int // occurrences within maxSpan to trigger detectRepeatedNgram
	minDistinct          int // min distinct tokens in a repeating block
	maxSpan              int // max token distance between first and last occurrence for non-consecutive detection
}

const (
	defaultNgramWindowSize = 1000
	defaultNgramBlockSize  = 4
	// defaultNgramConsecutiveThreshold is low: back-to-back repetition is an
	// unambiguous signal (no legitimate reason for the same block to repeat
	// immediately), so it should fire fast to bound wasted tokens.
	defaultNgramConsecutiveThreshold = 5
	// defaultNgramSpacedThreshold is high, matching MiMo-Code's
	// Flag.MIMOCODE_TEXT_REPEAT_THRESHOLD: occurrences spread across the
	// window are a weaker signal (a phrase can legitimately recur many
	// times while the model makes real progress), so it needs more
	// repetitions — bounded by maxSpan — before it's treated as a loop.
	defaultNgramSpacedThreshold = 20
	defaultNgramMinDistinct     = 3
	defaultNgramMaxSpan         = 200 // max token distance between first and last occurrence
)

func newReasoningNgramMonitor() *reasoningNgramMonitor {
	return &reasoningNgramMonitor{
		windowSize:           defaultNgramWindowSize,
		blockSize:            defaultNgramBlockSize,
		consecutiveThreshold: defaultNgramConsecutiveThreshold,
		spacedThreshold:      defaultNgramSpacedThreshold,
		minDistinct:          defaultNgramMinDistinct,
		maxSpan:              defaultNgramMaxSpan,
	}
}

// tokenize splits text into lowercase tokens for n-gram analysis.
// Numbers (integers and floats) are replaced with a <NUM> placeholder
// so that structurally identical reasoning with different numeric
// literals (line counts, coordinates, cooldown values, …) still
// matches as the same repeating block.
func tokenize(text string) []string {
	text = strings.ToLower(text)
	text = numRe.ReplaceAllString(text, "<NUM>")
	text = strings.Join(strings.Fields(text), " ")
	parts := strings.Split(text, " ")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Feed appends new reasoning text and returns true if repetition is
// detected within the sliding window.  Two detection methods are used:
//  1. detectConsecutiveRepeat — catches back-to-back periodic blocks
//     (e.g. "A B C D A B C D A B C D")
//  2. detectRepeatedNgram — catches the same n-gram appearing multiple
//     times anywhere in the window, even with other content between
//     occurrences (e.g. "A B C ... X Y Z ... A B C ... P Q R ... A B C")
func (m *reasoningNgramMonitor) Feed(text string) bool {
	if text == "" {
		return false
	}
	newTokens := tokenize(text)
	m.tokens = append(m.tokens, newTokens...)
	if len(m.tokens) > m.windowSize {
		m.tokens = m.tokens[len(m.tokens)-m.windowSize:]
	}
	return detectConsecutiveRepeat(m.tokens, m.blockSize, m.consecutiveThreshold, m.minDistinct) ||
		detectRepeatedNgram(m.tokens, m.blockSize, m.spacedThreshold, m.maxSpan)
}

// Reset clears the accumulated tokens.
func (m *reasoningNgramMonitor) Reset() {
	m.tokens = nil
}

// detectConsecutiveRepeat searches for a block of tokens that repeats
// consecutively.  It tries all period lengths p from minBlockSize to
// len(tokens)/2.  For each period, it counts how many consecutive
// positions i satisfy tokens[i] == tokens[i+p].  When a run of
// p*(threshold-1) matches is found and the block contains at least
// minDistinct unique tokens, it returns true.
//
// This catches patterns like:
//
//	A B C D A B C D A B C D   (period=4, threshold=3)
//
// even when the blocks are not byte-identical (they are token-identical).
func detectConsecutiveRepeat(tokens []string, minBlockSize, threshold, minDistinct int) bool {
	if threshold < 2 || len(tokens) < minBlockSize*threshold {
		return false
	}
	// Allow detecting blocks up to half the window size.  The original
	// len(tokens)/threshold limit was too restrictive for large repeating
	// blocks (e.g. a 200-token block repeating 3x in a 500-token window).
	maxPeriod := len(tokens) / 2
	for p := minBlockSize; p <= maxPeriod; p++ {
		run := 0
		for i := 0; i <= len(tokens)-p-1; i++ {
			if tokens[i] == tokens[i+p] {
				run++
				if run >= p*(threshold-1) {
					blockStart := i - run + 1
					distinct := make(map[string]struct{}, p)
					for _, t := range tokens[blockStart : blockStart+p] {
						distinct[t] = struct{}{}
					}
					if len(distinct) >= minDistinct {
						return true
					}
					run = 0
				}
			} else {
				run = 0
			}
		}
	}
	return false
}

// detectRepeatedNgram checks whether any n-gram appears at least threshold
// times within a maxSpan-wide sub-window.  Unlike a raw occurrence counter,
// this prevents false positives when a phrase legitimately appears many times
// spread across a large reasoning window (e.g. "terrain mesh" referenced in
// different contexts 200+ tokens apart).
//
// For each n-gram, the last threshold positions are tracked.  When the span
// between the oldest and newest of those positions is ≤ maxSpan, the
// repetitions are close enough to constitute a real loop.
func detectRepeatedNgram(tokens []string, n, threshold, maxSpan int) bool {
	if len(tokens) < n || threshold < 2 {
		return false
	}
	positions := make(map[string][]int)
	for i := 0; i <= len(tokens)-n; i++ {
		key := strings.Join(tokens[i:i+n], "\x00")
		pos := positions[key]
		pos = append(pos, i)
		// Keep only the last threshold positions to bound memory.
		if len(pos) > threshold {
			pos = pos[len(pos)-threshold:]
		}
		positions[key] = pos
		if len(pos) >= threshold {
			if pos[len(pos)-1]-pos[0] <= maxSpan {
				return true
			}
		}
	}
	return false
}
