package memory

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// words builds a space-joined string of n unique content tokens (stop-word
// filter leaves these untouched: alphanumeric runs).
func words(prefix string, n int) string {
	parts := make([]string, n)
	for i := range parts {
		parts[i] = fmt.Sprintf("%s%02d", prefix, i)
	}
	return strings.Join(parts, " ")
}

// TestContainmentSimilarity_Basics pins the secondary dedup metric
// (#375 proposal 5): |A ∩ B| / min(|A|, |B|), symmetric, zero on empty input.
func TestContainmentSimilarity_Basics(t *testing.T) {
	a := tokenize("alpha bravo charlie delta")
	subset := tokenize("alpha bravo charlie delta echo foxtrot golf hotel")
	partial := tokenize("alpha bravo india juliet")
	empty := tokenize("")

	if got := containmentSimilarity(a, subset); got != 1.0 {
		t.Errorf("full containment (a ⊂ subset) = %v, want 1.0", got)
	}
	if got := containmentSimilarity(subset, a); got != 1.0 {
		t.Errorf("containment must be symmetric = %v, want 1.0", got)
	}
	if got := containmentSimilarity(a, partial); got != 0.5 {
		t.Errorf("2-of-4 overlap = %v, want 0.5", got)
	}
	if got := containmentSimilarity(a, empty); got != 0 {
		t.Errorf("empty set = %v, want 0", got)
	}
}

// TestFindSimilar_ContainmentSecondaryTrigger: a long verbose restatement
// clears containment (≥0.65) while its Jaccard sits far below 0.60 — the
// shape of the 2026-10-29 eval-run pair (#375: 0.460 Jaccard / 0.68
// containment). Without the secondary trigger, Record would have stored a
// second copy instead of re-affirming.
func TestFindSimilar_ContainmentSecondaryTrigger(t *testing.T) {
	s := newTestStore(t, false)

	// Existing long record: 40 content tokens.
	existing := words("perf", 40)
	if _, err := s.RecordGlobal(context.Background(), existing, ProducerLocal, ConsumerAll, Roles{}); err != nil {
		t.Fatalf("RecordGlobal: %v", err)
	}

	// Restatement: 21 of perf00..perf19+perf20 shared, 9 fresh words →
	// containment 21/30 = 0.70 ≥ 0.65, Jaccard 21/49 ≈ 0.43 < 0.60.
	shared := strings.Fields(existing)[:21]
	restatement := strings.Join(append(shared, strings.Fields(words("fresh", 9))...), " ")

	got := s.FindSimilar(restatement, DuplicateSimilarityThreshold)
	if got == nil {
		t.Fatal("long verbose restatement must match via the containment trigger")
	}
	if got.Content != existing {
		t.Errorf("matched the wrong percept: %q", got.Content)
	}
}

// TestFindSimilar_ContainmentSkipsShortFacts: below containmentMinTokens the
// Jaccard rule alone governs — a short new fact whose every word happens to
// appear inside an existing long record is NOT a duplicate ("keep Jaccard
// as-is for short facts", #375 proposal 5).
func TestFindSimilar_ContainmentSkipsShortFacts(t *testing.T) {
	s := newTestStore(t, false)

	existing := words("perf", 40)
	if _, err := s.RecordGlobal(context.Background(), existing, ProducerLocal, ConsumerAll, Roles{}); err != nil {
		t.Fatalf("RecordGlobal: %v", err)
	}

	// Short fact fully contained in the long record: containment would read
	// 1.0, but with < containmentMinTokens the smaller set must not trigger.
	shortFact := strings.Join(strings.Fields(existing)[:8], " ")
	if got := s.FindSimilar(shortFact, DuplicateSimilarityThreshold); got != nil {
		t.Errorf("short contained fact must not match, got %q", got.Content)
	}
}
