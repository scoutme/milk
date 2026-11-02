package obs

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// archiveKeep is how many datestamped archives (base.YYYY-MM-DD.ext)
// PruneArchives keeps per file family and directory. /otel trim archives but
// never prunes, and a single pre-rotation archive can hold a 1.5 GB debug log
// — that is exactly how ~/.milk/otel reached 3.7 GB of archives nothing ever
// removed (issue #378). Three keeps a meaningful recent history while making
// the directory self-limiting.
const archiveKeep = 3

// PruneArchives removes old datestamped archives of the known observability
// file families, keeping the newest archiveKeep per (directory, family). It
// scans otelDir plus the directories of the debug log files (their archives
// land next to the live files). Only names matching base.YYYY-MM-DD.ext are
// candidates, so live files (milk.log) and rotation files (milk.1.log,
// ADR-0040) are never touched. Best-effort and stat-only: directories that
// cannot be read are skipped, never failing a startup. Returns how many
// files were removed and the bytes they occupied.
func PruneArchives(otelDir string) (removed int, freed int64) {
	bases := archiveBases()
	dirs := map[string]bool{otelDir: true}
	for _, p := range debugLogPathsFn() {
		dirs[filepath.Dir(p)] = true
	}
	for dir := range dirs {
		r, f := pruneDirArchives(dir, bases)
		removed += r
		freed += f
	}
	return removed, freed
}

// archiveBases is the set of file-family bases whose datestamped archives
// are eligible for pruning: the three OTel signals + milk.log live in
// otelDir, and the debug logs are derived from their live paths.
func archiveBases() map[string]bool {
	bases := map[string]bool{"logs": true, "metrics": true, "traces": true, "milk": true}
	for _, p := range debugLogPathsFn() {
		b := strings.TrimSuffix(filepath.Base(p), filepath.Ext(p))
		if b != "" {
			bases[b] = true
		}
	}
	return bases
}

// pruneDirArchives applies the retention to one directory: groups datestamped
// archives by family, sorts newest-date first, removes everything past
// archiveKeep. Returns (filesRemoved, bytesFreed).
func pruneDirArchives(dir string, bases map[string]bool) (removed int, freed int64) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, 0 // missing/unreadable dir: nothing to prune
	}
	byFamily := map[string][]os.DirEntry{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		base, date, ok := splitArchiveName(name, bases)
		if !ok {
			continue
		}
		_ = date // order comes from the lexicographic name sort below
		byFamily[base] = append(byFamily[base], e)
	}
	for _, group := range byFamily {
		if len(group) <= archiveKeep {
			continue
		}
		// base.YYYY-MM-DD.ext sorts newest-date-first when reversed.
		sort.Slice(group, func(i, j int) bool {
			return group[i].Name() > group[j].Name()
		})
		for _, e := range group[archiveKeep:] {
			info, err := e.Info()
			if err != nil && !os.IsNotExist(err) {
				continue
			}
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				continue // best-effort: a locked file must not fail startup
			}
			removed++
			if info != nil {
				freed += info.Size()
			}
		}
	}
	return removed, freed
}

// splitArchiveName reports whether name is a datestamped archive of one of
// the given families and, if so, returns the family base and the date.
// Shape: <base>.YYYY-MM-DD.<log|jsonl> — the date must be exactly 10 chars
// with dashes at 3 and 6, and the extension must be one archives actually
// use (milk.1.log and friends carry no date and never match).
func splitArchiveName(name string, bases map[string]bool) (base, date string, ok bool) {
	for b := range bases {
		if !strings.HasPrefix(name, b+".") {
			continue
		}
		rest := name[len(b)+1:] // "2026-08-27.log"
		if len(rest) < len("YYYY-MM-DD")+1 {
			continue
		}
		d, ext := rest[:10], rest[10:]
		if !isDate(d) {
			continue
		}
		if ext != ".log" && ext != ".jsonl" {
			continue
		}
		return b, d, true
	}
	return "", "", false
}

func isDate(s string) bool {
	if len(s) != 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < 10; i++ {
		if i == 4 || i == 7 {
			continue
		}
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// FormatBytes renders a byte count for user-facing status lines.
func FormatBytes(b int64) string { return formatBytes(b) }

// PruneSummary renders the startup notice, or "" when nothing was removed.
func PruneSummary(removed int, freed int64) string {
	if removed == 0 {
		return ""
	}
	return fmt.Sprintf("otel: pruned %d old archive(s), %s freed (keeps %d newest per file)",
		removed, FormatBytes(freed), archiveKeep)
}
