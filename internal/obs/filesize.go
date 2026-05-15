package obs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
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

// extractTimestamp does a best-effort scan for a timestamp-like string in a
// JSON line. Looks for "startTime", "timeUnixNano", or "time" fields.
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
		// quoted string value
		if line[start] == '"' {
			start++
			end := strings.Index(line[start:], `"`)
			if end > 0 {
				return line[start : start+end]
			}
		}
		// numeric nanosecond timestamp
		end := strings.IndexAny(line[start:], ",}")
		if end > 0 {
			ns := line[start : start+end]
			// strip quotes if present
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
	fmt.Fprint(&b, "hint: /otel trim to archive and reset, /otel off to disable for this session")
	return b.String()
}

// Trim renames each signal file to <name>.<date>.jsonl and creates a fresh empty one.
func Trim(otelDir string) error {
	date := time.Now().Format("2006-01-02")
	names := []string{"traces.jsonl", "metrics.jsonl", "logs.jsonl"}
	for _, name := range names {
		src := filepath.Join(otelDir, name)
		if _, err := os.Stat(src); os.IsNotExist(err) {
			continue
		}
		ext := strings.TrimSuffix(name, ".jsonl")
		dst := filepath.Join(otelDir, fmt.Sprintf("%s.%s.jsonl", ext, date))
		if err := os.Rename(src, dst); err != nil {
			return fmt.Errorf("trim %s: %w", name, err)
		}
		// Create fresh empty file.
		f, err := os.OpenFile(src, os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return fmt.Errorf("recreate %s: %w", name, err)
		}
		f.Close()
	}
	return nil
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
	// Try RFC3339 first.
	if t, err := time.Parse(time.RFC3339Nano, ts); err == nil {
		return t.UTC().Format("2006-01-02")
	}
	// Nanosecond unix timestamp.
	var ns int64
	if _, err := fmt.Sscan(ts, &ns); err == nil && ns > 0 {
		return time.Unix(0, ns).UTC().Format("2006-01-02")
	}
	return ""
}
