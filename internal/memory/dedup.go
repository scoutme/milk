package memory

import "strings"

// DuplicateSimilarityThreshold is the Jaccard similarity score above which a
// new percept is considered a near-duplicate of an existing one and will be
// suppressed. Chosen to catch rewordings and subset/superset phrasings while
// allowing genuinely new assertions through.
const DuplicateSimilarityThreshold = 0.60

// DuplicateError is returned by Record and RecordGlobal when the incoming
// content is too similar to an existing percept. The caller can inspect
// Existing to surface a helpful message instead of silently failing.
type DuplicateError struct {
	Existing   Percept
	Similarity float64
}

func (e *DuplicateError) Error() string {
	return "near-duplicate of existing percept " + e.Existing.ID[:8]
}

// IsDuplicate reports whether err is a *DuplicateError (convenience helper for
// callers that want to branch on deduplication without a type assertion).
func IsDuplicate(err error) (*DuplicateError, bool) {
	d, ok := err.(*DuplicateError)
	return d, ok
}

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

// FindSimilar returns the most similar existing Percept whose Jaccard token
// overlap with content exceeds threshold, or nil if none qualifies.
// Searches both global and session stores. The caller must not hold s.mu.
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
		score := jaccardSimilarity(newTokens, tokenize(p.Content))
		if score >= threshold && score > bestScore {
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
