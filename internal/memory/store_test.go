package memory

import (
	"context"
	"os"
	"testing"
)

func newTestStore(t *testing.T, withSession bool) *Store {
	t.Helper()
	dir := t.TempDir()
	sessID := ""
	if withSession {
		sessID = "test-session"
	}
	s, err := NewStore(dir, sessID)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	return s
}

func TestRecord_GlobalWhenNoSession(t *testing.T) {
	s := newTestStore(t, false)
	id, err := s.Record(context.Background(), "test fact", ProducerUser, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
	if len(s.global.Percepts) != 1 {
		t.Fatalf("expected 1 global percept, got %d", len(s.global.Percepts))
	}
}

func TestRecord_SessionScoped(t *testing.T) {
	s := newTestStore(t, true)
	_, err := s.Record(context.Background(), "session fact", ProducerLocal, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(s.session.Percepts) != 1 {
		t.Fatalf("expected 1 session percept, got %d", len(s.session.Percepts))
	}
	if len(s.global.Percepts) != 0 {
		t.Fatal("session-scoped record should not touch global")
	}
}

func TestRecord_CoreGoesToGlobal(t *testing.T) {
	s := newTestStore(t, true)
	_, err := s.Record(context.Background(), "core fact", ProducerUser, Roles{}, true)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	if len(s.global.Percepts) != 1 {
		t.Fatal("core percept must go to global even with active session")
	}
	if len(s.session.Percepts) != 0 {
		t.Fatal("core percept must not appear in session store")
	}
}

func TestRecordGlobal_AlwaysGlobal(t *testing.T) {
	s := newTestStore(t, true)
	id, err := s.RecordGlobal(context.Background(), "global fact", ProducerUser, Roles{})
	if err != nil {
		t.Fatalf("RecordGlobal: %v", err)
	}
	if id == "" {
		t.Fatal("expected non-empty ID")
	}
	if len(s.global.Percepts) != 1 {
		t.Fatal("RecordGlobal must write to global store")
	}
}

func TestQuery_FindsByKeyword(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "needle in a haystack", ProducerUser, Roles{}, false) //nolint:errcheck
	s.Record(context.Background(), "unrelated fact", ProducerUser, Roles{}, false)       //nolint:errcheck

	results := s.Query(context.Background(), "needle", 0, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Content != "needle in a haystack" {
		t.Errorf("unexpected content: %q", results[0].Content)
	}
}

func TestQuery_EmptyQueryReturnsAll(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "fact one", ProducerUser, Roles{}, false)   //nolint:errcheck
	s.Record(context.Background(), "fact two", ProducerSystem, Roles{}, false) //nolint:errcheck

	results := s.Query(context.Background(), "", 0, 10)
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestQuery_MinConfidenceFilters(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "low weight fact", ProducerSystem, Roles{}, false) //nolint:errcheck
	// ProducerSystem gets W=0.5

	results := s.Query(context.Background(), "", 0.9, 10)
	if len(results) != 0 {
		t.Fatalf("expected 0 results above 0.9 threshold, got %d", len(results))
	}
}

func TestQuery_SortedByWeightDesc(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "system fact", ProducerSystem, Roles{}, false) //nolint:errcheck
	s.Record(context.Background(), "user fact", ProducerUser, Roles{}, false)     //nolint:errcheck
	// ProducerUser W=1.0, ProducerSystem W=0.5

	results := s.Query(context.Background(), "", 0, 10)
	if len(results) < 2 {
		t.Fatal("expected 2 results")
	}
	if results[0].W < results[1].W {
		t.Error("results should be sorted by weight descending")
	}
}

func TestList_ScopeFilter(t *testing.T) {
	s := newTestStore(t, true)
	s.RecordGlobal(context.Background(), "global fact", ProducerUser, Roles{})              //nolint:errcheck
	s.Record(context.Background(), "session fact", ProducerLocal, Roles{}, false)           //nolint:errcheck

	global := s.List(ListOpts{Scope: "global"})
	if len(global) != 1 || global[0].Content != "global fact" {
		t.Errorf("global scope filter failed: %+v", global)
	}

	sess := s.List(ListOpts{Scope: "session"})
	if len(sess) != 1 || sess[0].Content != "session fact" {
		t.Errorf("session scope filter failed: %+v", sess)
	}

	both := s.List(ListOpts{})
	if len(both) != 2 {
		t.Errorf("expected 2 percepts with no scope filter, got %d", len(both))
	}
}

func TestList_PatternFilter(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "alpha fact", ProducerUser, Roles{}, false) //nolint:errcheck
	s.Record(context.Background(), "beta fact", ProducerUser, Roles{}, false)  //nolint:errcheck

	results := s.List(ListOpts{Pattern: "ALPHA"}) // case-insensitive
	if len(results) != 1 || results[0].Content != "alpha fact" {
		t.Errorf("pattern filter failed: %+v", results)
	}
}

func TestList_ProducerFilter(t *testing.T) {
	s := newTestStore(t, false)
	s.Record(context.Background(), "user fact", ProducerUser, Roles{}, false)     //nolint:errcheck
	s.Record(context.Background(), "system fact", ProducerSystem, Roles{}, false) //nolint:errcheck

	results := s.List(ListOpts{Producer: "user"})
	if len(results) != 1 || results[0].Producer != ProducerUser {
		t.Errorf("producer filter failed: %+v", results)
	}
}

func TestPersistence_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	s1, _ := NewStore(dir, "")
	s1.Record(context.Background(), "persistent fact", ProducerUser, Roles{}, false) //nolint:errcheck

	s2, err := NewStore(dir, "")
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	results := s2.Query(context.Background(), "persistent", 0, 10)
	if len(results) != 1 {
		t.Fatalf("expected persisted percept after reload, got %d results", len(results))
	}
}

func TestFlush_WritesFiles(t *testing.T) {
	s := newTestStore(t, true)
	s.Record(context.Background(), "some fact", ProducerUser, Roles{}, false) //nolint:errcheck

	if err := s.Flush(); err != nil {
		t.Fatalf("Flush: %v", err)
	}
	if _, err := os.Stat(s.sessionPath); err != nil {
		t.Errorf("session file not found after Flush: %v", err)
	}
}

func TestInitialWeight(t *testing.T) {
	cases := []struct {
		producer Producer
		core     bool
		want     float64
	}{
		{ProducerUser, false, 1.0},
		{ProducerUser, true, 1.0},
		{ProducerSystem, false, 0.5},
		{ProducerLocal, false, 0.7},
		{ProducerClaude, false, 0.7},
	}
	for _, c := range cases {
		got := initialWeight(c.producer, c.core)
		if got != c.want {
			t.Errorf("initialWeight(%s, core=%v) = %v, want %v", c.producer, c.core, got, c.want)
		}
	}
}
