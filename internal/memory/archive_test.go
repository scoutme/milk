package memory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// archiveStore opens a store whose clock the test drives.
func archiveStore(t *testing.T, p Params) (*Store, *time.Time) {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStoreParams(dir, "sess", p)
	if err != nil {
		t.Fatal(err)
	}
	clock := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	s.clock = func() time.Time { return clock }
	s.lastTick = clock
	return s, &clock
}

// seedGlobal adds a non-core global percept already decayed to the floor.
func seedGlobal(t *testing.T, s *Store, content string) string {
	t.Helper()
	out, err := s.RecordGlobal(context.Background(), content, ProducerLocal, ConsumerAll, Roles{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	for i := range s.global.Percepts {
		if s.global.Percepts[i].ID == out.ID {
			s.global.Percepts[i].Core = false
			s.global.Percepts[i].W = s.params.PruneThreshold
			s.global.Percepts[i].Prior = s.params.PruneThreshold // decayed to the floor all the way down
		}
	}
	s.mu.Unlock()
	return out.ID
}

func hasGlobal(s *Store, id string) bool {
	for _, p := range s.AllGlobal() {
		if p.ID == id {
			return true
		}
	}
	return false
}

func advance(t *testing.T, s *Store, clock *time.Time, d time.Duration) {
	t.Helper()
	*clock = clock.Add(d)
	if err := s.Consolidate(); err != nil {
		t.Fatal(err)
	}
}

// Running time is what counts, not the number of consolidations: ten short
// passes and one long pass over the same total time archive at the same moment.
func TestArchive_SessionTimeSumsRegardlessOfPassCount(t *testing.T) {
	const day = 24 * time.Hour
	p := Params{GlobalArchiveAfterDays: 14}

	many, clockA := archiveStore(t, p)
	idA := seedGlobal(t, many, "alpha unused fact about the build")
	for i := 0; i < 13; i++ {
		advance(t, many, clockA, day)
	}
	if !hasGlobal(many, idA) {
		t.Fatal("archived before 14 days of session time")
	}
	advance(t, many, clockA, day)
	if hasGlobal(many, idA) {
		t.Error("not archived after 14 days of session time over 14 passes")
	}

	one, clockB := archiveStore(t, p)
	idB := seedGlobal(t, one, "beta unused fact about the build")
	advance(t, one, clockB, 13*day)
	if !hasGlobal(one, idB) {
		t.Fatal("archived before 14 days in a single pass")
	}
	advance(t, one, clockB, day)
	if hasGlobal(one, idB) {
		t.Error("one pass over the same 14 days did not archive")
	}
}

func TestArchive_UseResetsTheIdleClock(t *testing.T) {
	const day = 24 * time.Hour
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: 14})
	used := seedGlobal(t, s, "gamma fact that keeps being injected")
	unused := seedGlobal(t, s, "delta fact nobody asks about anymore")

	advance(t, s, clock, 10*day)
	s.MarkUsed([]string{used}) // injected into a prompt
	advance(t, s, clock, 6*day)

	if !hasGlobal(s, used) {
		t.Error("a percept used 6 days ago was archived")
	}
	if hasGlobal(s, unused) {
		t.Error("a percept idle for 16 days survived")
	}
}

func TestArchive_RecallAndReinforcementCountAsUse(t *testing.T) {
	const day = 24 * time.Hour
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: 14})
	recalled := seedGlobal(t, s, "epsilon deployment uses kubernetes")
	restated := seedGlobal(t, s, "zeta the database is postgres sixteen")
	advance(t, s, clock, 12*day)

	s.Query(context.Background(), "kubernetes deployment", 0, 5, ConsumerAll)
	if _, err := s.RecordGlobal(context.Background(), "zeta the database is postgres sixteen", ProducerUser, ConsumerAll, Roles{}); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clock, 5*day)

	if !hasGlobal(s, recalled) || !hasGlobal(s, restated) {
		t.Errorf("used percepts archived: recalled=%v restated=%v", hasGlobal(s, recalled), hasGlobal(s, restated))
	}
}

