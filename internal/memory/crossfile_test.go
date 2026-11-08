package memory

// Cross-session store resolution: forget_family lookups (forget_memory,
// /forget, /memory show, record_memory's related_to) must reach percepts that
// live in another session's file, not just global + the loaded session slice.
// Before this, a cold session's percepts were unreachable from the tools —
// only resuming that session or hand-editing the file could remove them (the
// exact failure seen in the field: forget by full ID returned "not found" for
// a percept that sat in a sibling file all along).

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func recordIn(t *testing.T, s *Store, content string) string {
	t.Helper()
	out, err := s.Record(context.Background(), content, ProducerUser, ConsumerAll, Roles{}, false)
	if err != nil {
		t.Fatalf("Record: %v", err)
	}
	return out.ID
}

func mustReadStoreFile(t *testing.T, path string) storeFile {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return f
}

func perceptOnDisk(t *testing.T, path, id string) bool {
	t.Helper()
	for _, p := range mustReadStoreFile(t, path).Percepts {
		if p.ID == id {
			return true
		}
	}
	return false
}

// A percept recorded in session A is forgotten from session B's store: the
// id-prefix lookup crosses files, the delete rewrites A's file (atomically),
// and a fresh open of A no longer loads it. B's own slice never saw it.
func TestForget_ResolvesAcrossSiblingSessionFiles(t *testing.T) {
	dir := t.TempDir()
	a, err := NewStore(dir, "sess-a")
	if err != nil {
		t.Fatalf("NewStore a: %v", err)
	}
	id := recordIn(t, a, "sibling-only fact: the frostwire-99 zither clause")
	aPath := filepath.Join(dir, "sess-a.json")
	if !perceptOnDisk(t, aPath, id) {
		t.Fatal("precondition: percept missing from sess-a.json")
	}

	b, err := NewStore(dir, "sess-b")
	if err != nil {
		t.Fatalf("NewStore b: %v", err)
	}
	// The lookup crosses files: a full-id prefix resolves even though the
	// percept is in neither b's global nor b's session slice.
	res := DispatchForgetMemory(context.Background(), b, `{"id":"`+id[:8]+`"}`)
	if !strings.Contains(res, "deleted") || strings.Contains(res, "not found") {
		t.Fatalf("forget across sessions = %q, want deleted", res)
	}
	if perceptOnDisk(t, aPath, id) {
		t.Error("sibling file still holds the percept after delete")
	}
	if len(mustReadStoreFile(t, aPath).Percepts) != 0 {
		t.Error("sibling file should be empty after deleting its only percept")
	}

	// A fresh open of A (the realistic next lifecycle step) no longer loads it.
	a2, err := NewStore(dir, "sess-a")
	if err != nil {
		t.Fatalf("reopen A: %v", err)
	}
	if got := a2.List(ListOpts{Pattern: "frostwire-99"}); len(got) != 0 {
		t.Errorf("reopened A still lists the deleted percept: %v", got)
	}
	// B's own files are untouched: B never wrote anything here.
	if got := b.List(ListOpts{Pattern: "frostwire-99"}); len(got) != 0 {
		t.Errorf("B's loaded pool should not contain the sibling percept, got %v", got)
	}
}

// pattern spans the union: one match in B's loaded slice + one in a sibling
// file → both deleted, count reported honestly.
func TestForget_PatternSpansSessionFiles(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewStore(dir, "sess-a") //nolint:errcheck
	idA := recordIn(t, a, "crossfire quip alpha: zorp")
	if idA == "" {
		t.Fatal("idA empty")
	}
	b, _ := NewStore(dir, "sess-b") //nolint:errcheck
	idB := recordIn(t, b, "crossfire quip beta: meld")

	res := DispatchForgetMemory(context.Background(), b, `{"pattern":"crossfire quip"}`)
	if !strings.Contains(res, "deleted 2 percepts") {
		t.Fatalf("pattern across files = %q, want deleted 2 percepts", res)
	}
	if perceptOnDisk(t, filepath.Join(dir, "sess-a.json"), idA) {
		t.Error("sibling match not deleted")
	}
	if perceptOnDisk(t, filepath.Join(dir, "sess-b.json"), idB) {
		t.Error("loaded match not deleted")
	}
}

