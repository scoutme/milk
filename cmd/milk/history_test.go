package main

import (
	"os"
	"path/filepath"
	"testing"
)

// --- appendDeduped ---

func TestAppendDeduped_AddsEntry(t *testing.T) {
	h := appendDeduped(nil, "foo", 10)
	if len(h) != 1 || h[0] != "foo" {
		t.Errorf("expected [foo], got %v", h)
	}
}

func TestAppendDeduped_SkipsDuplicate(t *testing.T) {
	h := []string{"foo"}
	h = appendDeduped(h, "foo", 10)
	if len(h) != 1 {
		t.Errorf("duplicate should not be appended, got %v", h)
	}
}

func TestAppendDeduped_AllowsNonConsecutiveDuplicate(t *testing.T) {
	h := []string{"foo", "bar"}
	h = appendDeduped(h, "foo", 10)
	if len(h) != 3 || h[2] != "foo" {
		t.Errorf("non-consecutive duplicate should be appended, got %v", h)
	}
}

func TestAppendDeduped_CapsAtMax(t *testing.T) {
	var h []string
	for i := range 5 {
		h = appendDeduped(h, string(rune('a'+i)), 3)
	}
	if len(h) != 3 {
		t.Errorf("expected cap at 3, got %d", len(h))
	}
	if h[0] != "c" || h[2] != "e" {
		t.Errorf("expected [c d e], got %v", h)
	}
}

// --- searchBack ---

func TestSearchBack_FindsLastMatch(t *testing.T) {
	h := []string{"hello", "world", "hello again"}
	idx := searchBack(h, "hello", -1)
	if idx != 2 {
		t.Errorf("expected index 2, got %d", idx)
	}
}

func TestSearchBack_FromIndex(t *testing.T) {
	h := []string{"hello", "world", "hello again"}
	idx := searchBack(h, "hello", 2) // start exclusive from 2, search [0..1]
	if idx != 0 {
		t.Errorf("expected index 0, got %d", idx)
	}
}

func TestSearchBack_NoMatch(t *testing.T) {
	h := []string{"foo", "bar"}
	idx := searchBack(h, "baz", -1)
	if idx != -1 {
		t.Errorf("expected -1 for no match, got %d", idx)
	}
}

func TestSearchBack_EmptyHistory(t *testing.T) {
	if idx := searchBack(nil, "x", -1); idx != -1 {
		t.Errorf("expected -1 for empty history, got %d", idx)
	}
}

// --- searchForward ---

func TestSearchForward_FindsFirstMatch(t *testing.T) {
	h := []string{"hello", "world", "hello again"}
	idx := searchForward(h, "hello", -1)
	if idx != 0 {
		t.Errorf("expected index 0, got %d", idx)
	}
}

func TestSearchForward_FromIndex(t *testing.T) {
	h := []string{"hello", "world", "hello again"}
	idx := searchForward(h, "hello", 0) // start exclusive from 0, search [1..2]
	if idx != 2 {
		t.Errorf("expected index 2, got %d", idx)
	}
}

func TestSearchForward_NoMatch(t *testing.T) {
	h := []string{"foo", "bar"}
	idx := searchForward(h, "baz", -1)
	if idx != -1 {
		t.Errorf("expected -1 for no match, got %d", idx)
	}
}

func TestSearchForward_EmptyHistory(t *testing.T) {
	if idx := searchForward(nil, "x", -1); idx != -1 {
		t.Errorf("expected -1 for empty history, got %d", idx)
	}
}

// --- history file round-trip ---

func TestHistoryRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	lines := []string{"first", "second", "third"}
	writeHistoryFile(path, lines)
	got := readHistoryFile(path)
	if len(got) != len(lines) {
		t.Fatalf("expected %d lines, got %d", len(lines), len(got))
	}
	for i, l := range lines {
		if got[i] != l {
			t.Errorf("line %d: expected %q, got %q", i, l, got[i])
		}
	}
}

func TestHistoryRoundTrip_MultiLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	entries := []string{
		"single line",
		"line one\nline two\nline three",
		"another entry",
	}
	writeHistoryFile(path, entries)
	got := readHistoryFile(path)
	if len(got) != len(entries) {
		t.Fatalf("expected %d entries, got %d: %q", len(entries), len(got), got)
	}
	for i, want := range entries {
		if got[i] != want {
			t.Errorf("entry %d: expected %q, got %q", i, want, got[i])
		}
	}
}

func TestHistoryRoundTrip_Missing(t *testing.T) {
	got := readHistoryFile("/nonexistent/path/history")
	if got != nil {
		t.Errorf("expected nil for missing file, got %v", got)
	}
}

func TestHistoryRoundTrip_CapsAtMax(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "history")
	lines := make([]string, maxPersistedHistory+10)
	for i := range lines {
		lines[i] = string(rune('a' + i%26))
	}
	writeHistoryFile(path, lines)
	got := readHistoryFile(path)
	if len(got) != maxPersistedHistory {
		t.Errorf("expected %d lines after cap, got %d", maxPersistedHistory, len(got))
	}
}

func TestSessionHistoryPath_CreatesDirs(t *testing.T) {
	// Temporarily override HOME so sessionHistoryPath creates under t.TempDir()
	orig := os.Getenv("HOME")
	defer os.Setenv("HOME", orig)
	os.Setenv("HOME", t.TempDir())

	path, err := sessionHistoryPath("test-session-id")
	if err != nil {
		t.Fatalf("sessionHistoryPath: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(path)); err != nil {
		t.Errorf("sessions dir not created: %v", err)
	}
}
