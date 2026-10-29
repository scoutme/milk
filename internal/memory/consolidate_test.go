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
		ID:                  "corr-1",
		Content:             "corroborated inference",
		Producer:            ProducerLocal,
		W:                   0.85,
		Evidence:            evidencePromote,              // corroborated in two distinct runs
		AboveThresholdSince: s.now().Add(-48 * time.Hour), // durable: 2 running days above the gate
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
	t.Error("inference percept with Evidence 2 at derivedW 0.85, durable, should be promoted")
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

	// Run 1: stated and re-stated — one corroboration event at most (echo in
	// the same run adds nothing), W 0.7 → derivedW 0.7 + corrobBoost(1).
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
	// is the second distinct run: derivedW = 0.7 + corrobBoost(2) = 0.825,
	// Evidence 2. First pass over the threshold just starts the durability
	// clock — a boost burst cannot graduate on momentum.
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
	if len(s2.global.Percepts) != 0 {
		t.Fatalf("a freshly-thresholded fact must wait out the durability window, got %v", s2.global.Percepts)
	}
	if s2.session.Percepts[0].AboveThresholdSince.IsZero() {
		t.Error("the durability clock should have started at this pass")
	}

	// One running day later the fact has held the gate unchallenged: it is
	// durable and graduates. (Decay of W meanwhile is forgetting, not
	// falsifying — the gate reads derivedW.)
	advanceClock(s2, 25*time.Hour)
	if err := s2.Consolidate(); err != nil {
		t.Fatalf("run 2 second Consolidate: %v", err)
	}
	for _, rp := range s2.global.Percepts {
		if rp.ID == first.ID {
			if rp.Evidence != 2 {
				t.Errorf("promoted percept Evidence = %d, want 2", rp.Evidence)
			}
			return // promoted
		}
	}
	t.Error("percept corroborated in two runs and durable (derivedW 0.825) should promote")
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

func TestEdgeEvents_Extends(t *testing.T) {
	s := newTestStore(t, true)
	percepts := []Percept{
		{ID: "a", W: 0.5, Producer: ProducerLocal},
		{ID: "b", W: 0.5, Producer: ProducerLocal},
	}
	edges := []Edge{{From: "a", To: "b", Relation: RelationExtends}}
	normalizePriors(percepts)
	s.applyEdgeEvents(percepts, edges)
	if !closeTo(percepts[1].W, 0.5+corrobBoost(1)) {
		t.Errorf("extends edge should raise target W by the corroboration marginal, got %v", percepts[1].W)
	}
	if percepts[1].Evidence != 1 {
		t.Errorf("extends target Evidence = %d, want 1", percepts[1].Evidence)
	}
	if !closeTo(percepts[0].W, 0.5) {
		t.Errorf("extends source W should be untouched, got %v", percepts[0].W)
	}
	// Per-run cap: a second pass over the same edge changes nothing.
	s.applyEdgeEvents(percepts, edges)
	if !closeTo(percepts[1].W, 0.5+corrobBoost(1)) || percepts[1].Evidence != 1 {
		t.Errorf("edge events must apply exactly once per Store lifetime: W=%v Evidence=%d", percepts[1].W, percepts[1].Evidence)
	}
}

func TestEdgeEvents_Contradicts(t *testing.T) {
	s := newTestStore(t, true)
	percepts := []Percept{
		{ID: "a", W: 0.5, Producer: ProducerLocal},
		{ID: "b", W: 0.5, Producer: ProducerLocal},
	}
	edges := []Edge{{From: "a", To: "b", Relation: RelationContradicts}}
	normalizePriors(percepts)
	s.applyEdgeEvents(percepts, edges)
	if !closeTo(percepts[0].W, 0.5-contraDelta) {
		t.Errorf("contradicts edge should lower From W, got %v", percepts[0].W)
	}
	if !closeTo(percepts[1].W, 0.5-contraDelta) {
		t.Errorf("contradicts edge should lower To W, got %v", percepts[1].W)
	}
	if percepts[0].Contradictions != 1 || percepts[1].Contradictions != 1 {
		t.Errorf("contradicts endpoints should each count one conflict event, got %d/%d",
			percepts[0].Contradictions, percepts[1].Contradictions)
	}
	if percepts[0].Evidence != 0 || percepts[1].Evidence != 0 {
		t.Error("a conflict is not corroboration — no Evidence either side")
	}
	// Per-run cap: the second pass does not bite again.
	s.applyEdgeEvents(percepts, edges)
	if !closeTo(percepts[0].W, 0.5-contraDelta) || percepts[0].Contradictions != 1 {
		t.Errorf("conflict events must apply exactly once per Store lifetime: W=%v K=%d",
			percepts[0].W, percepts[0].Contradictions)
	}
}

