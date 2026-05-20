package memory

import (
	"context"
	"testing"
)

// --- tokenize ---

func TestTokenize_BasicWords(t *testing.T) {
	got := tokenize("user prefers JSON output")
	// "user" and "prefers" and "json" and "output" — no stop-words
	for _, want := range []string{"user", "prefers", "json", "output"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected token %q in result %v", want, got)
		}
	}
}

func TestTokenize_StopWordsExcluded(t *testing.T) {
	got := tokenize("the user wants a flat file")
	for _, stop := range []string{"the", "a"} {
		if _, ok := got[stop]; ok {
			t.Errorf("stop-word %q should be excluded but was in result", stop)
		}
	}
}

func TestTokenize_Punctuation(t *testing.T) {
	got := tokenize("user, prefers: JSON.")
	if _, ok := got["user"]; !ok {
		t.Error("expected 'user' after punctuation stripping")
	}
}

func TestTokenize_CaseInsensitive(t *testing.T) {
	got := tokenize("User PREFERS JSON")
	for _, want := range []string{"user", "prefers", "json"} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected lowercase token %q", want)
		}
	}
}

// --- jaccardSimilarity ---

func TestJaccard_IdenticalSets(t *testing.T) {
	a := tokenize("user prefers JSON output")
	b := tokenize("user prefers JSON output")
	got := jaccardSimilarity(a, b)
	if got != 1.0 {
		t.Errorf("identical sets: want 1.0, got %v", got)
	}
}

func TestJaccard_DisjointSets(t *testing.T) {
	a := tokenize("escalate claude code")
	b := tokenize("flat file output format")
	got := jaccardSimilarity(a, b)
	if got != 0.0 {
		t.Errorf("disjoint sets: want 0.0, got %v", got)
	}
}

func TestJaccard_PartialOverlap(t *testing.T) {
	a := tokenize("escalate to claude when working on milk codebase")
	b := tokenize("escalate to claude when user asks to refactor milk")
	got := jaccardSimilarity(a, b)
	// Both share: escalate, claude, milk, (when, to are stop-words)
	// Rough lower bound: some meaningful overlap
	if got <= 0 || got >= 1 {
		t.Errorf("partial overlap: expected score in (0,1), got %v", got)
	}
}

func TestJaccard_BothEmpty(t *testing.T) {
	got := jaccardSimilarity(map[string]struct{}{}, map[string]struct{}{})
	if got != 0.0 {
		t.Errorf("both empty: want 0.0, got %v", got)
	}
}

// --- Store dedup integration ---

func TestRecord_DuplicateBlocked(t *testing.T) {
	s := newTestStore(t, false)
	original := "user prefers flat file output over JSON"
	id1, err := s.Record(context.Background(), original, ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("first Record failed: %v", err)
	}

	// Near-identical phrasing — should be caught as duplicate
	duplicate := "user prefers flat file output not JSON"
	id2, err := s.Record(context.Background(), duplicate, ProducerUser, ConsumerAll, Roles{}, false)
	if _, ok := IsDuplicate(err); !ok {
		t.Fatalf("expected DuplicateError, got err=%v  id=%s", err, id2)
	}
	// Returns the existing percept's ID
	if id2 != id1 {
		t.Errorf("expected duplicate to return existing id %s, got %s", id1, id2)
	}
	// Store still has only one percept
	if len(s.global.Percepts) != 1 {
		t.Errorf("expected 1 percept after duplicate suppression, got %d", len(s.global.Percepts))
	}
}

func TestRecord_UniquePasses(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "escalate to claude when working on milk", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	_, err := s.Record(context.Background(), "prefer JSON output format for responses", ProducerUser, ConsumerAll, Roles{}, false)
	if _, ok := IsDuplicate(err); ok {
		t.Error("distinct content should not be flagged as duplicate")
	}
	if len(s.global.Percepts) != 2 {
		t.Errorf("expected 2 percepts, got %d", len(s.global.Percepts))
	}
}

func TestRecordGlobal_DuplicateBlocked(t *testing.T) {
	s := newTestStore(t, false)
	original := "escalate to claude code when user asks to design or implement a feature"
	id1, err := s.RecordGlobal(context.Background(), original, ProducerUser, ConsumerAll, Roles{})
	if err != nil {
		t.Fatalf("first RecordGlobal: %v", err)
	}

	// Superseding rule — highly overlapping
	superseding := "escalate to claude code when user asks to design implement refactor or add a feature"
	id2, err := s.RecordGlobal(context.Background(), superseding, ProducerUser, ConsumerAll, Roles{})
	if dup, ok := IsDuplicate(err); !ok {
		t.Fatalf("expected DuplicateError for superseding rule, got err=%v", err)
	} else {
		t.Logf("correctly blocked duplicate (%.0f%% overlap), existing: %s", dup.Similarity*100, dup.Existing.Content)
	}
	if id2 != id1 {
		t.Errorf("expected duplicate to return existing id %s, got %s", id1, id2)
	}
}

func TestRecord_DuplicateError_Fields(t *testing.T) {
	s := newTestStore(t, false)
	original := "always use tabs not spaces for indentation"
	s.Record(context.Background(), original, ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	_, err := s.Record(context.Background(), "always use tabs not spaces", ProducerUser, ConsumerAll, Roles{}, false)
	dup, ok := IsDuplicate(err)
	if !ok {
		t.Fatalf("expected DuplicateError, got %v", err)
	}
	if dup.Existing.Content != original {
		t.Errorf("Existing.Content = %q, want %q", dup.Existing.Content, original)
	}
	if dup.Similarity <= 0 || dup.Similarity > 1 {
		t.Errorf("Similarity out of range: %v", dup.Similarity)
	}
}

func TestFindSimilar_ReturnsNilWhenNoMatch(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "flat file output", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	got := s.FindSimilar("completely unrelated escalation policy", DuplicateSimilarityThreshold)
	if got != nil {
		t.Errorf("expected nil, got %+v", got)
	}
}

func TestFindSimilar_ReturnsMatchAboveThreshold(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "prefer JSON output format", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	got := s.FindSimilar("prefer JSON output", DuplicateSimilarityThreshold)
	if got == nil {
		t.Fatal("expected a match, got nil")
	}
}
