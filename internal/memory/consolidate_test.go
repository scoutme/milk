package memory

import (
	"context"
	"testing"
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

func TestConsolidate_DecaysNonCorePercepts(t *testing.T) {
	s := newTestStore(t, true)
	s.Record(context.Background(), "decayable fact", ProducerLocal, ConsumerAll, Roles{}, false) //nolint:errcheck

	before := s.session.Percepts[0].W // ProducerLocal → 0.7
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// After decay: 0.7 − 0.10 = 0.60, which is < promoteThreshold (0.80) and > pruneThreshold (0.20).
	// So the percept should survive in session with reduced W.
	remaining := append(s.session.Percepts, s.global.Percepts...) //nolint:gocritic
	for _, p := range remaining {
		if p.Content == "decayable fact" {
			if p.W >= before {
				t.Error("non-core percept should have decayed")
			}
			return
		}
	}
	t.Errorf("percept should survive first consolidation at W=%.2f (after decay %.2f > pruneThreshold %.2f)",
		before, before-decayPerSession, pruneThreshold)
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

func TestConsolidate_PromotesHighWeight(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{
		ID:       "high-1",
		Content:  "high weight fact",
		Producer: ProducerUser,
		W:        0.9,
		Core:     false,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.global.Percepts {
		if rp.ID == "high-1" {
			return // promoted successfully
		}
	}
	// Check it wasn't left in session either
	for _, rp := range s.session.Percepts {
		if rp.ID == "high-1" {
			t.Error("high-weight percept should have been promoted to global, not left in session")
			return
		}
	}
	t.Error("high-weight percept not found after consolidation")
}

func TestApplyDecayCount(t *testing.T) {
	percepts := []Percept{
		{ID: "1", W: 0.5, Core: false},
		{ID: "2", W: 1.0, Core: true},
		{ID: "3", W: 0.02, Core: false},
	}
	result, n := applyDecayCount(percepts)
	if n != 2 {
		t.Errorf("expected 2 decayed, got %d", n)
	}
	if result[0].W != 0.5-decayPerSession {
		t.Errorf("percept 1 W wrong: got %v", result[0].W)
	}
	if result[1].W != 1.0 {
		t.Errorf("core percept should not decay, got %v", result[1].W)
	}
	if result[2].W != 0 {
		t.Errorf("W should floor at 0, got %v", result[2].W)
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
	result, pruned := prunePerceptsCount(percepts)
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
	if result[1].W != 0.5+edgePositiveDelta {
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
	// W=0.91: after decay (−0.10) becomes 0.81, which is >= promoteThreshold (0.80) → promotes.
	p := Percept{
		ID:       "promote-1",
		Content:  "high confidence fact",
		Producer: ProducerLocal,
		W:        0.91,
		Core:     false,
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
			t.Error("percept should have been promoted to global, not left in session")
			return
		}
	}
	t.Error("percept not found after consolidation")
}

func TestConsolidate_DoesNotPromoteBelowThreshold(t *testing.T) {
	s := newTestStore(t, true)
	// W=0.85: after decay (−0.10) becomes 0.75, which is < promoteThreshold (0.80) → stays in session.
	p := Percept{
		ID:       "nopromote-1",
		Content:  "medium confidence fact",
		Producer: ProducerLocal,
		W:        0.85,
		Core:     false,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	for _, rp := range s.global.Percepts {
		if rp.ID == "nopromote-1" {
			t.Error("percept with post-decay W=0.75 should NOT have been promoted (threshold 0.80)")
			return
		}
	}
}

func TestConsolidate_UserPerceptPromotesAfterOneSession(t *testing.T) {
	s := newTestStore(t, true)
	// ProducerUser initialWeight=0.9; after one decay (−0.10) = 0.80 == promoteThreshold.
	_, err := s.Record(context.Background(), "user stated fact", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.global.Percepts {
		if rp.Content == "user stated fact" {
			return // promoted — correct
		}
	}
	t.Error("user percept (W=0.9) should promote after one session (post-decay W=0.80 >= threshold 0.80)")
}

func TestConsolidate_PrunesWeakPercept(t *testing.T) {
	s := newTestStore(t, true)
	// W=0.30: after decay (−0.10) = 0.20, which is NOT > pruneThreshold (0.20) → pruned.
	p := Percept{ID: "weak-1", Content: "system hint", Producer: ProducerSystem, W: 0.30, Core: false}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, rp := range s.session.Percepts {
		if rp.ID == "weak-1" {
			t.Error("W=0.30 percept should have been pruned after decay to 0.20 (must be strictly > pruneThreshold 0.20)")
			return
		}
	}
	for _, rp := range s.global.Percepts {
		if rp.ID == "weak-1" {
			t.Error("weak percept should not have been promoted")
		}
	}
}
