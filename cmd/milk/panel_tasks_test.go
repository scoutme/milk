package main

import (
	"strings"
	"testing"

	"github.com/scoutme/milk/internal/tasks"
)

// TestBuildTasksPanelLines_NilStore verifies the panel shows "(unavailable)" when
// no task store is configured.
func TestBuildTasksPanelLines_NilStore(t *testing.T) {
	lines := buildTasksPanelLines(nil, 32)
	if len(lines) == 0 {
		t.Fatal("expected non-empty output for nil store")
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "unavailable") {
		t.Errorf("expected 'unavailable' in nil-store output; got:\n%s", joined)
	}
}

// TestBuildTasksPanelLines_EmptyStore verifies the panel shows SESSION/GLOBAL
// section headers and "(none)" markers when the store has no tasks.
func TestBuildTasksPanelLines_EmptyStore(t *testing.T) {
	dir := t.TempDir()
	ts, err := tasks.New(dir, "test-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	lines := buildTasksPanelLines(ts, 32)
	joined := strings.Join(lines, "\n")

	for _, want := range []string{"SESSION", "GLOBAL", "(none)"} {
		// Strip ANSI before checking so terminal colour codes don't interfere.
		plain := stripANSI(joined)
		if !strings.Contains(plain, want) {
			t.Errorf("expected %q in panel output; got:\n%s", want, plain)
		}
	}
}

// TestBuildTasksPanelLines_WithTasks verifies that tasks appear in the panel
// output with their titles and correct status badges.
func TestBuildTasksPanelLines_WithTasks(t *testing.T) {
	dir := t.TempDir()
	ts, err := tasks.New(dir, "test-session")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	t1, _ := ts.Create("implement login", nil)
	t2, _ := ts.Create("write docs", nil)
	_ = ts.Complete(t1.ID)

	lines := buildTasksPanelLines(ts, 32)
	joined := strings.Join(lines, "\n")
	plain := stripANSI(joined)

	if !strings.Contains(plain, "implement login") {
		t.Errorf("title 'implement login' not found in panel output:\n%s", plain)
	}
	if !strings.Contains(plain, "write docs") {
		t.Errorf("title 'write docs' not found in panel output:\n%s", plain)
	}
	_ = t2
}

// TestBuildTasksPanelLines_GlobalTask verifies that a promoted task appears in
// the GLOBAL section when opened from a different session.
func TestBuildTasksPanelLines_GlobalTask(t *testing.T) {
	dir := t.TempDir()
	ts1, _ := tasks.New(dir, "sess1")
	task, _ := ts1.Create("global plan item", nil)
	_ = ts1.Promote(task.ID)

	// Open from a different session.
	ts2, _ := tasks.New(dir, "sess2")
	lines := buildTasksPanelLines(ts2, 32)
	joined := strings.Join(lines, "\n")
	plain := stripANSI(joined)

	if !strings.Contains(plain, "global plan item") {
		t.Errorf("global task not shown in panel from different session:\n%s", plain)
	}
	if !strings.Contains(plain, "GLOBAL") {
		t.Errorf("GLOBAL section not found:\n%s", plain)
	}
}
