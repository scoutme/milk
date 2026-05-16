package obs

import (
	"os"
	"path/filepath"
	"testing"
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
	// No signal files created — should return empty, not error.
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
	results := []SearchResult{
		{File: "logs.jsonl", LineNum: 3, Line: `{"msg":"hello"}`},
	}
	out := FormatSearchResults(results, "hello")
	if out == "" {
		t.Error("expected non-empty formatted output")
	}
	if len(out) < 10 {
		t.Errorf("output seems too short: %q", out)
	}
}
