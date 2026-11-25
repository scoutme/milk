package obs

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/scoutme/milk/internal/config"
)

// SignalFileStat describes a single OTel signal file.
type SignalFileStat struct {
	Name    string
	Path    string
	Bytes   int64
	Records int64  // approximate: number of newlines
	Oldest  string // first timestamp found, or ""
	Newest  string // last timestamp found, or ""
}

// FileStats returns stats for all three signal files.
func FileStats(otelDir string) []SignalFileStat {
	names := []string{"traces.jsonl", "metrics.jsonl", "logs.jsonl"}
	out := make([]SignalFileStat, 0, len(names))
	for _, name := range names {
		out = append(out, statFile(otelDir, name))
	}
	return out
}

func statFile(dir, name string) SignalFileStat {
	path := filepath.Join(dir, name)
	return statFileFromPath(path)
}

// statFileFromPath returns a SignalFileStat for a file given its full path.
// The Name field is set to filepath.Base(path).
func statFileFromPath(path string) SignalFileStat {
	name := filepath.Base(path)
	s := SignalFileStat{Name: name, Path: path}

	info, err := os.Stat(path)
	if err != nil {
		return s
	}
	s.Bytes = info.Size()

	f, err := os.Open(path)
	if err != nil {
		return s
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 64*1024)
	var lastLine string
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}
		s.Records++
		if s.Oldest == "" {
			s.Oldest = extractTimestamp(line)
		}
		lastLine = line
	}
	if lastLine != "" {
		s.Newest = extractTimestamp(lastLine)
	}
	return s
}

func extractTimestamp(line string) string {
	for _, key := range []string{`"time":"`, `"startTime":"`, `"observedTimeUnixNano":`, `"timeUnixNano":`} {
		idx := strings.Index(line, key)
		if idx < 0 {
			continue
		}
		start := idx + len(key)
		if start >= len(line) {
			continue
		}
		if line[start] == '"' {
			start++
			end := strings.Index(line[start:], `"`)
			if end > 0 {
				return line[start : start+end]
			}
		}
		end := strings.IndexAny(line[start:], ",}")
		if end > 0 {
			ns := line[start : start+end]
			ns = strings.Trim(ns, `"`)
			return ns
		}
	}
	return ""
}

// FormatStats returns a human-readable summary of otel file stats.
func FormatStats(otelDir string) string {
	stats := FileStats(otelDir)
	var b strings.Builder
	fmt.Fprintf(&b, "observability files (%s)\n", otelDir)

	var totalBytes int64
	for _, s := range stats {
		totalBytes += s.Bytes
		oldest, newest := formatTS(s.Oldest), formatTS(s.Newest)
		fmt.Fprintf(&b, "  %-16s %8s  %6d records", s.Name, formatBytes(s.Bytes), s.Records)
		if oldest != "" {
			fmt.Fprintf(&b, "  %s → %s", oldest, newest)
		}
		fmt.Fprintln(&b)
	}
	fmt.Fprintf(&b, "  %-16s %8s\n", "total", formatBytes(totalBytes))
	switch {
	case sizeTrip.Load():
		fmt.Fprintln(&b, "  state: DISABLED — otel.max_mb exceeded; /otel trim archives the files and reopens the gate")
	case off.Load():
		fmt.Fprintln(&b, "  state: disabled for this session (/otel on to resume)")
	}
	fmt.Fprint(&b, "hint: /otel trim to archive and reset, /otel off to disable for this session")
	return b.String()
}