func TestEdgeEvents_UpdatesSupersedes(t *testing.T) {
	s := newTestStore(t, true)
	percepts := []Percept{
		{ID: "old", W: 0.7, Producer: ProducerLocal},
		{ID: "new", W: 0.7, Producer: ProducerLocal},
	}
	// updates: From = the newer claim, To = the superseded one.
	edges := []Edge{{From: "new", To: "old", Relation: RelationUpdates}}
	normalizePriors(percepts)
	s.applyEdgeEvents(percepts, edges)

	if !closeTo(percepts[1].W, 0.7+corrobBoost(1)) || percepts[1].Evidence != 1 {
		t.Errorf("updates should corroborate its source (the newer claim): W=%v Evidence=%d", percepts[1].W, percepts[1].Evidence)
	}
	if !closeTo(percepts[0].W, 0.7-supersedPenalty) || percepts[0].Superseded != 1 {
		t.Errorf("updates should supersede its target: W=%v Superseded=%d", percepts[0].W, percepts[0].Superseded)
	}

	// Idempotent: a second pass changes nothing...
	s.applyEdgeEvents(percepts, edges)
	if !closeTo(percepts[0].W, 0.7-supersedPenalty) || percepts[0].Superseded != 1 {
		t.Errorf("supersession must not stack: W=%v Superseded=%d", percepts[0].W, percepts[0].Superseded)
	}

	// ...and removing the edge lifts the supersession again.
	s.applyEdgeEvents(percepts, nil)
	if percepts[0].Superseded != 0 {
		t.Errorf("a removed updates edge should lift the supersession, got Superseded=%d", percepts[0].Superseded)
	}
	if !closeTo(percepts[0].W, 0.7) {
		t.Errorf("lifting the supersession should restore its marginal, got W=%v", percepts[0].W)
	}
}

func TestDerivedW(t *testing.T) {
	p := Percept{Producer: ProducerLocal, Prior: 0.7}
	if w := derivedW(p); !closeTo(w, 0.7) {
		t.Errorf("derivedW(prior 0.7) = %v, want 0.7", w)
	}
	p.Evidence = 2
	if w := derivedW(p); !closeTo(w, 0.7+corrobBoost(2)) {
		t.Errorf("derivedW with Evidence 2 = %v, want %v", w, 0.7+corrobBoost(2))
	}
	p.Contradictions = 1
	if w := derivedW(p); !closeTo(w, 0.7+corrobBoost(2)-contraDelta) {
		t.Errorf("derivedW with one conflict = %v, want %v", w, 0.7+corrobBoost(2)-contraDelta)
	}
	// Corroboration saturates at corrobMax and non-core W never passes the
	// reconfirm cap: repetition cannot mint perfect confidence.
	p.Contradictions = 0
	p.Evidence = 1000
	if w := derivedW(p); w > reconfirmCap || w != reconfirmCap {
		t.Errorf("derivedW with huge Evidence = %v, want the cap %v", w, reconfirmCap)
	}
	// Contradiction penalties saturate at contraMax.
	p.Evidence = 0
	p.Contradictions = 1000
	if w := derivedW(p); !closeTo(w, 0.7-contraMax) {
		t.Errorf("derivedW with huge Contradictions = %v, want %v", w, 0.7-contraMax)
	}
	// Core is exempt from the cap.
	c := Percept{Core: true, Prior: 1.0, Evidence: 3}
	if w := derivedW(c); w != 1.0 {
		t.Errorf("derivedW(core) = %v, want 1.0", w)
	}
}

func TestNormalizePriors_Migration(t *testing.T) {
	// A pre-v4 percept: W carries the count-derived boosts already.
	percepts := []Percept{
		{ID: "legacy", W: 0.80, Evidence: 2, Producer: ProducerLocal},
	}
	normalizePriors(percepts)
	// The corroboration backing is removed from the prior...
	if !closeTo(percepts[0].Prior, 0.80-corrobBoost(2)) {
		t.Errorf("migrated Prior = %v, want %v (boosts backed out once)", percepts[0].Prior, 0.80-corrobBoost(2))
	}
	// ...so re-derivation reproduces the stored W exactly — no double count.
	if !closeTo(derivedW(percepts[0]), 0.80) {
		t.Errorf("derivedW after migration = %v, want the stored 0.80", derivedW(percepts[0]))
	}
	// Idempotent: running again does not move the prior.
	normalizePriors(percepts)
	if !closeTo(percepts[0].Prior, 0.80-corrobBoost(2)) {
		t.Errorf("migration must be idempotent, Prior = %v", percepts[0].Prior)
	}
	// And a v4 percept is never touched.
	v4 := []Percept{{ID: "v4", W: 0.7, Prior: 0.7, Evidence: 1}}
	normalizePriors(v4)
	if v4[0].Prior != 0.7 {
		t.Errorf("v4 Prior must not be re-migrated, got %v", v4[0].Prior)
	}
}

