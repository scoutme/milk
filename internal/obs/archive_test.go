package obs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestPruneArchives_KeepsNewestThreePerFamily pins the archive-retention
// policy (#378): /otel trim archives but never prunes — that is how
// ~/.milk/otel accreted 3.7 GB. Per (directory, family) the newest three
// datestamped archives stay, everything older goes, and files that merely
// look similar (live files, ADR-0040 rotation files, wrong base or
// extension, malformed dates) are never candidates.
func TestPruneArchives_KeepsNewestThreePerFamily(t *testing.T) {
	dir := t.TempDir()
	write := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// milk.log family: 6 archives → keep 09-13, 09-01, 08-29.
	for _, d := range []string{"2026-07-01", "2026-08-01", "2026-08-27", "2026-08-29", "2026-09-01", "2026-09-13"} {
		write("milk." + d + ".log")
	}
	// logs signal family: 5 archives → keep 09-13, 09-02, 08-27.
	for _, d := range []string{"2026-07-06", "2026-08-01", "2026-08-27", "2026-09-02", "2026-09-13"} {
		write("logs." + d + ".jsonl")
	}
	// Non-candidates: live + rotation + wrong base + wrong ext + bad date.
	for _, name := range []string{
		"milk.log", "milk.1.log", "logs.jsonl",
		"unrelated.2026-01-01.txt", "metrics.2026-01-01.txt", "metrics.2026-01-012.jsonl",
	} {
		write(name)
	}
	// Debug logs archive beside their live file (second directory).
	dbgDir := t.TempDir()
	old := debugLogPathsFn
	t.Cleanup(func() { debugLogPathsFn = old })
	debugLogPathsFn = func() []string { return []string{filepath.Join(dbgDir, "test_cli.log")} }
	for _, d := range []string{"2026-07-01", "2026-08-01", "2026-09-01", "2026-09-13"} {
		if err := os.WriteFile(filepath.Join(dbgDir, "test_cli."+d+".log"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dbgDir, "test_cli.log"), []byte("live"), 0o600); err != nil {
		t.Fatal(err)
	}

	removed, freed := PruneArchives(dir)
	if removed != 6 || freed != 6 { // 3 milk + 2 logs + 1 debug, one byte each
		t.Errorf("removed = %d, freed = %d; want 6, 6", removed, freed)
	}

	// Survivors: newest three per family, everywhere.
	for _, name := range []string{
		"milk.2026-09-13.log", "milk.2026-09-01.log", "milk.2026-08-29.log",
		"logs.2026-09-13.jsonl", "logs.2026-09-02.jsonl", "logs.2026-08-27.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("kept archive missing: %s (%v)", name, err)
		}
	}
	// Removed: the older archives.
	for _, name := range []string{
		"milk.2026-08-27.log", "milk.2026-08-01.log", "milk.2026-07-01.log",
		"logs.2026-08-01.jsonl", "logs.2026-07-06.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Errorf("old archive should be pruned: %s", name)
		}
	}
	// Non-candidates are untouched.
	for _, name := range []string{
		"milk.log", "milk.1.log", "logs.jsonl",
		"unrelated.2026-01-01.txt", "metrics.2026-01-01.txt", "metrics.2026-01-012.jsonl",
	} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("non-candidate was pruned: %s (%v)", name, err)
		}
	}
	// Debug directory: kept 3, pruned 1, live file untouched.
	for _, name := range []string{"test_cli.2026-09-13.log", "test_cli.2026-09-01.log", "test_cli.2026-08-01.log", "test_cli.log"} {
		if _, err := os.Stat(filepath.Join(dbgDir, name)); err != nil {
			t.Errorf("debug-dir file missing: %s (%v)", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dbgDir, "test_cli.2026-07-01.log")); !os.IsNotExist(err) {
		t.Error("old debug archive should be pruned")
	}
}

// TestTrim_PrunesOldArchives pins the /otel trim wiring: archiving without
// pruning was the growth bug, so Trim must sweep old archives right after
// creating the fresh one.
func TestTrim_PrunesOldArchives(t *testing.T) {
	isolateDebugLogPaths(t)
	dir := t.TempDir()
	for _, name := range []string{"logs.jsonl", "milk.log"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("live\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, d := range []string{"2026-07-01", "2026-08-01", "2026-09-01"} {
		if err := os.WriteFile(filepath.Join(dir, "milk."+d+".log"), []byte("old\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Trim(dir); err != nil {
		t.Fatal(err)
	}
	// Trim archived today's files; with today + 3 old milk archives the
	// newest-three rule keeps today, 09-01 and 08-01 — only 07-01 goes.
	for _, name := range []string{"milk.2026-09-01.log", "milk.2026-08-01.log", "milk.log", "logs.jsonl"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s missing after trim: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "milk.2026-07-01.log")); !os.IsNotExist(err) {
		t.Errorf("old archive survived trim: milk.2026-07-01.log")
	}
}

// TestPruneSummary: the startup notice renders only when something went.
func TestPruneSummary(t *testing.T) {
	if s := PruneSummary(0, 0); s != "" {
		t.Errorf("empty prune must not announce: %q", s)
	}
	s := PruneSummary(6, 6)
	if s == "" {
		t.Fatal("prune notice missing")
	}
	for _, want := range []string{"6", "keeps 3"} {
		if !strings.Contains(s, want) {
			t.Errorf("notice %q lacks %q", s, want)
		}
	}
}
