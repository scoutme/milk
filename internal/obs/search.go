package obs

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SearchResult holds a single matching line from a signal file.
type SearchResult struct {
	File    string
	LineNum int
	Line    string
}

// SearchSignals scans the given signal files for lines containing pattern
// (case-insensitive). signals may contain "logs", "traces", "metrics"; an empty
// slice searches all three. Returns up to maxResults results.
func SearchSignals(otelDir, pattern string, signals []string, maxResults int) []SearchResult {
	if maxResults <= 0 {
		maxResults = 20
	}
	if len(signals) == 0 {
		signals = []string{"logs", "traces", "metrics"}
	}

	lower := strings.ToLower(pattern)
	var out []SearchResult

	for _, sig := range signals {
		if len(out) >= maxResults {
			break
		}
		name := sig + ".jsonl"
		path := filepath.Join(otelDir, name)
		f, err := os.Open(path)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		scanner.Buffer(make([]byte, 256*1024), 256*1024)
		lineNum := 0
		for scanner.Scan() && len(out) < maxResults {
			lineNum++
			line := scanner.Text()
			if line == "" {
				continue
			}
			if strings.Contains(strings.ToLower(line), lower) {
				out = append(out, SearchResult{File: name, LineNum: lineNum, Line: line})
			}
		}
		f.Close()
	}
	return out
}

// FormatSearchResults renders search results as a human-readable string.
func FormatSearchResults(results []SearchResult, pattern string) string {
	if len(results) == 0 {
		return fmt.Sprintf("no matches for %q in signal files", pattern)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d match(es) for %q:\n", len(results), pattern)
	for _, r := range results {
		line := r.Line
		if len(line) > 200 {
			line = line[:197] + "..."
		}
		fmt.Fprintf(&b, "  %s:%d  %s\n", r.File, r.LineNum, line)
	}
	return strings.TrimRight(b.String(), "\n")
}
