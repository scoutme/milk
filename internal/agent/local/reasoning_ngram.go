package local

import "strings"

// reasoningNgramMonitor detects periodic repetition in streaming reasoning
// content.  It accumulates tokens in a sliding window and checks for blocks
// of tokens that repeat consecutively (e.g. "I'm done → let me check → I'm
// done → let me check").  This catches the "conclude → doubt → re-conclude"
// loop that reasoning models like mimo-v2.5-pro fall into, even when each
// cycle has slightly different wording.
//
// Ported from MiMo-Code's TextNgramMonitor (text-ngram-detection.ts).
type reasoningNgramMonitor struct {
	tokens      []string
	windowSize  int
	blockSize   int // min period length in tokens
	threshold   int // consecutive repetitions to trigger
	minDistinct int // min distinct tokens in a repeating block
}

const (
	defaultNgramWindowSize  = 1000
	defaultNgramBlockSize   = 4
	defaultNgramThreshold   = 3
	defaultNgramMinDistinct = 3
)

func newReasoningNgramMonitor() *reasoningNgramMonitor {
	return &reasoningNgramMonitor{
		windowSize:  defaultNgramWindowSize,
		blockSize:   defaultNgramBlockSize,
		threshold:   defaultNgramThreshold,
		minDistinct: defaultNgramMinDistinct,
	}
}

// tokenize splits text into lowercase tokens for n-gram analysis.
func tokenize(text string) []string {
	text = strings.ToLower(text)
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

// Feed appends new reasoning text and returns true if periodic repetition is
// detected within the sliding window.
func (m *reasoningNgramMonitor) Feed(text string) bool {
	if text == "" {
		return false
	}
	newTokens := tokenize(text)
	m.tokens = append(m.tokens, newTokens...)
	if len(m.tokens) > m.windowSize {
		m.tokens = m.tokens[len(m.tokens)-m.windowSize:]
	}
	return detectConsecutiveRepeat(m.tokens, m.blockSize, m.threshold, m.minDistinct)
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
