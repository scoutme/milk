package memory

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Session-file housekeeping (issue #375, verdict proposal 4).
//
// The memory dir accumulated ~199 session files of which 178 held zero
// percepts — every finished session leaves its file behind forever (only the
// global store and the *current* session's file are ever loaded, so the rest
// is pure disk/hygiene debt, plus it made "assess the store" audits read
// like a much larger corpus than the live 74 percepts).

const (
	// gcSessionMinAge is how old a session file must be before GC may remove
	// it: a freshly created file belongs to a live session that simply has
	// not recorded anything yet (deleting it would only churn a recreate),
	// and the age gap doubles as a race guard — a file that was empty and
	// untouched for an hour is overwhelmingly unlikely to be mid-write.
	gcSessionMinAge = time.Hour
)

// GCSessionFiles removes session memory files in dir that provably hold no
// state — zero percepts, zero engrams, zero edges — except keepSessionID's
// own file (the session being opened must keep its identity even while
// empty). Non-session files (global.json, global-archive.jsonl), unparseable
// files and files younger than gcSessionMinAge are always kept: only
// *provably* empty, *settled* files are deleted.
//
// Race guard: the file is re-stat'ed after being read; if its mtime or size
// changed in between, a writer is active and the file is skipped.
//
// Best-effort by design — called on every store open (NewStoreParams), where
// a failed sweep must never fail the open itself. Returns how many files were
// removed (0, nil when dir does not exist yet).
func GCSessionFiles(dir, keepSessionID string) (int, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, nil
		}
		return 0, err
	}
	keep := keepSessionID + ".json" // keepSessionID "" → ".json", which no real file is named
	removed := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || name == "global.json" || name == keep {
			continue
		}
		info, err := e.Info()
		if err != nil || time.Since(info.ModTime()) < gcSessionMinAge {
			continue
		}
		path := filepath.Join(dir, name)
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var f storeFile
		if json.Unmarshal(data, &f) != nil {
			continue // unparsable → not provably empty; leave it alone
		}
		if len(f.Percepts) > 0 || len(f.Engrams) > 0 || len(f.Edges) > 0 {
			continue
		}
		// Re-stat: if anything changed since the read, treat the file as live.
		after, err := os.Stat(path)
		if err != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			continue
		}
		if os.Remove(path) == nil {
			removed++
		}
	}
	return removed, nil
}
