package memory

import (
	"context"
	"math"
	"testing"
	"time"
)

func TestConsolidate_NoSessionIsNoop(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "global fact", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate with no session: %v", err)
	}
	// Global store untouched
	if len(s.global.Percepts) != 1 {
		t.Errorf("expected 1 global percept, got %d", len(s.global.Percepts))
	}
}

func TestConsolidate_DecaysByRunningTime(t *testing.T) {
	s := newTestStore(t, true)
	s.Record(context.Background(), "decayable fact", ProducerLocal, ConsumerAll, Roles{}, false) //nolint:errcheck

	// One session half-life (3 running days) of milk running time: 0.7 · 0.5.
	advanceClock(s, time.Duration(sessionHalfLifeDays)*24*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, p := range s.session.Percepts {
		if p.Content == "decayable fact" {
			if !closeTo(p.W, 0.35) {
				t.Errorf("W = %v, want 0.35 (0.7 halved over one session half-life)", p.W)
			}
			return
		}
	}
	t.Error("percept should survive consolidation at W=0.35 (> pruneThreshold 0.20)")
}

func TestConsolidate_DecayIgnoresPassCount(t *testing.T) {
	s := newTestStore(t, true)
	s.Record(context.Background(), "turn-count invariant", ProducerLocal, ConsumerAll, Roles{}, false) //nolint:errcheck

	// Chatty sessions must not accelerate forgetting: ten consolidations at
	// the same instant decay nothing.
	for i := 0; i < 10; i++ {
		if err := s.Consolidate(); err != nil {
			t.Fatalf("Consolidate %d: %v", i, err)
		}
	}
	for _, p := range s.session.Percepts {
		if !closeTo(p.W, 0.7) {
			t.Errorf("W = %v, want 0.7 (no running time elapsed)", p.W)
		}
	}
}

func TestConsolidate_CoreExemptsFromDecay(t *testing.T) {
	s := newTestStore(t, true)
	// Core percepts go to global, not session — record directly in session for test
	p := Percept{
		ID:       "core-1",
		Content:  "core fact",
		Producer: ProducerUser,
		W:        1.0,
		Core:     true,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	advanceClock(s, 30*24*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// Core percept should survive with W unchanged
	all := append(s.session.Percepts, s.global.Percepts...) //nolint:gocritic
	for _, rp := range all {
		if rp.ID == "core-1" {
			if rp.W != 1.0 {
				t.Errorf("core percept W changed: got %v", rp.W)
			}
			return
		}
	}
	t.Error("core percept disappeared after consolidation")
}

func TestConsolidate_PrunesZeroWeight(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{
		ID:       "low-1",
		Content:  "low weight fact",
		Producer: ProducerSystem,
		W:        0.0,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.session.Percepts {
		if rp.ID == "low-1" {
			t.Error("zero-weight percept should have been pruned")
		}
	}
}

func TestConsolidate_PromotesUserFastPath(t *testing.T) {
	s := newTestStore(t, true)
	// ProducerUser facts are explicit assertions: W 0.9 >= promoteThreshold
	// graduates on the first pass, Evidence or no Evidence (ADR-0090).
	s.Record(context.Background(), "user stated fact", ProducerUser, ConsumerAll, Roles{}, false) //nolint:errcheck

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.global.Percepts {
		if rp.Content == "user stated fact" {
			return // promoted — correct
		}
	}
	t.Error("user percept (W=0.9) should promote on the fast path")
}

func TestConsolidate_PromotesCorroboratedInference(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{
		ID:       "corr-1",
		Content:  "corroborated inference",
		Producer: ProducerLocal,
		W:        0.85,
		Evidence: evidencePromote, // corroborated in two distinct runs
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.global.Percepts {
		if rp.ID == "corr-1" {
			return // promoted
		}
	}
	t.Error("inference percept with Evidence 2 at W 0.85 should be promoted")
}

func TestConsolidate_DefersUnCorroboratedInference(t *testing.T) {
	s := newTestStore(t, true)
	// W alone is not "stable": one sitting can mint Evidence 1 at most, and
	// that is not enough to graduate (ADR-0090).
	p := Percept{
		ID:       "lonely-1",
		Content:  "single-run inference",
		Producer: ProducerLocal,
		W:        0.85,
		Evidence: 1,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.global.Percepts {
		if rp.ID == "lonely-1" {
			t.Error("Evidence 1 must not promote off the fast path")
			return
		}
	}
	found := false
	for _, rp := range s.session.Percepts {
		if rp.ID == "lonely-1" {
			found = true
		}
	}
	if !found {
		t.Error("held-back percept should stay in the session store")
	}
}

func TestConsolidate_DefersContradictedFact(t *testing.T) {
	s := newTestStore(t, true)
	a, err := s.Record(context.Background(), "the api listens on port 8080", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	b, err := s.Record(context.Background(), "workers run batch jobs on port 9090 instead", ProducerLocal, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if a.ID == b.ID {
		t.Fatal("test setup: percepts must be distinct (no reinforcement)")
	}
	if err := s.AddEdge(a.ID, b.ID, RelationContradicts); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// The user fact (fast path) is blocked by the live contradicts edge; the
	// inference fact both lacks Evidence and is contradicted.
	for _, rp := range s.global.Percepts {
		if rp.ID == a.ID || rp.ID == b.ID {
			t.Error("contradicted percepts must not promote until the conflict is resolved")
		}
	}
}

func TestConsolidate_PromotesAfterSecondRunCorroboration(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	content := "the deploy target is a k3s cluster"

	// Run 1: stated and re-stated — Evidence 1 at most, W 0.7 → 0.75.
	s1, err := NewStoreParams(dir, "sess", Params{})
	if err != nil {
		t.Fatalf("run 1 open: %v", err)
	}
	freezeClock(s1)
	first, err := s1.Record(ctx, content, ProducerLocal, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("run 1 Record: %v", err)
	}
	if out, err := s1.Record(ctx, content, ProducerLocal, ConsumerAll, Roles{}, false); err != nil || !out.Reinforced {
		t.Fatalf("run 1 reinforce: %+v, %v", out, err)
	}
	if err := s1.Consolidate(); err != nil {
		t.Fatalf("run 1 Consolidate: %v", err)
	}
	if len(s1.global.Percepts) != 0 {
		t.Fatalf("run 1 must not promote (Evidence 1), got %v", s1.global.Percepts)
	}

	// Run 2: a new Store lifetime over the same files — one more restatement
	// is the second distinct run: W 0.80, Evidence 2 → graduates.
	s2, err := NewStoreParams(dir, "sess", Params{})
	if err != nil {
		t.Fatalf("run 2 open: %v", err)
	}
	freezeClock(s2)
	if out, err := s2.Record(ctx, content, ProducerLocal, ConsumerAll, Roles{}, false); err != nil || !out.Reinforced {
		t.Fatalf("run 2 reinforce: %+v, %v", out, err)
	}
	if err := s2.Consolidate(); err != nil {
		t.Fatalf("run 2 Consolidate: %v", err)
	}
	for _, rp := range s2.global.Percepts {
		if rp.ID == first.ID {
			if rp.Evidence != 2 {
				t.Errorf("promoted percept Evidence = %d, want 2", rp.Evidence)
			}
			return // promoted
		}
	}
	t.Error("percept corroborated in two runs (W 0.80, Evidence 2) should promote")
}

func TestDecayPercepts_SessionHalving(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-72 * time.Hour) // one session half-life of running time
	percepts := []Percept{
		{ID: "1", W: 0.7, UpdatedAt: since.Add(-time.Hour)},
		{ID: "2", W: 1.0, Core: true},
		{ID: "3", W: 0.4, UpdatedAt: since.Add(-time.Hour)},
	}
	n := decayPercepts(percepts, now, since, sessionHalfLifeDays, 0)
	if n != 2 {
		t.Errorf("expected 2 decayed, got %d", n)
	}
	if !closeTo(percepts[0].W, 0.35) {
		t.Errorf("percept 1 W = %v, want 0.35", percepts[0].W)
	}
	if percepts[1].W != 1.0 {
		t.Errorf("core percept should not decay, got %v", percepts[1].W)
	}
	if !closeTo(percepts[2].W, 0.20) {
		t.Errorf("percept 3 W = %v, want 0.20", percepts[2].W)
	}
}

func TestDecayPercepts_ReconfirmationRestartsClock(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-72 * time.Hour)
	// Restated mid-interval: only the remainder decays, not the whole pass.
	percepts := []Percept{
		{ID: "fresh", W: 0.80, UpdatedAt: now.Add(-time.Hour)},
	}
	if n := decayPercepts(percepts, now, since, sessionHalfLifeDays, 0); n != 1 {
		t.Fatalf("expected 1 decayed, got %d", n)
	}
	// 0.80 · 2^(−1h/72h), comfortably above the 0.80 a full-pass decay
	// would have shaved below.
	if percepts[0].W <= 0.80*math.Pow(0.5, 24.0/(sessionHalfLifeDays*24)) {
		t.Error("a mid-interval re-confirmation must not be decayed for time before it")
	}
	if percepts[0].W >= 0.80 {
		t.Error("the untouched remainder of the interval must still decay")
	}
}

func TestDecayPercepts_GlobalFloorsAtPruneThreshold(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-180 * 24 * time.Hour) // 3 global half-lives
	percepts := []Percept{
		{ID: "g", W: 0.80, UpdatedAt: since.Add(-time.Hour)},
	}
	decayPercepts(percepts, now, since, globalHalfLifeDays, pruneThreshold)
	// 0.20 + 0.60·2^(−3) = 0.275 — still above the floor and the snap band.
	if !closeTo(percepts[0].W, 0.275) {
		t.Errorf("global W = %v, want 0.275", percepts[0].W)
	}

	// Long enough to fall within floorSnapBand of the floor: snaps to exactly
	// the floor, keeping the ADR-0056 "decayed to prune_threshold" archive
	// rule reachable under asymptotic decay.
	since = now.Add(-2000 * 24 * time.Hour)
	percepts = []Percept{{ID: "g", W: 0.80, UpdatedAt: since.Add(-time.Hour)}}
	decayPercepts(percepts, now, since, globalHalfLifeDays, pruneThreshold)
	if percepts[0].W != pruneThreshold {
		t.Errorf("global W = %v, want the prune floor %v", percepts[0].W, pruneThreshold)
	}
}

func TestDecayPercepts_NoTimeNoDecay(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	percepts := []Percept{{ID: "1", W: 0.7}}
	if n := decayPercepts(percepts, now, now, sessionHalfLifeDays, 0); n != 0 {
		t.Errorf("expected no decay at Δt=0, got %d", n)
	}
	if n := decayPercepts(percepts, now, time.Time{}, sessionHalfLifeDays, 0); n != 0 {
		t.Errorf("expected no decay with a zero cursor, got %d", n)
	}
	if percepts[0].W != 0.7 {
		t.Errorf("W = %v, want 0.7", percepts[0].W)
	}
}

func TestPrunePerceptsCount(t *testing.T) {
	// pruneThreshold = 0.20: percepts with W <= 0.20 are removed.
	percepts := []Percept{
		{ID: "1", W: 0.5},  // survives
		{ID: "2", W: 0.0},  // pruned (== threshold floor)
		{ID: "3", W: 0.1},  // pruned (< threshold)
		{ID: "4", W: 0.21}, // survives (> threshold)
		{ID: "5", W: 0.20}, // pruned (== threshold, not strictly greater)
	}
	result, pruned := prunePerceptsCount(percepts, pruneThreshold)
	if pruned != 3 {
		t.Errorf("expected 3 pruned, got %d", pruned)
	}
	if len(result) != 2 {
		t.Errorf("expected 2 remaining, got %d", len(result))
	}
}

func TestClampW(t *testing.T) {
	if clampW(1.5) != 1.0 {
		t.Error("clampW should cap at 1.0")
	}
	if clampW(-0.1) != 0 {
		t.Error("clampW should floor at 0")
	}
	if clampW(0.5) != 0.5 {
		t.Error("clampW should pass through valid values")
	}
}

func TestEdgePropagation_Extends(t *testing.T) {
	percepts := []Percept{
		{ID: "a", W: 0.5},
		{ID: "b", W: 0.5},
	}
	edges := []Edge{{From: "a", To: "b", Relation: RelationExtends}}
	result := applyEdges(percepts, edges)
	if result[1].W != 0.5+edgeBoost {
		t.Errorf("extends edge should raise target W, got %v", result[1].W)
	}
}

func TestEdgePropagation_Contradicts(t *testing.T) {
	percepts := []Percept{
		{ID: "a", W: 0.5},
		{ID: "b", W: 0.5},
	}
	edges := []Edge{{From: "a", To: "b", Relation: RelationContradicts}}
	result := applyEdges(percepts, edges)
	if result[0].W != 0.5-edgeNegativeDelta {
		t.Errorf("contradicts edge should lower From W, got %v", result[0].W)
	}
	if result[1].W != 0.5-edgeNegativeDelta {
		t.Errorf("contradicts edge should lower To W, got %v", result[1].W)
	}
}

func TestConsolidate_PromotesAtThreshold(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{
		ID:       "promote-1",
		Content:  "high confidence fact",
		Producer: ProducerLocal,
		W:        promoteThreshold,
		Evidence: evidencePromote,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	for _, rp := range s.global.Percepts {
		if rp.ID == "promote-1" {
			return // promoted successfully
		}
	}
	for _, rp := range s.session.Percepts {
		if rp.ID == "promote-1" {
			t.Error("percept at the threshold should have been promoted to global, not left in session")
			return
		}
	}
	t.Error("percept not found after consolidation")
}

func TestConsolidate_DoesNotPromoteBelowThreshold(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{
		ID:       "nopromote-1",
		Content:  "medium confidence fact",
		Producer: ProducerLocal,
		W:        promoteThreshold - 0.05,
		Evidence: evidencePromote,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	for _, rp := range s.global.Percepts {
		if rp.ID == "nopromote-1" {
			t.Error("percept below promoteThreshold must not be promoted")
			return
		}
	}
}

func TestConsolidate_PrunesWeakPercept(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{ID: "weak-1", Content: "system hint", Producer: ProducerSystem, W: 0.30, Core: false}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	// One half-life of running time: 0.30 · 0.5 = 0.15 ≤ pruneThreshold.
	advanceClock(s, time.Duration(sessionHalfLifeDays)*24*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.session.Percepts {
		if rp.ID == "weak-1" {
			t.Error("W=0.30 percept should have been pruned after one half-life decayed it to 0.15")
			return
		}
	}
	for _, rp := range s.global.Percepts {
		if rp.ID == "weak-1" {
			t.Error("weak percept should not have been promoted")
		}
	}
}
