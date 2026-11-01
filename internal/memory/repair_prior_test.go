package memory

// Regression pins for issue #375, verdict proposal 2: the pre-v4 Prior
// migration (normalizePriors: Prior := W for Prior-less percepts) seeded
// long-lived user facts at the prune floor, because the old linear model had
// already decayed them to 0.20 before v4 shipped. Prior is what every v4 pass
// derives strength from, so those facts stayed pinned at 0.20 forever —
// below recall's min_confidence (0.4) and below the injection weight floor,
// while being the store's most trusted class (user decisions).
//
// repairMigratedUserPriors restores the known provenance (user prior =
// initialWeight(user) = 0.9) and refreshes W to derivedW; it must be
// idempotent and must not touch non-user, healthy or unmigrated percepts.

import (
	"math"
	"testing"
)

func TestRepairMigratedUserPriors_FloorPinnedUserFact(t *testing.T) {
	ps := []Percept{{ID: "p1", Producer: ProducerUser, W: 0.20, Prior: 0.20}}
	if n := repairMigratedUserPriors(ps, pruneThreshold); n != 1 {
		t.Fatalf("repaired %d percepts, want 1", n)
	}
	if ps[0].Prior != initialWeight(ProducerUser, false) {
		t.Errorf("Prior = %v, want 0.9", ps[0].Prior)
	}
	if math.Abs(ps[0].W-derivedW(ps[0])) > 1e-12 {
		t.Errorf("W = %v, want derivedW %v", ps[0].W, derivedW(ps[0]))
	}
	// Idempotent: a repaired prior can never re-trigger the condition.
	if n := repairMigratedUserPriors(ps, pruneThreshold); n != 0 {
		t.Errorf("second pass repaired %d percepts, want 0", n)
	}
}

// Snap-band pinning: normalizePriors backs boosts out of W, so migrated
// priors land at values like 0.249 — inside the floor snap band (0.05), not
// only exactly at 0.20. The repair must cover the whole band.
func TestRepairMigratedUserPriors_SnapBandPinned(t *testing.T) {
	ps := []Percept{{ID: "p2", Producer: ProducerUser, W: 0.249, Prior: 0.249}}
	if n := repairMigratedUserPriors(ps, pruneThreshold); n != 1 {
		t.Fatalf("repaired %d percepts, want 1 (prior in snap band)", n)
	}
	if ps[0].Prior != 0.9 {
		t.Errorf("Prior = %v, want 0.9", ps[0].Prior)
	}
}

// A healthy user fact (post-migration records start at Prior 0.9) must be
// left alone even when decay has dragged W down — aging is not migration.
func TestRepairMigratedUserPriors_HealthyUserUntouched(t *testing.T) {
	ps := []Percept{{ID: "p3", Producer: ProducerUser, W: 0.31, Prior: 0.9}}
	if n := repairMigratedUserPriors(ps, pruneThreshold); n != 0 {
		t.Fatalf("repaired %d percepts, want 0", n)
	}
	if ps[0].Prior != 0.9 || ps[0].W != 0.31 {
		t.Errorf("healthy fact mutated: prior=%v w=%v", ps[0].Prior, ps[0].W)
	}
}

// Migrated primary/escalation/system facts keep the monotone-safe
// approximation normalizePriors gave them; session percepts are meant to
// prune at the floor. Only the user class is repaired.
func TestRepairMigratedUserPriors_NonUserUntouched(t *testing.T) {
	for _, producer := range []Producer{ProducerLocal, ProducerEscalation, ProducerSystem} {
		ps := []Percept{{ID: "x", Producer: producer, W: 0.20, Prior: 0.20}}
		if n := repairMigratedUserPriors(ps, pruneThreshold); n != 0 {
			t.Errorf("producer %s repaired %d percepts, want 0", producer, n)
		}
		if ps[0].Prior != 0.20 || ps[0].W != 0.20 {
			t.Errorf("producer %s mutated: prior=%v w=%v", producer, ps[0].Prior, ps[0].W)
		}
	}
}

func TestRepairMigratedUserPriors_CoreAndUnmigratedUntouched(t *testing.T) {
	ps := []Percept{
		{ID: "core", Producer: ProducerUser, Core: true, W: 0.20, Prior: 0.20}, // core exempt
		{ID: "fresh", Producer: ProducerUser, W: 0.20, Prior: 0},               // normalizePriors' job, not ours
	}
	if n := repairMigratedUserPriors(ps, pruneThreshold); n != 0 {
		t.Fatalf("repaired %d percepts, want 0", n)
	}
	if ps[0].Prior != 0.20 || ps[1].Prior != 0 {
		t.Errorf("touched: core prior=%v unmigrated prior=%v", ps[0].Prior, ps[1].Prior)
	}
}

// The non-core ceiling (reconfirmCap 0.90) sits exactly at the restored user
// prior, so a repaired user fact lands at derivedW — capped — no matter how
// much Evidence the migration had: user assertions are already max trust,
// corroboration cannot mint more (the model's own rule). The repair must go
// through derivedW (the same function every other W refresh uses), not a raw
// assignment, so cap and penalties stay in play.
func TestRepairMigratedUserPriors_RespectsReconfirmCap(t *testing.T) {
	ps := []Percept{{ID: "p4", Producer: ProducerUser, W: 0.20, Prior: 0.20, Evidence: 3}}
	if n := repairMigratedUserPriors(ps, pruneThreshold); n != 1 {
		t.Fatalf("repaired %d percepts, want 1", n)
	}
	if want := derivedW(ps[0]); ps[0].W != want {
		t.Errorf("W = %v, want derivedW %v", ps[0].W, want)
	}
	if ps[0].W != reconfirmCap {
		t.Errorf("W = %v, want the non-core cap %v", ps[0].W, reconfirmCap)
	}
}

// End-to-end through normalizeLocked: a pre-v4 user percept (Prior absent in
// JSON, W already floor-decayed by the old linear model) migrates and is
// repaired in the same pass, with the store's configured prune threshold.
func TestNormalizeLocked_RepairsMigratedUserPriors(t *testing.T) {
	s := newTestStore(t, false)
	s.global.Percepts = []Percept{
		{ID: "old-user", Producer: ProducerUser, W: 0.20}, // Prior absent → pre-v4
		{ID: "old-escalation", Producer: ProducerEscalation, W: 0.20},
	}
	s.normalizeLocked()

	if s.global.Percepts[0].Prior != 0.9 || s.global.Percepts[0].W != 0.9 {
		t.Errorf("user fact after migrate+repair: prior=%v w=%v, want 0.9/0.9",
			s.global.Percepts[0].Prior, s.global.Percepts[0].W)
	}
	if s.global.Percepts[1].Prior != 0.20 || s.global.Percepts[1].W != 0.20 {
		t.Errorf("non-user fact mutated: prior=%v w=%v", s.global.Percepts[1].Prior, s.global.Percepts[1].W)
	}
	// And a second pass changes nothing.
	if before := s.global.Percepts[0]; before.Prior != 0.9 {
		t.Fatalf("precondition: %+v", before)
	}
	s.normalizeLocked()
	if s.global.Percepts[0].Prior != 0.9 || s.global.Percepts[0].W != 0.9 {
		t.Errorf("not idempotent: prior=%v w=%v", s.global.Percepts[0].Prior, s.global.Percepts[0].W)
	}
}
