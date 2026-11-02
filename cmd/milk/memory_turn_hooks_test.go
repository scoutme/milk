package main

import (
	"context"
	"fmt"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
	"github.com/scoutme/milk/internal/oversight"
)

// seedGlobals records n distinct percepts into a fresh store's *session*
// scope with the user producer, so the after-turn pass promotes all of them
// to the global store on its fast path (explicit assertions, derivedW 0.9 ≥
// promote gate). Promoted facts are non-core — the class the size cap may
// evict; direct RecordGlobal seeds would be Core and exempt (see
// PruneGlobal's core partition).
func seedGlobals(t *testing.T, n int) *memory.Store {
	t.Helper()
	mem, err := memory.NewStoreParams(t.TempDir(), "hook-sess", memory.Params{})
	if err != nil {
		t.Fatalf("NewStoreParams: %v", err)
	}
	for i := 0; i < n; i++ {
		// Fully disjoint token sets per i: distinct contents must not
		// dedup into one another (overlap would collapse the seed).
		if _, err := mem.Record(context.Background(),
			fmt.Sprintf("seed%02d nebula%02d lantern%02d quasar%02d", i, i, i, i),
			memory.ProducerUser, memory.ConsumerAll, memory.Roles{}, false); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	return mem
}

// TestConsolidateAfterTurn pins the shared after-turn hook (#375 proposal 3):
// nil is a safe no-op, a small store under the default cap is untouched (the
// percept_store_max backstop only binds on runaway growth), and an explicit
// cap prunes right after the pass.
func TestConsolidateAfterTurn(t *testing.T) {
	t.Run("nil store is a no-op", func(t *testing.T) {
		consolidateAfterTurn(nil, config.Config{}) // must not panic
	})

	t.Run("default config leaves a small store alone", func(t *testing.T) {
		mem := seedGlobals(t, 2)
		consolidateAfterTurn(mem, config.Config{}) // unset → DefaultPerceptStoreMax (200)
		if got := len(mem.AllGlobal()); got != 2 {
			t.Errorf("globals = %d, want 2 (backstop must not bind below 200)", got)
		}
	})

	t.Run("configured cap prunes to the limit", func(t *testing.T) {
		mem := seedGlobals(t, 2)
		consolidateAfterTurn(mem, config.Config{PerceptStoreMax: 1})
		if got := len(mem.AllGlobal()); got != 1 {
			t.Errorf("globals = %d, want 1 (cap=1)", got)
		}
	})
}

// TestWorkflowTurnRunner_RunConsolidatesHostStore pins the workflow hook
// (#375 proposal 3): every role turn runs the pass against the host's store —
// workflow runs used to have no hook at all, so an all-workflow session never
// decayed, pruned or promoted.
func TestWorkflowTurnRunner_RunConsolidatesHostStore(t *testing.T) {
	mem := seedGlobals(t, 2)
	wtr := &workflowTurnRunner{
		inner:    &fakeTurnRunner{},
		cfg:      config.Config{PerceptStoreMax: 1},
		sess:     newWorkflowSession(),
		mem:      mem,
		role:     RoleWorkflow,
		roleName: "generator",
		notifier: oversight.Noop{},
	}
	if _, err := wtr.Run(context.Background(), "do the thing", nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if got := len(mem.AllGlobal()); got != 1 {
		t.Errorf("globals after role turn = %d, want 1 (consolidateAfterTurn must run)", got)
	}
}
