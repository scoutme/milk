package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
	"github.com/scoutme/milk/internal/memory"
)

// memStateWith opens a store whose global.json holds one non-core (promoted)
// percept — RecordGlobal makes core ones, which never go stale.
func memStateWith(t *testing.T, id, content string) *interactiveState {
	t.Helper()
	dir := t.TempDir()
	seed := `{"percepts":[{"id":"` + id + `","content":"` + content + `","producer":"primary","w":0.5,"roles":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],"engrams":[],"edges":[]}`
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	mem, err := memory.NewStoreParams(dir, "sess", memory.Params{GlobalArchiveAfterDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	return &interactiveState{mem: mem}
}

func memStateEmpty(t *testing.T) *interactiveState {
	t.Helper()
	mem, err := memory.NewStoreParams(t.TempDir(), "sess", memory.Params{GlobalArchiveAfterDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	return &interactiveState{mem: mem}
}

func TestExecMemory_StaleArchiveRestoreOnAnEmptyStore(t *testing.T) {
	st := memStateEmpty(t)
	if out := execMemory("stale", st); !strings.Contains(out, "no non-core global percepts") {
		t.Errorf("stale on empty store: %q", out)
	}
	if out := execMemory("archive", st); !strings.Contains(out, "archive is empty") {
		t.Errorf("archive on empty store: %q", out)
	}
	if out := execMemory("restore", st); !strings.Contains(out, "usage: /memory restore") {
		t.Errorf("restore without id: %q", out)
	}
	if out := execMemory("restore abc123", st); !strings.Contains(out, "cannot restore") {
		t.Errorf("restore of an unknown id: %q", out)
	}
}

func TestExecMemoryStale_ListsGlobalPerceptsWithTimeLeft(t *testing.T) {
	st := memStateWith(t, "11111111-aaaa-bbbb-cccc-000000000002", "the staging cluster lives in eu central")
	got := execMemory("stale", st)
	if !strings.Contains(got, "#11111111") || !strings.Contains(got, "days left") || !strings.Contains(got, "staging cluster") {
		t.Errorf("stale listing: %q", got)
	}
}

// Percepts injected into a prompt are in use: injection restarts their idle
// clock, so a fact that keeps being relevant is never archived as stale.
func TestPerceptsForAgent_InjectedPerceptsCountAsUsed(t *testing.T) {
	st := memStateWith(t, "22222222-aaaa-bbbb-cccc-000000000003", "the build uses task and go test")
	if got := perceptsForAgent(config.Config{}, st.mem, "how do I run the build", false); len(got) == 0 {
		t.Fatal("expected the percept to be injected")
	}
	entries := st.mem.Stale()
	if len(entries) != 1 || entries[0].Percept.LastUsedAt.IsZero() {
		t.Errorf("an injected percept was not marked as used: %+v", entries)
	}
}

// memStateWithTwo seeds a store with two non-core globals at the given
// weights whose contents share the prompt's keywords — so the relevance gate
// passes both and only the weight floor can tell them apart.
func memStateWithTwo(t *testing.T) *interactiveState {
	t.Helper()
	dir := t.TempDir()
	seed := `{"percepts":[` +
		`{"id":"33333333-aaaa-bbbb-cccc-000000000004","content":"the build uses go test and task","producer":"primary","w":0.2,"roles":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},` +
		`{"id":"44444444-aaaa-bbbb-cccc-000000000005","content":"run the build with go test every time","producer":"primary","w":0.9,"roles":{},"created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"}],` +
		`"engrams":[],"edges":[]}`
	if err := os.WriteFile(filepath.Join(dir, "global.json"), []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	mem, err := memory.NewStoreParams(dir, "sess", memory.Params{GlobalArchiveAfterDays: 14})
	if err != nil {
		t.Fatal(err)
	}
	return &interactiveState{mem: mem}
}

// TestPerceptsForAgent_WeightFloorHoldsIdleClock: a prune-floor percept whose
// keywords match the prompt must NOT be injected under the default floor —
// injection marks percepts used, so injecting it would reset the 14-day
// staleness-archiving clock every turn (issue #375, verdict item 1). The
// strong percept above the floor injects and counts as used as before; an
// explicitly disabled floor restores the old behavior for both.
func TestPerceptsForAgent_WeightFloorHoldsIdleClock(t *testing.T) {
	st := memStateWithTwo(t)
	got := perceptsForAgent(config.Config{}, st.mem, "how do I run the build with go test", false)
	if len(got) != 1 || !strings.Contains(got[0], "run the build") {
		t.Fatalf("default floor: injected %v, want only the percept above the floor", got)
	}
	var floor, strong *memory.Percept
	for _, e := range st.mem.Stale() {
		switch e.Percept.ID {
		case "33333333-aaaa-bbbb-cccc-000000000004":
			p := e.Percept
			floor = &p
		case "44444444-aaaa-bbbb-cccc-000000000005":
			p := e.Percept
			strong = &p
		}
	}
	if floor == nil || strong == nil {
		t.Fatalf("expected both globals in the store, got floor=%v strong=%v", floor, strong)
	}
	if !floor.LastUsedAt.IsZero() {
		t.Error("the floor percept must not be marked used — that is what starves the archive rule")
	}
	if strong.LastUsedAt.IsZero() {
		t.Error("an injected percept above the floor must still count as used")
	}

	off := config.Config{PerceptInjectMinW: -1}
	if got := perceptsForAgent(off, st.mem, "how do I run the build with go test", false); len(got) != 2 {
		t.Errorf("disabled floor: injected %d, want both", len(got))
	}
}