func Trim(otelDir string) error {
	names := []string{"traces.jsonl", "metrics.jsonl", "logs.jsonl", "milk.log"}
	for _, name := range names {
		src := filepath.Join(otelDir, name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		if err := trimOne(src, name); err != nil {
			return err
		}
	}
	// Also trim the 3 debug log files that live in ~/.milk/ (not otelDir).
	for _, dbgPath := range debugLogPathsFn() {
		if _, err := os.Stat(dbgPath); os.IsNotExist(err) {
			continue
		}
		if err := trimOne(dbgPath, filepath.Base(dbgPath)); err != nil {
			return err
		}
	}
	// Retention (#378): archiving without pruning is what grew
	// ~/.milk/otel to 3.7 GB — sweep old datestamped archives right
	// after archiving so the directory stays self-limiting.
	PruneArchives(otelDir)
	// The size cap that auto-closed the emission gate (#457) tripped on data
	// this trim just archived: reopen the gate. A manual /otel off survives —
	// its own flag (off) stays set.
	sizeTrip.Store(false)
	return nil
}

// errArchiveExists signals the candidate archive path is already taken (a
// second trim the same day); trimOne tries the next candidate name instead of
// overwriting the earlier archive (#457).
var errArchiveExists = errors.New("obs: archive name already exists")

// trimOne archives src's content into a free, atomically claimed archive
// name and truncates the live file in place.
func trimOne(src, name string) error {
	now := time.Now()
	var last error
	for _, dst := range archiveCandidates(src, now) {
		err := archiveAndTruncate(src, dst)
		if errors.Is(err, errArchiveExists) {
			last = err
			continue
		}
		if err != nil {
			return fmt.Errorf("trim %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("trim %s: no free archive name: %w", name, last)
}

// archiveCandidates lists the archive paths to try for src, in order:
// <base>.<date>.<ext> first (the shape splitArchiveName has always
// recognized), then <base>.<date>.<HHMMSS>.<ext> and numbered variants for
// trims that already claimed the earlier names today. All are recognized by
// splitArchiveName, so every candidate still prunes.
func archiveCandidates(src string, now time.Time) []string {
	dir := filepath.Dir(src)
	ext := filepath.Ext(src)
	base := strings.TrimSuffix(filepath.Base(src), ext)
	day := now.Format("2006-01-02")
	clock := now.Format("150405")
	out := make([]string, 0, 101)
	out = append(out, filepath.Join(dir, base+"."+day+ext))
	out = append(out, filepath.Join(dir, base+"."+day+"."+clock+ext))
	for i := 1; i <= 99; i++ {
		out = append(out, filepath.Join(dir, fmt.Sprintf("%s.%s.%s.%d%s", base, day, clock, i, ext)))
	}
	return out
}

// archiveAndTruncate copies src's content into dst and empties src in place.
//
// Three properties matter for concurrent milk sessions (#457):
//
//   - dst is claimed with O_EXCL: two sessions trimming the same day never
//     overwrite each other's archive (errArchiveExists moves Trim to the
//     next candidate name).
//   - The copy streams to EOF and the truncate follows immediately, so the
//     window in which a concurrent append can be lost shrank from "the
//     whole archive write" to one syscall after the final read.
//   - src is truncated in place, never renamed: other sessions hold
//     O_APPEND descriptors on this inode and must keep writing to the live
//     path (TestTrim_TruncatesInPlaceForOpenWriters pins that contract).
//     Cooperating writers (RotatingWriter, a concurrent trim) serialize on
//     the "<file>.lock" sidecar; OTel exporters don't lock, which is why the
//     window is minimized rather than eliminated.
func archiveAndTruncate(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode == 0 {
		mode = 0o600
	}
	if unlock, lerr := config.LockSidecar(src + ".lock"); lerr == nil {
		defer unlock()
	} else {
		Debug("obs: trim lock unavailable", "file", src, "err", lerr)
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		if os.IsExist(err) {
			return errArchiveExists
		}
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst) // a half-written archive must not squat the name
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(dst)
		return err
	}

	_ = in.Close()
	wf, err := os.OpenFile(src, os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	err = wf.Truncate(0)
	if cerr := wf.Close(); err == nil {
		err = cerr
	}
	return err
}

func formatBytes(b int64) string {
	switch {
	case b >= 1024*1024:
		return fmt.Sprintf("%.1f MB", float64(b)/1024/1024)
	case b >= 1024:
		return fmt.Sprintf("%.1f KB", float64(b)/1024)
	default:
		return fmt.Sprintf("%d B", b)
	}
}

func formatTS(ts string) string {
	if ts == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	var ns int64
	if _, err := fmt.Sscan(ts, &ns); err == nil && ns > 0 {
		return time.Unix(0, ns).UTC().Format("2006-01-02")
	}
	return ""
}