// A description matching one percept in a sibling file and one in the loaded
// pool is ambiguous — the union counts before resolving, so nothing is
// deleted (all-or-nothing guarantee extends across files).
func TestForget_AmbiguityAcrossFiles_IsAtomic(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewStore(dir, "sess-a") //nolint:errcheck
	idA := recordIn(t, a, "grumblewort quip: only one should match")
	b, _ := NewStore(dir, "sess-b") //nolint:errcheck
	idB := recordIn(t, b, "grumblewort quip in a different file")

	res := DispatchForgetMemory(context.Background(), b, `{"id":"grumblewort quip"}`)
	if !strings.Contains(res, "matches 2 percepts") {
		t.Fatalf("cross-file ambiguity = %q, want 'matches 2 percepts'", res)
	}
	if !perceptOnDisk(t, filepath.Join(dir, "sess-a.json"), idA) {
		t.Error("ambiguous call must not delete the sibling match")
	}
	if !perceptOnDisk(t, filepath.Join(dir, "sess-b.json"), idB) {
		t.Error("ambiguous call must not delete the loaded match")
	}
}

// ListOpts.AllSessions is opt-in: the default pool (list_memory, recall
// injection, the panel) stays global + this session even with siblings on
// disk; scope "global" + AllSessions never pulls session files in.
func TestList_AllSessionsIsOptIn(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewStore(dir, "sess-a") //nolint:errcheck
	recordIn(t, a, "hidden-from-default-list frobnicate")
	b, _ := NewStore(dir, "sess-b") //nolint:errcheck

	if got := b.List(ListOpts{Pattern: "frobnicate"}); len(got) != 0 {
		t.Errorf("default List leaked a sibling percept: %v", got)
	}
	if got := b.List(ListOpts{Pattern: "frobnicate", AllSessions: true}); len(got) != 1 {
		t.Errorf("AllSessions List = %d matches, want 1", len(got))
	}
	if got := b.List(ListOpts{Scope: "global", Pattern: "frobnicate", AllSessions: true}); len(got) != 0 {
		t.Errorf("global scope + AllSessions must exclude session files, got %v", got)
	}
	// siblingSessionPaths excludes global.json and this store's own file.
	for _, p := range b.siblingSessionPaths() {
		base := filepath.Base(p)
		if base == "global.json" || base == "sess-b.json" {
			t.Errorf("siblingSessionPaths leaked %s", base)
		}
	}
}

// A corrupt sibling file must not break lookups: unreadable files are
// skipped, and deletion still reaches the target in a readable sibling.
func TestCrossSession_SkipsBrokenSiblingFile(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewStore(dir, "sess-a") //nolint:errcheck
	idA := recordIn(t, a, "readable sibling fact: wilted-77 goose")
	broken := filepath.Join(dir, "broken.json")
	if err := os.WriteFile(broken, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	b, _ := NewStore(dir, "sess-b") //nolint:errcheck

	res := DispatchForgetMemory(context.Background(), b, `{"id":"`+idA[:8]+`"}`)
	if !strings.Contains(res, "deleted") {
		t.Fatalf("forget with a broken sibling present = %q, want deleted", res)
	}
	if perceptOnDisk(t, filepath.Join(dir, "sess-a.json"), idA) {
		t.Error("readable sibling not deleted")
	}
	if data, err := os.ReadFile(broken); err != nil || string(data) != "{not json" {
		t.Errorf("broken file was touched (err=%v data=%q)", err, data)
	}
}

// record_memory's relation + related_to resolve the target across files:
// the call succeeds (no "related_to failed") and the edge lands in the
// recording session's file — provenance across sessions, with events
// staying file-local per operations.md.
func TestRecordRelatedTo_CrossSessionTarget(t *testing.T) {
	dir := t.TempDir()
	a, _ := NewStore(dir, "sess-a") //nolint:errcheck
	idA := recordIn(t, a, "ancient cross-file claim: meridian the gopher")
	b, _ := NewStore(dir, "sess-b") //nolint:errcheck

	res := DispatchRecordMemory(context.Background(), b,
		`{"content":"fresh corroboration of the gopher claim","producer":"user",`+
			`"relation":"updates","related_to":"`+idA[:8]+`"}`)
	if strings.Contains(res, "related_to failed") || !strings.Contains(res, "edge recorded") {
		t.Fatalf("cross-session related_to = %q, want edge recorded without related_to failure", res)
	}
	var found bool
	for _, e := range mustReadStoreFile(t, filepath.Join(dir, "sess-b.json")).Edges {
		if e.To == idA && e.Relation == RelationUpdates {
			found = true
		}
	}
	if !found {
		t.Errorf("edge to the sibling percept not stored in sess-b.json (want To=%s)", idA[:8])
	}
}
