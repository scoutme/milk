package memory

// Validation suite for the ADR-0053 weight model: deterministic simulations
// with known ground truth for every trajectory the model claims. These double
// as the empirical validation of the reinforcement (B) and recency/decay (D)
// designs, and of the edge wiring (C).

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// closeTo compares weights within float-noise tolerance.
func closeTo(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// --- B: reinforcement ---

// TestReconfirmTrajectory validates the ground-truth W path for an inference
// percept (born 0.7 via ProducerLocal) under the ADR-0091 derived model:
// re-statement in the same run is echo — it adds nothing to W beyond the
// run's single corroboration event — while corroboration in a distinct second
// run moves derivedW past the promotion threshold. W is refreshed to derivedW
// at every re-affirmation, so the trajectory is the corroboration series
// 0.7 + corrobBoost(n), not a flat +0.05 per restatement.
func TestReconfirmTrajectory(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	s, err := NewStoreParams(dir, "traj", Params{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	freezeClock(s)

	first, err := s.Record(ctx, "milk is configured to escalate to claude cli", ProducerLocal, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if first.W != 0.7 {
		t.Fatalf("expected birth W 0.7 (ProducerLocal), got %v", first.W)
	}

	// First restatement earns the run's one corroboration event; the four
	// after it are echo and change nothing.
	want := []float64{
		0.7 + corrobBoost(1),
		0.7 + corrobBoost(1),
		0.7 + corrobBoost(1),
		0.7 + corrobBoost(1),
		0.7 + corrobBoost(1),
	}
	for i, w := range want {
		out, err := s.Record(ctx, "milk is configured to escalate to claude cli", ProducerLocal, ConsumerAll, Roles{}, false)
		if err != nil {
			t.Fatalf("reinforce %d: %v", i+1, err)
		}
		if !out.Reinforced {
			t.Fatalf("reinforce %d: expected Reinforced=true", i+1)
		}
		if out.ID != first.ID {
			t.Errorf("reinforce %d: kept ID %s, want %s", i+1, out.ID, first.ID)
		}
		if !closeTo(out.W, w) {
			t.Errorf("reinforce %d: W = %v, want %v (echo in one run must not climb)", i+1, out.W, w)
		}
		if len(s.session.Percepts) != 1 {
			t.Fatalf("reinforcement must not insert: %d percepts in session", len(s.session.Percepts))
		}
	}

	// Five re-statements in one sitting are one corroboration event.
	if got := s.session.Percepts[0].Evidence; got != 1 {
		t.Fatalf("Evidence = %d after one run of reinforcement, want 1", got)
	}

	// One corroboration (derivedW 0.783) is still held back: Evidence 1
	// cannot graduate an inference percept (promotion_deferred).
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	if len(s.global.Percepts) != 0 {
		t.Fatalf("one run must not promote, got global=%v", s.global.Percepts)
	}

	// A second Store lifetime re-confirming the fact is the second distinct
	// run: Evidence 2, derivedW 0.7 + corrobBoost(2) = 0.825 — over the gate,
	// but the durability window starts only now.
	s2, err := NewStoreParams(dir, "traj", Params{})
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	freezeClock(s2)
	out, err := s2.Record(ctx, "milk is configured to escalate to claude cli", ProducerLocal, ConsumerAll, Roles{}, false)
	if err != nil || !out.Reinforced {
		t.Fatalf("run 2 reinforce: %+v, %v", out, err)
	}
	if !closeTo(out.W, 0.7+corrobBoost(2)) {
		t.Errorf("run 2 W = %v, want %v", out.W, 0.7+corrobBoost(2))
	}
	if err := s2.Consolidate(); err != nil {
		t.Fatalf("run 2 Consolidate: %v", err)
	}
	if len(s2.global.Percepts) != 0 {
		t.Fatalf("the durability window must gate run 2, got global=%v", s2.global.Percepts)
	}
	advanceClock(s2, 25*time.Hour)
	if err := s2.Consolidate(); err != nil {
		t.Fatalf("run 2 durable Consolidate: %v", err)
	}
	if len(s2.global.Percepts) != 1 || s2.global.Percepts[0].ID != first.ID {
		t.Fatalf("expected the percept promoted to global once durable, got global=%v", s2.global.Percepts)
	}
	if got := s2.global.Percepts[0].Evidence; got != 2 {
		t.Errorf("promoted Evidence = %d, want 2 (one per run)", got)
	}
	if len(s2.session.Percepts) != 0 {
		t.Errorf("expected empty session after promotion, got %d", len(s2.session.Percepts))
	}
}

// TestReconfirmNeverLowers: a percept already above the reconfirm cap (a
// ProducerUser fact born at 0.9) keeps its weight on re-statement.
func TestReconfirmNeverLowers(t *testing.T) {
	s := newTestStore(t, true)
	ctx := context.Background()
	out, err := s.Record(ctx, "user prefers tabs over spaces", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	out2, err := s.Record(ctx, "user prefers tabs over spaces", ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("reinforce: %v", err)
	}
	if out2.W != out.W {
		t.Errorf("reinforcement lowered/changed W: %v -> %v", out.W, out2.W)
	}
}

// TestReconfirmTouchesUpdatedAt: reinforcement refreshes the rank score's
// recency factor (and adds the evidence factor), so a re-statement recovers
// recall ordering.
func TestReconfirmTouchesUpdatedAt(t *testing.T) {
	s := newTestStore(t, true)
	ctx := context.Background()
	out, _ := s.Record(ctx, "the deploy target is a k3s cluster", ProducerLocal, ConsumerAll, Roles{}, false)
	for i := range s.session.Percepts {
		if s.session.Percepts[i].ID == out.ID {
			s.session.Percepts[i].CreatedAt = time.Now().Add(-90 * 24 * time.Hour)
			s.session.Percepts[i].UpdatedAt = time.Now().Add(-90 * 24 * time.Hour)
		}
	}
	before := s.rankScore(s.session.Percepts[0], time.Now())

	if out2, _ := s.Record(ctx, "the deploy target is a k3s cluster", ProducerLocal, ConsumerAll, Roles{}, false); !out2.Reinforced {
		t.Fatal("expected reinforcement")
	}
	after := s.rankScore(s.session.Percepts[0], time.Now())
	if after <= before {
		t.Errorf("rank score did not recover: %v -> %v", before, after)
	}
}

// --- D: recency + decay ---

// TestRecencyOrdering validates the ground-truth recall ordering:
// equal relevance → the recently updated percept ranks first;
// more relevance always beats the recency bonus (hits dominate);
// min_confidence gates raw W, never age.
func TestRecencyOrdering(t *testing.T) {
	s := newTestStore(t, true)
	ctx := context.Background()

	fresh, _ := s.Record(ctx, "staging database is postgres sixteen with read replicas in europe", ProducerLocal, ConsumerAll, Roles{}, false)
	old, _ := s.Record(ctx, "staging database is postgres fifteen legacy cluster in the south", ProducerLocal, ConsumerAll, Roles{}, false)
	if fresh.ID == old.ID {
		t.Fatalf("test setup: percepts must be distinct (no reinforcement): fresh=%+v old=%+v", fresh, old)
	}

	// Age "old" to 120 days (4 half-lives → bonus ≈ 0.006) while keeping W equal.
	for i := range s.session.Percepts {
		if s.session.Percepts[i].ID == old.ID {
			s.session.Percepts[i].UpdatedAt = time.Now().Add(-120 * 24 * time.Hour)
		}
	}

	// Same query hits (3 each): fresh must rank first on recency alone.
	got := s.Query(ctx, "staging database postgres", 0, 0, ConsumerAll)
	if len(got) != 2 {
		t.Fatalf("expected 2 results, got %d", len(got))
	}
	if got[0].ID != fresh.ID {
		t.Errorf("expected the fresh percept first, got %q", got[0].Content)
	}

	// Hits dominate: a 4-token match on the old percept beats a 3-token match
	// on the fresh one regardless of recency.
	got = s.Query(ctx, "staging database postgres fifteen", 0, 0, ConsumerAll)
	if len(got) != 2 || got[0].ID != old.ID {
		t.Errorf("expected the higher-hit (old) percept first, got %d results", len(got))
	}

	// min_confidence gates raw W, never age: the old percept (W 0.7, bonus
	// ~0) passes min_confidence=0.7.
	if got := s.Query(ctx, "fifteen", 0.7, 0, ConsumerAll); len(got) != 1 || got[0].ID != old.ID {
		t.Errorf("expected the old percept visible at min_confidence=0.7 (raw W gate), got %v", got)
	}
	if got := s.Query(ctx, "fifteen", 0.75, 0, ConsumerAll); len(got) != 0 {
		t.Errorf("expected min_confidence=0.75 to filter the old percept, got %d results", len(got))
	}
}

// TestGlobalDecay validates the gentle global pass: non-core (promoted)
// percepts decay toward the prune floor with a running-time half-life of
// global_half_life_days; core percepts never decay.
func TestGlobalDecay(t *testing.T) {
	s := newTestStore(t, true)
	ctx := context.Background()

	if _, err := s.RecordGlobal(ctx, "milk lives at github.com/scoutme/milk", ProducerUser, ConsumerAll, Roles{}); err != nil {
		t.Fatalf("RecordGlobal: %v", err)
	}
	s.global.Percepts = append(s.global.Percepts, Percept{
		ID: "promoted-1", Content: "build with go 1.25", Producer: ProducerLocal, W: 0.80,
		UpdatedAt: s.now().Add(-time.Hour),
	})

	// One global half-life of running time: 0.20 + 0.60·0.5 = 0.50.
	advanceClock(s, time.Duration(globalHalfLifeDays)*24*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	for _, p := range s.global.Percepts {
		if p.ID == "promoted-1" && !closeTo(p.W, 0.50) {
			t.Fatalf("after one half-life W = %v, want 0.50", p.W)
		}
	}

	// Two more half-lives: sinks further but never below the floor.
	advanceClock(s, 2*time.Duration(globalHalfLifeDays)*24*time.Hour)
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}

	for _, p := range s.global.Percepts {
		if p.ID == "promoted-1" {
			if !closeTo(p.W, 0.20+0.60*math.Pow(0.5, 3)) {
				t.Errorf("after three half-lives W = %v, want %v", p.W, 0.20+0.60*math.Pow(0.5, 3))
			}
			if p.W <= pruneThreshold {
				t.Error("global decay must stop at the prune floor, never below")
			}
		} else if p.W != 1.0 {
			t.Errorf("core percept must not decay, got W %v", p.W)
		}
	}
}

// --- C: edge wiring ---

// TestEdgeWiring validates that AddEdge + consolidation actually move weights
// now that edges are wired: contradicts penalizes both endpoints (session
// pass), extends boosts the target (global pass), and edge bookkeeping is
// idempotent and validated.
func TestEdgeWiring(t *testing.T) {
	s := newTestStore(t, true)
	ctx := context.Background()

	a, _ := s.Record(ctx, "the api server listens on port 8080 in production", ProducerLocal, ConsumerAll, Roles{}, false)
	b, _ := s.Record(ctx, "the workers listen on port 9090 during batch jobs", ProducerLocal, ConsumerAll, Roles{}, false)
	if a.ID == b.ID {
		t.Fatalf("test setup: percepts must be distinct (no reinforcement): %+v", a)
	}

	if err := s.AddEdge(a.ID, b.ID, RelationContradicts); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := s.AddEdge(a.ID, b.ID, RelationContradicts); err != nil {
		t.Fatalf("duplicate AddEdge must be idempotent: %v", err)
	}
	if err := s.AddEdge(a.ID, b.ID, "nonsense"); err == nil {
		t.Error("unknown relation must be rejected")
	}
	if err := s.AddEdge(a.ID, a.ID, RelationExtends); err == nil {
		t.Error("self-edge must be rejected")
	}
	if err := s.AddEdge(a.ID, "no-such-percept", RelationExtends); err == nil {
		t.Error("edge to a missing target must be rejected")
	}

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// Ground truth (no running time elapsed): birth 0.7 − contradicts 0.10 =
	// 0.60 (the duplicate edge must NOT bite twice), and a contradicts edge is
	// a conflict, not corroboration — no Evidence either side.
	for _, p := range s.session.Percepts {
		if p.ID == a.ID || p.ID == b.ID {
			if !closeTo(p.W, 0.60) {
				t.Errorf("contradicts endpoint %s W = %v, want 0.60", p.ID[:8], p.W)
			}
			if p.Evidence != 0 {
				t.Errorf("contradicts endpoint %s Evidence = %d, want 0", p.ID[:8], p.Evidence)
			}
		}
	}

	// Global pass: a positive edge between two non-core global percepts.
	c := Percept{ID: "g1", Content: "the api server is written in go", Producer: ProducerLocal, W: 0.60}
	d := Percept{ID: "g2", Content: "the api server is written in rust", Producer: ProducerLocal, W: 0.60}
	s.global.Percepts = append(s.global.Percepts, c, d)
	if err := s.AddEdge("g1", "g2", RelationExtends); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// Ground truth for d: 0.60 + one corroboration marginal (no running time
	// elapsed, no decay), plus one Evidence point for the positive edge.
	for _, p := range s.global.Percepts {
		if p.ID == "g2" {
			if !closeTo(p.W, 0.60+corrobBoost(1)) {
				t.Errorf("extends target W = %v, want %v", p.W, 0.60+corrobBoost(1))
			}
			if p.Evidence != 1 {
				t.Errorf("extends target Evidence = %d, want 1 (positive-edge corroboration)", p.Evidence)
			}
		}
		if p.ID == "g1" && !closeTo(p.W, 0.60) {
			t.Errorf("extends source (decay only) W = %v, want 0.60", p.W)
		}
	}

	// The corroboration point is per run: another pass on the same edge does
	// not double-count it — neither the Evidence nor the W boost.
	if err := s.Consolidate(); err != nil {
		t.Fatalf("second Consolidate: %v", err)
	}
	for _, p := range s.global.Percepts {
		if p.ID == "g2" {
			if p.Evidence != 1 {
				t.Errorf("Evidence = %d after a second pass on the same edge, want 1 (once per run)", p.Evidence)
			}
			if !closeTo(p.W, 0.60+corrobBoost(1)) {
				t.Errorf("edge boost must apply once per run, got W = %v", p.W)
			}
		}
	}
}

// TestRecordMemoryRelationTool validates the tool-level wiring: relation +
// related_to on record_memory create a real edge that consolidation applies.
func TestRecordMemoryRelationTool(t *testing.T) {
	s := newToolsStore(t)
	ctx := context.Background()

	res := DispatchRecordMemory(ctx, s, `{"content":"nightly releases are signed with cosign"}`)
	if strings.Contains(res, `"error"`) {
		t.Fatalf("record: %s", res)
	}
	target := s.global.Percepts[0]
	before := target.W

	// Deliberately unrelated content so this inserts a NEW percept (not a
	// reinforcement) whose edge we can observe.
	res = DispatchRecordMemory(ctx, s, `{"content":"the staging database is postgres sixteen","relation":"contradicts","related_to":"`+target.ID[:8]+`"}`)
	if strings.Contains(res, `"error"`) {
		t.Fatalf("record with relation: %s", res)
	}
	if len(s.global.Percepts) != 2 {
		t.Fatalf("expected a new percept (not reinforced), got %d", len(s.global.Percepts))
	}

	if err := s.Consolidate(); err != nil {
		t.Fatalf("Consolidate: %v", err)
	}
	// Ground truth (no running time elapsed): the target takes the contradiction
	// penalty contraDelta; source does too.
	for _, p := range s.global.Percepts {
		if p.ID == target.ID && !closeTo(p.W, before-contraDelta) {
			t.Errorf("contradicts target W = %v, want %v", p.W, before-contraDelta)
		}
	}
}

// --- corpus-effect run ---

// TestCorpusEffect mirrors the real ~/.milk/memory corpus shape (measured:
// 16 global non-core @0.80 promoted, 15 session W 0.2–0.7, 0 edges) and
// reports what the new model does over successive consolidations. With
// MEMORY_CORPUS=/path/to/copy it runs against a real store copy instead.
func TestCorpusEffect(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	real := os.Getenv("MEMORY_CORPUS") != ""

	if real {
		entries, err := os.ReadDir(os.Getenv("MEMORY_CORPUS"))
		if err != nil {
			t.Fatalf("read corpus: %v", err)
		}
		for _, e := range entries {
			data, err := os.ReadFile(filepath.Join(os.Getenv("MEMORY_CORPUS"), e.Name()))
			if err != nil {
				t.Fatalf("read %s: %v", e.Name(), err)
			}
			if err := os.WriteFile(filepath.Join(dir, e.Name()), data, 0o600); err != nil {
				t.Fatalf("write %s: %v", e.Name(), err)
			}
		}
	} else {
		s := &Store{globalPath: filepath.Join(dir, "global.json"), params: DefaultParams()}
		for i := 0; i < 16; i++ {
			s.global.Percepts = append(s.global.Percepts, Percept{
				ID: "g" + string(rune('a'+i)), Content: "promoted fact " + string(rune('a'+i)),
				Producer: ProducerUser, W: 0.80,
				CreatedAt: time.Now().Add(-30 * 24 * time.Hour), UpdatedAt: time.Now().Add(-30 * 24 * time.Hour),
			})
		}
		if err := s.saveFile(s.globalPath, s.global); err != nil {
			t.Fatal(err)
		}
	}

	s, err := NewStoreParams(dir, "corpus-sim", Params{})
	if err != nil {
		t.Fatalf("open corpus copy: %v", err)
	}
	if !real {
		// Seed session percepts W 0.2–0.7 like the real corpus (0 edges).
		ws := []float64{0.2, 0.2, 0.3, 0.3, 0.3, 0.4, 0.4, 0.4, 0.5, 0.5, 0.5, 0.6, 0.6, 0.7, 0.7}
		for i, w := range ws {
			s.session.Percepts = append(s.session.Percepts, Percept{
				ID: "s" + string(rune('a'+i)), Content: "session fact " + string(rune('a'+i)),
				Producer: ProducerLocal, W: w,
				CreatedAt: time.Now(), UpdatedAt: time.Now(),
			})
		}
	}
	// One running day of milk time between consolidations — chatty or quiet,
	// decay is time-based.
	freezeClock(s)

	var counts []int
	for round := 1; round <= 3; round++ {
		advanceClock(s, 24*time.Hour)
		if err := s.ConsolidateCtx(ctx); err != nil {
			t.Fatalf("consolidation %d: %v", round, err)
		}
		counts = append(counts, len(s.session.Percepts))
		t.Logf("after consolidation %d: session=%d global=%d", round, len(s.session.Percepts), len(s.global.Percepts))
	}

	if !real {
		// Ground truth for the synthetic mirror (exact per-value floats are
		// noise-sensitive around the prune threshold, so assert the model's
		// claims rather than brittle counts): the weak half of the session
		// corpus is pruned within 3 consolidations, the strong percepts
		// survive (their W only halves over the 3 running days), and nothing
		// is ever deleted from global — its weights just sink
		// (0.80 → 0.2 + 0.6·2^(−3/60) ≈ 0.7796).
		if counts[0] != 13 {
			t.Errorf("after 1 running day only the two W=0.2 percepts should prune, got %v", counts)
		}
		if counts[2] >= 15 || counts[2] > counts[0] {
			t.Errorf("expected session corpus to shrink across consolidations, got %v", counts)
		}
		if counts[2] < 7 {
			t.Errorf("expected the 0.5/0.6/0.7 percepts to survive 3 consolidations, got %v", counts)
		}
		if !closeTo(s.global.Percepts[0].W, 0.2+0.6*math.Pow(0.5, 3.0/globalHalfLifeDays)) {
			t.Errorf("expected global decay 0.80 → %v after 3 running days, got %v",
				0.2+0.6*math.Pow(0.5, 3.0/globalHalfLifeDays), s.global.Percepts[0].W)
		}
		if len(s.global.Percepts) != 16 {
			t.Errorf("expected 16 global percepts (none deleted by decay), got %d", len(s.global.Percepts))
		}
	}
}