func TestConsolidate_PromotesAtThreshold(t *testing.T) {
	s := newTestStore(t, true)
	p := Percept{
		ID:                  "promote-1",
		Content:             "high confidence fact",
		Producer:            ProducerLocal,
		W:                   promoteThreshold,
		Evidence:            evidencePromote,
		AboveThresholdSince: s.now().Add(-48 * time.Hour), // durable
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

func TestConsolidate_DefersUndurableFact(t *testing.T) {
	s := newTestStore(t, true)
	// Corroborated in two runs and over the threshold — but the first pass
	// above the gate only starts the durability clock (ADR-0091): a boost
	// burst cannot graduate on momentum.
	p := Percept{
		ID:       "burst-1",
		Content:  "burst-corroborated inference",
		Producer: ProducerLocal,
		W:        0.85,
		Evidence: evidencePromote,
	}
	s.mu.Lock()
	s.session.Percepts = append(s.session.Percepts, p)
	s.mu.Unlock()

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(s.global.Percepts) != 0 {
		t.Fatalf("a just-thresholded fact must not promote on the same pass, got %v", s.global.Percepts)
	}
	stamp := s.session.Percepts[0].AboveThresholdSince
	if stamp.IsZero() {
		t.Fatal("the durability clock should have started")
	}

	// Half a running day is not durable yet.
	advanceClock(s, 12*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(s.global.Percepts) != 0 {
		t.Fatalf("half a day above the gate is not durable, got %v", s.global.Percepts)
	}

	// A full running day above the gate, unchallenged: durable, graduates.
	advanceClock(s, 13*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	found := false
	for _, rp := range s.global.Percepts {
		if rp.ID == "burst-1" {
			found = true
		}
	}
	if !found {
		t.Error("a durable, twice-corroborated fact should promote after the window")
	}
}

func TestConsolidate_DefersSupersededFact(t *testing.T) {
	s := newTestStore(t, true)
	a, err := s.Record(context.Background(), "the config lives in the old toml file", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	b, err := s.Record(context.Background(), "the config moved to the new yaml file", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := s.AddEdge(b.ID, a.ID, RelationUpdates); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// The newer claim (fast path) graduates; the superseded one is held back
	// even though it is a user fact — an updates edge means it is outdated.
	for _, rp := range s.global.Percepts {
		if rp.ID == a.ID {
			t.Error("a superseded percept must not promote while the updates edge is live")
		}
		if rp.ID != b.ID {
			t.Errorf("unexpected promoted percept %s", rp.ID[:8])
		}
	}
}

func TestDecayPercepts_UseSlowsForgetting(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-72 * time.Hour) // one bare session half-life
	percepts := []Percept{
		{ID: "unused", W: 0.8, UpdatedAt: since.Add(-time.Hour)},
		{ID: "used", W: 0.8, UpdatedAt: since.Add(-time.Hour), UseCount: useDecaySlowingCap},
	}
	decayPercepts(percepts, now, since, sessionHalfLifeDays, 0)
	// The used fact forgets slower (up to 2x half-life) — repetition buys
	// persistence, never truth: W only, nothing else moved.
	if !(percepts[1].W > percepts[0].W) {
		t.Errorf("a used fact must decay slower: used %v, unused %v", percepts[1].W, percepts[0].W)
	}
	want := 0.8 * math.Pow(0.5, 72.0/((sessionHalfLifeDays*useFactor(useDecaySlowingCap))*24))
	if !closeTo(percepts[1].W, want) {
		t.Errorf("used W = %v, want %v", percepts[1].W, want)
	}
}

func TestDecayPercepts_SupersededForgetsFaster(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	since := now.Add(-72 * time.Hour)
	percepts := []Percept{
		{ID: "current", W: 0.8, UpdatedAt: since.Add(-time.Hour)},
		{ID: "superseded", W: 0.8, UpdatedAt: since.Add(-time.Hour), Superseded: 1},
	}
	decayPercepts(percepts, now, since, sessionHalfLifeDays, 0)
	if !(percepts[1].W < percepts[0].W) {
		t.Errorf("a superseded fact must forget faster: superseded %v, current %v", percepts[1].W, percepts[0].W)
	}
}