func TestArchive_CoreAndHighWeightAreKept(t *testing.T) {
	const day = 24 * time.Hour
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: 14})
	core, err := s.RecordGlobal(context.Background(), "eta the user prefers tabs", ProducerUser, ConsumerAll, Roles{})
	if err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	s.global.Percepts[len(s.global.Percepts)-1].Core = true
	s.mu.Unlock()
	strong := seedGlobal(t, s, "theta strongly weighted fact about release")
	s.mu.Lock()
	for i := range s.global.Percepts {
		if s.global.Percepts[i].ID == strong {
			s.global.Percepts[i].W = 0.9
		}
	}
	s.mu.Unlock()
	advance(t, s, clock, 60*day)
	if !hasGlobal(s, core.ID) {
		t.Error("a core percept was archived")
	}
	if !hasGlobal(s, strong) {
		t.Error("a percept still above the prune weight was archived")
	}
}

func TestArchive_DisabledAndDefault(t *testing.T) {
	const day = 24 * time.Hour
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: -1})
	id := seedGlobal(t, s, "iota fact that must stay when archiving is off")
	advance(t, s, clock, 365*day)
	if !hasGlobal(s, id) {
		t.Error("archiving disabled but the percept was archived")
	}
	if got := (Params{}).normalized().GlobalArchiveAfterDays; got != 14 {
		t.Errorf("default = %v, want 14 days", got)
	}
}

// Nothing archives right after an upgrade: percepts from before the idle
// fields existed start their clock at zero.
func TestArchive_LegacyPerceptsStartAtZeroIdle(t *testing.T) {
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: 14})
	id := seedGlobal(t, s, "kappa older fact without idle fields")
	s.mu.Lock()
	for i := range s.global.Percepts {
		s.global.Percepts[i].LastUsedAt = time.Time{}
		s.global.Percepts[i].IdleSeconds = 0
	}
	s.mu.Unlock()
	advance(t, s, clock, time.Hour)
	if !hasGlobal(s, id) {
		t.Error("a legacy percept was archived on the first pass")
	}
}

func TestArchive_FileRoundTripAndRestore(t *testing.T) {
	const day = 24 * time.Hour
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: 14})
	id := seedGlobal(t, s, "lambda the staging cluster lives in eu central")
	other := seedGlobal(t, s, "mu the staging cluster uses spot instances")
	if err := s.AddEdge(id, other, RelationExtends); err != nil {
		t.Fatal(err)
	}
	advance(t, s, clock, 15*day)

	if hasGlobal(s, id) {
		t.Fatal("not archived")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(s.globalPath), "global-archive.jsonl"))
	if err != nil || !strings.Contains(string(raw), "staging cluster lives") {
		t.Fatalf("archive file missing the percept: %v %q", err, raw)
	}
	list, err := s.Archived()
	if err != nil || len(list) == 0 || list[0].Percept.ID != id || list[0].IdleDays < 14 || list[0].Reason == "" {
		t.Fatalf("Archived() = %+v, %v", list, err)
	}

	got, err := s.Restore(id[:8])
	if err != nil {
		t.Fatal(err)
	}
	if !hasGlobal(s, id) || got.IdleSeconds != 0 {
		t.Errorf("not restored with a fresh idle clock: %+v", got)
	}
	if rest, _ := s.Archived(); len(rest) != 0 {
		t.Errorf("restored percept still listed in the archive: %+v", rest)
	}
	// Reload from disk: the restore was persisted.
	again, err := NewStoreParams(filepath.Dir(s.globalPath), "sess2", Params{})
	if err != nil {
		t.Fatal(err)
	}
	if !hasGlobal(again, id) {
		t.Error("restored percept not persisted")
	}
	// The restored percept survives the next pass (idle was reset)...
	advance(t, s, clock, day)
	if !hasGlobal(s, id) {
		t.Error("restored percept archived again at once")
	}
	if _, err := s.Restore("nomatch"); err == nil {
		t.Error("restoring an unknown id should fail")
	}
}

func TestStale_ReportsRemainingTime(t *testing.T) {
	const day = 24 * time.Hour
	s, clock := archiveStore(t, Params{GlobalArchiveAfterDays: 14})
	id := seedGlobal(t, s, "nu fact being watched")
	advance(t, s, clock, 4*day)
	*clock = clock.Add(day) // a day of running time not yet accounted
	got := s.Stale()
	if len(got) != 1 || got[0].Percept.ID != id {
		t.Fatalf("Stale() = %+v", got)
	}
	if got[0].IdleDays < 4.9 || got[0].IdleDays > 5.1 || got[0].DaysLeft < 8.9 || got[0].DaysLeft > 9.1 || !got[0].AtFloor {
		t.Errorf("idle=%.2f left=%.2f floor=%v, want ~5 / ~9 / true", got[0].IdleDays, got[0].DaysLeft, got[0].AtFloor)
	}
}
