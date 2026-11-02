package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeGCFile writes a session file and optionally backdates it past
// gcSessionMinAge so the age guard lets GC consider it.
func writeGCFile(t *testing.T, dir, name string, data []byte, settled bool) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if settled {
		old := time.Now().Add(-2 * gcSessionMinAge)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return path
}

func gcJSON(t *testing.T, f storeFile) []byte {
	t.Helper()
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func gcExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TestGCSessionFiles_OnlyProvablyEmptySettledSiblings: the #375 housekeeping
// removes exactly what it promises — zero-state session files that have been
// sitting untouched — and keeps everything else (live state, fresh files,
// non-session files, unparsable files, the caller's own session).
func TestGCSessionFiles_OnlyProvablyEmptySettledSiblings(t *testing.T) {
	dir := t.TempDir()

	emptyOld := writeGCFile(t, dir, "dead.json", gcJSON(t, storeFile{}), true)
	fresh := writeGCFile(t, dir, "fresh.json", gcJSON(t, storeFile{}), false)
	withPercepts := writeGCFile(t, dir, "live.json",
		gcJSON(t, storeFile{Percepts: []Percept{{ID: "p1", Content: "still holds state"}}}), true)
	withEdges := writeGCFile(t, dir, "edgy.json",
		gcJSON(t, storeFile{Edges: []Edge{{From: "a", To: "b", Relation: RelationUpdates}}}), true)
	global := writeGCFile(t, dir, "global.json", gcJSON(t, storeFile{}), true)
	broken := writeGCFile(t, dir, "broken.json", []byte("{not json"), true)
	keepMine := writeGCFile(t, dir, "mine.json", gcJSON(t, storeFile{}), true)
	nonJSON := writeGCFile(t, dir, "stray.txt", []byte("x"), true)

	removed, err := GCSessionFiles(dir, "mine")
	if err != nil {
		t.Fatalf("GCSessionFiles: %v", err)
	}
	if removed != 1 {
		t.Errorf("removed = %d, want 1 (only dead.json)", removed)
	}
	if gcExists(emptyOld) {
		t.Error("empty settled session file must be removed")
	}
	for name, path := range map[string]string{
		"fresh file (age guard)":   fresh,
		"file with percepts":       withPercepts,
		"file with only edges":     withEdges,
		"global.json":              global,
		"unparsable file":          broken,
		"keepSessionID's own file": keepMine,
		"non-.json file":           nonJSON,
	} {
		if !gcExists(path) {
			t.Errorf("%s must be kept", name)
		}
	}
}

// TestGCSessionFiles_MissingDirIsNoop: NewStoreParams calls GC before the dir
// may exist on first run — a failed sweep must never fail the store open.
func TestGCSessionFiles_MissingDirIsNoop(t *testing.T) {
	removed, err := GCSessionFiles(filepath.Join(t.TempDir(), "never-created"), "sess")
	if err != nil || removed != 0 {
		t.Errorf("got (%d, %v), want (0, nil)", removed, err)
	}
}

// TestNewStoreParams_GCCallsOnOpen pins the wiring: opening a store sweeps
// empty sibling session files but never its own (even while empty) nor any
// file holding state.
func TestNewStoreParams_GCCallsOnOpen(t *testing.T) {
	dir := t.TempDir()
	dead := writeGCFile(t, dir, "dead.json", gcJSON(t, storeFile{}), true)
	live := writeGCFile(t, dir, "live.json",
		gcJSON(t, storeFile{Percepts: []Percept{{ID: "p1", Content: "keep me"}}}), true)
	mine := writeGCFile(t, dir, "sess.json", gcJSON(t, storeFile{}), true)

	s, err := NewStoreParams(dir, "sess", Params{})
	if err != nil {
		t.Fatalf("NewStoreParams: %v", err)
	}
	_ = s
	if gcExists(dead) {
		t.Error("empty sibling must be swept on open")
	}
	if !gcExists(live) {
		t.Error("sibling with percepts must be kept")
	}
	if !gcExists(mine) {
		t.Error("the opened session's own file must never be swept")
	}
}
