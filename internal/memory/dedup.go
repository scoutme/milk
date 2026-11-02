package memory

import "strings"

// DuplicateSimilarityThreshold is the Jaccard similarity score above which a
// new percept is considered a near-duplicate of an existing one. Instead of
// inserting a second copy, Record/RecordGlobal re-affirm the existing percept
// (ADR-0053/0091: a re-stated fact is corroborated — one event per run, W
// refreshed to its derived strength — not rejected). Chosen to catch
// rewordings and subset/superset phrasings while allowing genuinely new
// assertions through.
const DuplicateSimilarityThreshold = 0.60

// DuplicateContainmentThreshold is the secondary dedup trigger (issue #375,
// verdict proposal 5): token containment |A ∩ B| / min(|A|, |B|) between a
// new record and an existing Percept. Sentence-level Jaccard is structurally
// weak for long, verbose records — the two "full-3way eval run" percepts of
// 2026-10-29 scored only 0.460 because their per-run detail fields (times,
// judge, arm names) dominated the union — while containment reads 0.68 for
// the same pair: the shared event wording clearly dominates the smaller set.
// Jaccard stays the primary signal; containment only *adds* matches.
const DuplicateContainmentThreshold = 0.65

// containmentMinTokens is the minimum size of the smaller token set for the
// containment trigger to apply. Containment is meant for long verbose
// records; on small sets it would swallow genuinely new short facts whose
// every word happens to appear inside an existing longer record (subset
// phrasing through Jaccard is unchanged below this size — "keep Jaccard as
// is for short facts", #375 proposal 5).
const containmentMinTokens = 20

// tokenize lower-cases s, strips punctuation, and returns the unique word set.
// Common stop-words are excluded so content-bearing words dominate the overlap.
func tokenize(s string) map[string]struct{} {
	tokens := make(map[string]struct{})
	var buf strings.Builder
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z':
			buf.WriteRune(r)
		case r >= '0' && r <= '9':
			buf.WriteRune(r)
		default:
			if buf.Len() > 0 {
				w := buf.String()
				buf.Reset()
				if !stopWord[w] {
					tokens[w] = struct{}{}
				}
			}
		}
	}
	if buf.Len() > 0 {
		w := buf.String()
		if !stopWord[w] {
			tokens[w] = struct{}{}
		}
	}
	return tokens
}

// jaccardSimilarity returns |A ∩ B| / |A ∪ B| for two token sets.
// Returns 0 when both sets are empty.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	// Count intersection
	var intersection int
	for t := range a {
		if _, ok := b[t]; ok {
			intersection++
		}
	}
	union := len(a) + len(b) - intersection
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// containmentSimilarity returns |A ∩ B| / min(|A|, |B|) for two token sets —
// how much of the *smaller* set the larger one contains. Returns 0 when either
// set is empty.
func containmentSimilarity(a, b map[string]struct{}) float64 {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	if n == 0 {
		return 0
	}
	var intersection int
	for t := range a {
		if _, ok := b[t]; ok {
			intersection++
		}
	}
	return float64(intersection) / float64(n)
}

// FindSimilar returns the most similar existing Percept whose overlap with
// content qualifies — Jaccard ≥ threshold, or (for long verbose records,
// see containmentMinTokens) containment ≥ DuplicateContainmentThreshold —
// or nil if none qualifies. Candidates are ranked by their best qualifying
// score. Searches both global and session stores. The caller must not hold s.mu.
func (s *Store) FindSimilar(content string, threshold float64) *Percept {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.findSimilarLocked(content, threshold)
}

// findSimilarLocked is the lock-free inner search used inside Record/RecordGlobal.
// Caller must hold s.mu.
func (s *Store) findSimilarLocked(content string, threshold float64) *Percept {
	newTokens := tokenize(content)
	if len(newTokens) == 0 {
		return nil
	}

	var best *Percept
	var bestScore float64

	check := func(p Percept) {
		pTokens := tokenize(p.Content)
		// Primary: Jaccard against the caller's threshold.
		score := jaccardSimilarity(newTokens, pTokens)
		qualified := score >= threshold
		// Secondary containment trigger (#375): long verbose records with a
		// small shared core score poorly on Jaccard but clearly on
		// containment — qualifies on its own threshold, not the Jaccard one.
		n := len(newTokens)
		if len(pTokens) < n {
			n = len(pTokens)
		}
		if n >= containmentMinTokens {
			if c := containmentSimilarity(newTokens, pTokens); c >= DuplicateContainmentThreshold {
				if c > score {
					score = c
				}
				qualified = true
			}
		}
		if qualified && score > bestScore {
			bestScore = score
			cp := p
			best = &cp
		}
	}

	for _, p := range s.global.Percepts {
		check(p)
	}
	for _, p := range s.session.Percepts {
		check(p)
	}
	return best
}

// stopWord is a minimal English stop-word set. These words are excluded from
// token overlap so that short functional words don't inflate similarity.
var stopWord = map[string]bool{
	"a": true, "an": true, "and": true, "are": true, "as": true, "at": true,
	"be": true, "been": true, "but": true, "by": true, "do": true,
	"for": true, "from": true, "has": true, "have": true, "he": true,
	"her": true, "his": true, "i": true, "if": true, "in": true,
	"is": true, "it": true, "its": true, "me": true, "my": true,
	"no": true, "not": true, "of": true, "on": true, "or": true,
	"our": true, "out": true, "so": true, "that": true, "the": true,
	"their": true, "them": true, "then": true, "there": true, "they": true,
	"this": true, "to": true, "up": true, "us": true, "was": true,
	"we": true, "when": true, "which": true, "who": true, "will": true,
	"with": true, "you": true, "your": true,
}
