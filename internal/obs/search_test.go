package obs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/config"
)

func writeSignalFile(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestSearchSignals_FindsMatch(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "logs.jsonl", `{"msg":"hello world"}
{"msg":"nothing here"}
`)
	results := SearchSignals(dir, "hello", []string{"logs"}, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].File != "logs.jsonl" {
		t.Errorf("unexpected file: %q", results[0].File)
	}
	if results[0].LineNum != 1 {
		t.Errorf("expected line 1, got %d", results[0].LineNum)
	}
}

func TestSearchSignals_CaseInsensitive(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "logs.jsonl", `{"msg":"HELLO"}`)
	results := SearchSignals(dir, "hello", []string{"logs"}, 10)
	if len(results) != 1 {
		t.Fatalf("expected 1 match for case-insensitive search, got %d", len(results))
	}
}

func TestSearchSignals_AllSignals(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "logs.jsonl", `{"a":"needle"}`)
	writeSignalFile(t, dir, "traces.jsonl", `{"b":"needle"}`)
	writeSignalFile(t, dir, "metrics.jsonl", `{"c":"needle"}`)
	results := SearchSignals(dir, "needle", nil, 10)
	if len(results) != 3 {
		t.Errorf("expected 3 matches across all signal files, got %d", len(results))
	}
}

func TestSearchSignals_MaxResults(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "logs.jsonl", "match1\nmatch2\nmatch3\nmatch4\nmatch5\n")
	results := SearchSignals(dir, "match", []string{"logs"}, 3)
	if len(results) != 3 {
		t.Errorf("expected max 3 results, got %d", len(results))
	}
}

func TestSearchSignals_MissingFile(t *testing.T) {
	dir := t.TempDir()
	results := SearchSignals(dir, "anything", nil, 10)
	if len(results) != 0 {
		t.Errorf("expected 0 results for missing files, got %d", len(results))
	}
}

func TestSearchSignals_NoMatch(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "logs.jsonl", `{"msg":"nothing relevant"}`)
	results := SearchSignals(dir, "xyzzy", []string{"logs"}, 10)
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestFormatSearchResults_Empty(t *testing.T) {
	out := FormatSearchResults(nil, "foo")
	if out == "" {
		t.Error("expected non-empty message for empty results")
	}
	if out == "foo" {
		t.Error("expected a human-readable no-match message")
	}
}

func TestFormatSearchResults_WithResults(t *testing.T) {
	results := []SearchResult{{File: "logs.jsonl", LineNum: 3, Line: `{"msg":"hello"}`}}
	out := FormatSearchResults(results, "hello")
	if out == "" {
		t.Error("expected non-empty formatted output")
	}
	if len(out) < 10 {
		t.Errorf("output seems too short: %q", out)
	}
}

func TestFormatMetrics_EmptyFile(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "metrics.jsonl", "")
	out := FormatMetrics(dir)
	if out == "" {
		t.Fatal("expected message for empty metrics file")
	}
}

func TestFormatMetrics_ShowsLatestValues(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "metrics.jsonl", `{"ScopeMetrics":[{"Metrics":[{"Name":"milk.turns.total","Data":{"DataPoints":[{"Value":1,"Attributes":[{"Key":"target","Value":{"Value":"local"}}],"Time":"2024-01-01T00:00:00Z"}]}}]}]}
{"ScopeMetrics":[{"Metrics":[{"Name":"milk.turns.total","Data":{"DataPoints":[{"Value":2,"Attributes":[{"Key":"target","Value":{"Value":"local"}}],"Time":"2024-01-01T00:01:00Z"}]}}]}]}
`)
	out := FormatMetrics(dir)
	if !strings.Contains(out, "milk.turns.total") || !strings.Contains(out, "2") || !strings.Contains(out, "@ 2024-01-01T00:01:00Z") {
		t.Fatalf("expected latest metric value and timestamp in output, got %q", out)
	}
}

func TestFileStatsAndTrim(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "logs.jsonl", `{"time":"2024-01-01T00:00:00Z"}
{"time":"2024-01-02T00:00:00Z"}
`)
	stats := FileStats(dir)
	if len(stats) != 3 {
		t.Fatalf("expected 3 stats entries, got %d", len(stats))
	}
	if stats[2].Records != 2 {
		t.Fatalf("expected 2 records, got %d", stats[2].Records)
	}
	if stats[2].Oldest == "" || stats[2].Newest == "" {
		t.Fatalf("expected timestamps, got %+v", stats[2])
	}
	if err := Trim(dir); err != nil {
		t.Fatalf("trim failed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "logs.jsonl")); err != nil {
		t.Fatalf("expected recreated logs file: %v", err)
	}
	matches, _ := filepath.Glob(filepath.Join(dir, "logs.*.jsonl"))
	if len(matches) == 0 {
		t.Fatal("expected archived logs file")
	}
}

func TestCheckFileSizes(t *testing.T) {
	dir := t.TempDir()
	writeSignalFile(t, dir, "metrics.jsonl", strings.Repeat("x", 1024*1024))
	warn, exceeded := CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 2}, dir)
	if warn == "" || exceeded {
		t.Fatalf("expected warning only, got warn=%q exceeded=%v", warn, exceeded)
	}
	warn, exceeded = CheckFileSizes(config.OtelConfig{Enabled: true, WarnMB: 1, MaxMB: 1}, dir)
	if !exceeded {
		t.Fatal("expected max cap to disable otel")
	}
	_ = warn
}
