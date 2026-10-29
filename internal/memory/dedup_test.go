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

// --- Store dedup integration (ADR-0053: near-duplicates reinforce) ---

func TestRecord_DuplicateReinforces(t *testing.T) {
	s := newTestStore(t, false)
	original := "user prefers flat file output over JSON"
	out1, err := s.Record(context.Background(), original, ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("first Record failed: %v", err)
	}

	// Near-identical phrasing — should reinforce, not insert.
	duplicate := "user prefers flat file output not JSON"
	out2, err := s.Record(context.Background(), duplicate, ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("reinforcing Record failed: %v", err)
	}
	if !out2.Reinforced {
		t.Fatalf("expected reinforcement, got insert (id=%s)", out2.ID)
	}
	// Returns the existing percept's ID
	if out2.ID != out1.ID {
		t.Errorf("expected duplicate to return existing id %s, got %s", out1.ID, out2.ID)
	}
	if out2.Existing != original {
		t.Errorf("Expected existing content %q, got %q", original, out2.Existing)
	}
	if out2.Similarity <= 0 || out2.Similarity > 1 {
		t.Errorf("Similarity out of range: %v", out2.Similarity)
	}
	// Store still has only one percept
	if len(s.global.Percepts) != 1 {
		t.Errorf("expected 1 percept after reinforcement, got %d", len(s.global.Percepts))
	}
	// Reinforcement never lowers W: this percept (born at 0.9 via
	// ProducerUser) is already above the reconfirm cap, so it keeps its
	// weight; the boost trajectory is covered by
	// TestRecord_ReinforceCapAndPromotionPath.
	if got := s.global.Percepts[0].W; got < out1.W {
		t.Errorf("reinforcement must not lower W: before %v, after %v", out1.W, got)
	}
	if out2.W != s.global.Percepts[0].W {
		t.Errorf("outcome W %v != store W %v", out2.W, s.global.Percepts[0].W)
	}
}

func TestRecord_ReinforceCapAndPromotionPath(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	content := "milk routes coding tasks to the local model"

	// Inference percept starts at 0.7 (initialWeight) — cannot be promoted by
	// decay/edges alone; corroboration in distinct runs is its promotion path
	// (ADR-0091: W = 0.7 + corrobBoost(n), saturating at the reconfirm cap).
	s, err := NewStoreParams(dir, "", Params{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	freezeClock(s)
	out, err := s.Record(ctx, content, ProducerLocal, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}
	if out.W != 0.7 {
		t.Fatalf("expected initial W 0.7, got %v", out.W)
	}

	// Echo-proof: six re-statements in ONE run earn one corroboration and
	// nothing more — repetition is not corroboration.
	for i := 0; i < 6; i++ {
		out, err = s.Record(ctx, content, ProducerLocal, ConsumerAll, Roles{}, false)
		if err != nil {
			t.Fatalf("reinforce %d failed: %v", i, err)
		}
		if !out.Reinforced {
			t.Fatalf("reinforce %d: expected reinforcement", i)
		}
	}
	if !closeTo(out.W, 0.7+corrobBoost(1)) {
		t.Errorf("in-run repetition: W = %v, want %v (one event per run)", out.W, 0.7+corrobBoost(1))
	}

	// Distinct runs are the promotion path: each re-opening of the store
	// earns one more event and W climbs the saturating corroboration series.
	prev := out.W
	for run := 2; run <= 6; run++ {
		s, err = NewStoreParams(dir, "", Params{})
		if err != nil {
			t.Fatalf("reopen run %d: %v", run, err)
		}
		freezeClock(s)
		out, err = s.Record(ctx, content, ProducerLocal, ConsumerAll, Roles{}, false)
		if err != nil {
			t.Fatalf("run %d reinforce: %v", run, err)
		}
		if out.W <= prev {
			t.Errorf("run %d: W = %v, must climb across runs (was %v)", run, out.W, prev)
		}
		if out.W > reconfirmCap {
			t.Errorf("run %d: W = %v, must never pass the reconfirm cap %v", run, out.W, reconfirmCap)
		}
		prev = out.W
	}
	if prev < promoteThreshold {
		t.Errorf("reinforcement trajectory must reach promoteThreshold %v, got %v", promoteThreshold, prev)
	}
	if prev > reconfirmCap || prev < reconfirmCap-0.05 {
		t.Errorf("repeated corroboration converges on the cap %v, got %v", reconfirmCap, prev)
	}
}

func TestRecord_UniquePasses(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "escalate to claude when working on milk", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	out, err := s.Record(context.Background(), "prefer JSON output format for responses", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record failed: %v", err)
	}
	if out.Reinforced {
		t.Error("distinct content should not be reinforced")
	}
	if len(s.global.Percepts) != 2 {
		t.Errorf("expected 2 percepts, got %d", len(s.global.Percepts))
	}
}

func TestRecordGlobal_DuplicateReinforces(t *testing.T) {
	s := newTestStore(t, false)
	original := "escalate to claude code when user asks to design or implement a feature"
	out1, err := s.RecordGlobal(context.Background(), original, ProducerUser, ConsumerAll, Roles{})
	if err != nil {
		t.Fatalf("first RecordGlobal: %v", err)
	}

	// Superseding rule — highly overlapping
	superseding := "escalate to claude code when user asks to design implement refactor or add a feature"
	out2, err := s.RecordGlobal(context.Background(), superseding, ProducerUser, ConsumerAll, Roles{})
	if err != nil {
		t.Fatalf("reinforcing RecordGlobal failed: %v", err)
	}
	if !out2.Reinforced {
		t.Fatalf("expected reinforcement for superseding rule, got insert")
	}
	if out2.ID != out1.ID {
		t.Errorf("expected duplicate to return existing id %s, got %s", out1.ID, out2.ID)
	}
	t.Logf("reinforced duplicate (%.0f%% overlap), existing: %s", out2.Similarity*100, out2.Existing)
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
